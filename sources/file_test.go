package sources

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

type shortPatternReader struct {
	reader *bytes.Reader
	sizes  []int
	next   int
}

func (r *shortPatternReader) Read(p []byte) (int, error) {
	limit := r.sizes[r.next%len(r.sizes)]
	r.next++
	if len(p) > limit {
		p = p[:limit]
	}
	return r.reader.Read(p)
}

type testFragmentIdentity struct {
	path      string
	symlink   string
	line      int
	rawLen    int
	rawSHA256 [sha256.Size]byte
}

type testFragmentCapture struct {
	identity testFragmentIdentity
	raw      string
}

func fragmentCapturesForReader(t *testing.T, content io.Reader) []testFragmentCapture {
	t.Helper()
	file := File{
		Path:       "inner.txt",
		Symlink:    "link.txt",
		outerPaths: []string{"fixture.tar.zst"},
	}
	reader := bufio.NewReaderSize(content, 16)
	var captures []testFragmentCapture
	err := file.fileFragments(t.Context(), reader, true, func(fragment Fragment, err error) error {
		if err != nil {
			return err
		}
		if !bytes.Equal(fragment.Bytes, []byte(fragment.Raw)) {
			t.Fatalf("fragment Bytes differ from Raw at line %d", fragment.StartLine)
		}
		captures = append(captures, testFragmentCapture{
			identity: testFragmentIdentity{
				path:      fragment.Attr(AttrPath),
				symlink:   fragment.Attr(AttrFSSymlink),
				line:      fragment.StartLine,
				rawLen:    len(fragment.Raw),
				rawSHA256: sha256.Sum256([]byte(fragment.Raw)),
			},
			raw: fragment.Raw,
		})
		return nil
	})
	if err != nil {
		t.Fatalf("fileFragments() error = %v", err)
	}
	return captures
}

func testFramingChunk(prefix string, fill byte) []byte {
	chunk := bytes.Repeat([]byte{fill}, defaultBufferSize)
	copy(chunk, prefix)
	chunk[len(chunk)-2] = '\n'
	chunk[len(chunk)-1] = '\n'
	return chunk
}

func TestFileFragmentsIndependentOfShortReadBoundaries(t *testing.T) {
	chunks := [][]byte{
		testFramingChunk("first:", 'a'),
		testFramingChunk("second:", 'b'),
		[]byte("tail\n\n"),
	}
	content := bytes.Join(chunks, nil)
	want := []testFragmentIdentity{
		{path: "fixture.tar.zst!inner.txt", symlink: "link.txt", line: 1, rawLen: len(chunks[0]), rawSHA256: sha256.Sum256(chunks[0])},
		{path: "fixture.tar.zst!inner.txt", symlink: "link.txt", line: 3, rawLen: len(chunks[1]), rawSHA256: sha256.Sum256(chunks[1])},
		{path: "fixture.tar.zst!inner.txt", symlink: "link.txt", line: 5, rawLen: len(chunks[2]), rawSHA256: sha256.Sum256(chunks[2])},
	}
	tests := []struct {
		name      string
		newReader func() io.Reader
	}{
		{name: "full reads", newReader: func() io.Reader { return bytes.NewReader(content) }},
		{name: "small short reads", newReader: func() io.Reader {
			return &shortPatternReader{reader: bytes.NewReader(content), sizes: []int{17, 31, 127}}
		}},
		{name: "large short reads", newReader: func() io.Reader {
			return &shortPatternReader{reader: bytes.NewReader(content), sizes: []int{1_500, 2_003, 509}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fragmentCapturesForReader(t, tt.newReader())
			if len(got) != len(want) {
				t.Fatalf("fragment count = %d, want %d", len(got), len(want))
			}
			var complete strings.Builder
			complete.Grow(len(content))
			for i := range want {
				if got[i].identity != want[i] {
					t.Fatalf("fragment %d identity = %+v, want %+v", i, got[i].identity, want[i])
				}
				complete.WriteString(got[i].raw)
			}
			if complete.String() != string(content) {
				t.Fatal("fragments do not reconstruct the complete input")
			}
		})
	}
}

type cancelAfterShortReader struct {
	cancel   context.CancelFunc
	afterErr error
	reads    int
}

func (r *cancelAfterShortReader) Read(p []byte) (int, error) {
	r.reads++
	if r.reads == 1 {
		n := copy(p, "x\n\n")
		r.cancel()
		return n, nil
	}
	return 0, r.afterErr
}

func TestFileFragmentsChecksCancellationBetweenShortReads(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	afterErr := errors.New("read after cancellation")
	reader := &cancelAfterShortReader{cancel: cancel, afterErr: afterErr}
	file := File{Path: "fixture.txt", Buffer: make([]byte, defaultBufferSize)}
	yielded := 0
	err := file.fileFragments(ctx, bufio.NewReaderSize(reader, 16), false, func(Fragment, error) error {
		yielded++
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("fileFragments() error = %v, want context.Canceled", err)
	}
	if reader.reads != 1 {
		t.Fatalf("reader calls after cancellation = %d, want 1 total call", reader.reads)
	}
	if yielded != 0 {
		t.Fatalf("fragments yielded after cancellation = %d, want 0", yielded)
	}
}

type dataThenErrorReader struct {
	data []byte
	err  error
	done bool
}

func (r *dataThenErrorReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(p, r.data), r.err
}

func TestFileFragmentsPreservesReadErrorsWithData(t *testing.T) {
	fullErr := errors.New("full-buffer read error")
	tests := []struct {
		name string
		data []byte
		err  error
	}{
		{name: "full buffer and error", data: testFramingChunk("full-error:", 'f'), err: fullErr},
		{name: "partial and explicit unexpected EOF", data: []byte("partial\n\n"), err: io.ErrUnexpectedEOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := File{Path: "fixture.txt", Buffer: make([]byte, defaultBufferSize)}
			yielded := 0
			var yieldedErr error
			var yieldedRaw string
			err := file.fileFragments(
				t.Context(),
				bufio.NewReaderSize(&dataThenErrorReader{data: tt.data, err: tt.err}, 16),
				false,
				func(fragment Fragment, err error) error {
					yielded++
					yieldedErr = err
					yieldedRaw = fragment.Raw
					return err
				},
			)
			if !errors.Is(err, tt.err) {
				t.Fatalf("fileFragments() error = %v, want %v", err, tt.err)
			}
			if yielded != 1 {
				t.Fatalf("yield count = %d, want 1", yielded)
			}
			if !errors.Is(yieldedErr, tt.err) {
				t.Fatalf("yield error = %v, want %v", yieldedErr, tt.err)
			}
			if yieldedRaw != string(tt.data) {
				t.Fatalf("yielded raw length = %d, want %d", len(yieldedRaw), len(tt.data))
			}
		})
	}
}

type noProgressReader struct {
	reads int
}

func (r *noProgressReader) Read([]byte) (int, error) {
	r.reads++
	return 0, nil
}

func TestReadFragmentChunkStopsAfterNoProgress(t *testing.T) {
	reader := &noProgressReader{}
	n, err := readFragmentChunk(t.Context(), reader, make([]byte, 1))
	if !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("readFragmentChunk() error = %v, want io.ErrNoProgress", err)
	}
	if n != 0 {
		t.Fatalf("readFragmentChunk() bytes = %d, want 0", n)
	}
	if reader.reads != maxConsecutiveFragmentEmptyReads {
		t.Fatalf("reader calls = %d, want %d", reader.reads, maxConsecutiveFragmentEmptyReads)
	}
}

func TestAppendOuterPathDoesNotAliasInput(t *testing.T) {
	storage := []string{"outer.tar", "middle.tar", "sentinel", "sentinel"}
	outer := storage[:2]
	extended := appendOuterPath(outer, "inner.tar")
	if storage[2] != "sentinel" {
		t.Fatalf("appendOuterPath mutated caller storage: %q", storage[2])
	}
	if got, want := strings.Join(extended, InnerPathSeparator), "outer.tar!middle.tar!inner.tar"; got != want {
		t.Fatalf("appendOuterPath result = %q, want %q", got, want)
	}
	outer[0] = "changed-outer.tar"
	if extended[0] != "outer.tar" {
		t.Fatalf("appendOuterPath result aliases input: %q", extended[0])
	}
	extended[1] = "changed-middle.tar"
	if outer[1] != "middle.tar" {
		t.Fatalf("appendOuterPath input aliases result: %q", outer[1])
	}
}

func TestFullPathDoesNotMutateOuterPaths(t *testing.T) {
	storage := []string{"outer.tar", "middle.tar", "sentinel", "sentinel"}
	file := File{Path: "inner.txt", outerPaths: storage[:2]}
	if got, want := file.FullPath(), "outer.tar!middle.tar!inner.txt"; got != want {
		t.Fatalf("FullPath() = %q, want %q", got, want)
	}
	if storage[2] != "sentinel" {
		t.Fatalf("FullPath mutated outer-path storage: %q", storage[2])
	}
}

const (
	testArchiveCount      = 4
	testArchiveEntryCount = 4
)

func testArchiveEntryName(entry int) string {
	return fmt.Sprintf("entry-%02d.txt", entry)
}

func testArchiveEntryChunks(seed byte, entry int) [][]byte {
	return [][]byte{
		testFramingChunk(fmt.Sprintf("%c-%02d-first:", seed, entry), byte('a'+entry)),
		testFramingChunk(fmt.Sprintf("%c-%02d-second:", seed, entry), byte('k'+entry)),
		[]byte(fmt.Sprintf("%c-%02d-tail\n\n", seed, entry)),
	}
}

func writeTestZstdTar(t *testing.T, path string, seed byte) {
	t.Helper()
	archive, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	encoder, err := zstd.NewWriter(archive, zstd.WithEncoderConcurrency(1))
	if err != nil {
		archive.Close()
		t.Fatal(err)
	}
	tarWriter := tar.NewWriter(encoder)
	for entry := range testArchiveEntryCount {
		content := bytes.Join(testArchiveEntryChunks(seed, entry), nil)
		header := &tar.Header{
			Name: testArchiveEntryName(entry),
			Mode: 0o444,
			Size: int64(len(content)),
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(tarWriter, bytes.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
}

func expectedTestArchiveFragmentDigest(root string) FileScanDigestSnapshot {
	metrics := NewFileScanMetrics()
	for archive := range testArchiveCount {
		seed := byte('a' + archive)
		archivePath := filepath.Join(root, fmt.Sprintf("fixture-%02d.tar.zst", archive))
		for entry := range testArchiveEntryCount {
			path := filepath.ToSlash(archivePath) + InnerPathSeparator + filepath.ToSlash(testArchiveEntryName(entry))
			startLine := 1
			for _, raw := range testArchiveEntryChunks(seed, entry) {
				metrics.RecordFragment(root, Fragment{
					Raw:       string(raw),
					StartLine: startLine,
					Attributes: map[string]string{
						AttrPath: path,
					},
				})
				startLine += bytes.Count(raw, []byte{'\n'})
			}
		}
	}
	return metrics.Snapshot().Digests.Fragments
}

func TestFileScanZstdTarFragmentIdentityConcurrent(t *testing.T) {
	root := t.TempDir()
	for archive := range testArchiveCount {
		writeTestZstdTar(t, filepath.Join(root, fmt.Sprintf("fixture-%02d.tar.zst", archive)), byte('a'+archive))
	}

	want := expectedTestArchiveFragmentDigest(root)
	wantCount := uint64(testArchiveCount * testArchiveEntryCount * 3)
	if want.Count != wantCount {
		t.Fatalf("anchored fragment count = %d, want %d", want.Count, wantCount)
	}
	for run := range 5 {
		metrics := NewFileScanMetrics()
		files := Files{
			Path:            root,
			MaxArchiveDepth: 3,
			FileScan: &FileScanConfig{
				Backend:        FileScanBackendBaseline,
				Namespace:      FileScanNamespaceParallel,
				ActiveFiles:    4,
				Walkers:        4,
				InlineDetector: true,
				Metrics:        metrics,
			},
		}
		if err := files.Fragments(t.Context(), func(Fragment, error) error { return nil }); err != nil {
			t.Fatalf("run %d: Fragments() error = %v", run, err)
		}
		got := metrics.Snapshot().Digests.Fragments
		if got != want {
			t.Fatalf("run %d fragment identity = %+v, want %+v", run, got, want)
		}
	}
}
