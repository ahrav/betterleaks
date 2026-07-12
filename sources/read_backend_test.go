package sources

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestPrefetchReaderReadAndPreadMatchFile(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{FileScanReadModeRead, FileScanReadModePread} {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			for _, size := range []int{1, 17, 4095, 4096, 4097, 100_003, 530_123} {
				want := fileScanTestBytes(size)
				path := filepath.Join(t.TempDir(), "content.txt")
				if err := os.WriteFile(path, want, 0o600); err != nil {
					t.Fatal(err)
				}
				file, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				reader := newPrefetchReader(file, 13_117, mode)
				got, readErr := io.ReadAll(reader)
				closeErr := reader.Close()
				if err := errors.Join(readErr, closeErr); err != nil {
					t.Fatalf("size %d: %v", size, err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("size %d: content mismatch", size)
				}
			}
		})
	}
}

func TestPrefetchReaderPreservesDelayedError(t *testing.T) {
	wantErr := errors.New("injected read failure")
	file := &fileScanScriptedReadAtCloser{
		steps: []fileScanReadStep{
			{data: []byte("abc"), err: nil},
			{data: []byte("def"), err: wantErr},
		},
	}
	reader := &prefetchReader{file: file, buf: make([]byte, 3)}
	buf := make([]byte, 8)
	n, err := reader.Read(buf)
	if err != nil || string(buf[:n]) != "abcdef" {
		t.Fatalf("first read = %q, %v; want bytes then nil", buf[:n], err)
	}
	n, err = reader.Read(buf)
	if n != 0 || !errors.Is(err, wantErr) {
		t.Fatalf("second read = %d, %v; want injected error", n, err)
	}
}

func TestContentBackendsPreserveBytesAndDigests(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	paths := []string{
		filepath.Join(root, "small.txt"),
		filepath.Join(root, "nested", "large.txt"),
		filepath.Join(root, "unicode-雪.txt"),
	}
	if err := os.MkdirAll(filepath.Dir(paths[1]), 0o700); err != nil {
		t.Fatal(err)
	}
	contents := [][]byte{
		[]byte("first line\nlast line without newline"),
		fileScanTestBytes(350_017),
		[]byte("雪\nsecret-ish-value\n"),
	}
	for i := range paths {
		if err := os.WriteFile(paths[i], contents[i], 0o600); err != nil {
			t.Fatal(err)
		}
	}

	baseline := collectFileScanDigest(t, root, FileScanConfig{
		Backend: FileScanBackendBaseline, Namespace: FileScanNamespaceParallel,
		InlineDetector: true, ActiveFiles: 4, Walkers: 3,
	})
	configs := map[string]FileScanConfig{
		"buffered-read": {
			Backend: FileScanBackendBuffered, ReadMode: FileScanReadModeRead,
			PrefetchBytes: 128 * 1024, Advice: []string{"sequential"},
			InlineDetector: true, ActiveFiles: 4, Walkers: 3,
		},
		"buffered-pread": {
			Backend: FileScanBackendBuffered, ReadMode: FileScanReadModePread,
			PrefetchBytes: 512 * 1024, Advice: []string{"sequential", "noreuse"},
			InlineDetector: true, ActiveFiles: 4, Walkers: 3,
		},
		"serial-baseline": {
			Backend: FileScanBackendBaseline, Namespace: FileScanNamespaceSerial,
			InlineDetector: true, ActiveFiles: 4, Walkers: 3,
		},
		"openat-baseline": {
			Backend: FileScanBackendBaseline, Namespace: FileScanNamespaceOpenat,
			InlineDetector: true, ActiveFiles: 4, Walkers: 3,
		},
	}
	for name, cfg := range configs {
		t.Run(name, func(t *testing.T) {
			got := collectFileScanDigest(t, root, cfg)
			if got.Targets != baseline.Targets || got.Fragments != baseline.Fragments ||
				got.Errors != baseline.Errors {
				t.Fatalf("correctness digests differ\nbaseline: %+v\ncandidate: %+v", baseline, got)
			}
		})
	}
}

func TestRandomAccessArchiveUsesProductionFileRoute(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "bundle.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(file)
	entry, err := zw.Create("inside.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(entry, "inside archive\nwithout final newline"); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(zw.Close(), file.Close()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	metrics := NewFileScanMetrics()
	cfg := (FileScanConfig{
		Backend: FileScanBackendBuffered, PrefetchBytes: 128 * 1024,
		Strict: true, Metrics: metrics,
	}).Normalize()
	backend, err := newContentBackend(t.Context(), root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := backend.Open(t.Context(), ScanTarget{Path: path, Size: info.Size()})
	if err != nil {
		t.Fatal(err)
	}
	if opened.Backend != FileScanBackendBaseline || opened.Fallback != "" {
		t.Fatalf("archive route = backend %q fallback %q", opened.Backend, opened.Fallback)
	}
	if _, ok := opened.Reader.(io.ReaderAt); !ok {
		t.Fatalf("archive reader %T does not implement ReaderAt", opened.Reader)
	}
	if _, ok := opened.Reader.(io.Seeker); !ok {
		t.Fatalf("archive reader %T does not implement Seeker", opened.Reader)
	}
	if err := errors.Join(opened.Close(), backend.Close()); err != nil {
		t.Fatal(err)
	}
	if metrics.Snapshot().Digests.Fallbacks.Count != 0 {
		t.Fatal("intentional archive routing was recorded as a fallback")
	}

	baseline := collectFileScanDigest(t, root, FileScanConfig{
		Backend: FileScanBackendBaseline, InlineDetector: true, ActiveFiles: 2, Walkers: 2,
	})
	buffered := collectFileScanDigest(t, root, FileScanConfig{
		Backend: FileScanBackendBuffered, PrefetchBytes: 128 * 1024,
		InlineDetector: true, ActiveFiles: 2, Walkers: 2, Strict: true,
	})
	if baseline.Fragments != buffered.Fragments || baseline.Errors != buffered.Errors {
		t.Fatalf("archive candidate changed output\nbaseline: %+v\nbuffered: %+v", baseline, buffered)
	}
}

func TestDispatcherRetainsOwnershipAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	metrics := NewFileScanMetrics()
	started := make(chan struct{})
	release := make(chan struct{})
	dispatcher := newFileScanDispatcher(ctx, 1, metrics, func(Fragment, error) error {
		close(started)
		<-release
		return nil
	})
	local := metrics.NewLocal()
	done := make(chan error, 1)
	go func() {
		done <- dispatcher.callback(local)(Fragment{Raw: "owned"}, nil)
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		t.Fatalf("callback returned before worker released ownership: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.Close(); err != nil {
		t.Fatal(err)
	}
	metrics.MergeLocal(local)
	queue := metrics.Gauge("fragment_queue_depth")
	if queue.Current != 0 || queue.Peak != 1 {
		t.Fatalf("queue gauge = %+v, want current 0 peak 1", queue)
	}
}

func TestUringArenaIsReservedOnceInGlobalBudget(t *testing.T) {
	metrics := NewFileScanMetrics()
	cfg := (FileScanConfig{
		Backend:          FileScanBackendUring,
		QueueDepth:       8,
		PrefetchBytes:    128 * 1024,
		MaxInFlightBytes: 4 * 1024 * 1024,
		ActiveFiles:      8,
		Metrics:          metrics,
	}).Normalize()
	backend, err := newContentBackend(t.Context(), t.TempDir(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	wantArena := int64(cfg.QueueDepth) * fileScanRoundedBufferBytes(cfg.PrefetchBytes)
	if backend.transportBytes != wantArena {
		t.Fatalf("reserved transport = %d, want %d", backend.transportBytes, wantArena)
	}
	if got := fileScanSessionMemoryCost(cfg); got != int64(defaultBufferSize+4096) {
		t.Fatalf("per-file session cost = %d, want framing-only %d", got, defaultBufferSize+4096)
	}
	if gauge := metrics.Gauge(FileScanGaugeInFlightBytes); gauge.Current != wantArena {
		t.Fatalf("in-flight reserve = %+v, want current %d", gauge, wantArena)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	if gauge := metrics.Gauge(FileScanGaugeInFlightBytes); gauge.Current != 0 || gauge.Peak != wantArena {
		t.Fatalf("closed in-flight reserve = %+v, want current 0 peak %d", gauge, wantArena)
	}
}

func collectFileScanDigest(t *testing.T, root string, cfg FileScanConfig) FileScanDigestsSnapshot {
	t.Helper()
	metrics := NewFileScanMetrics()
	cfg.Metrics = metrics
	cfg = cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	source := &Files{Path: root, MaxArchiveDepth: 8, FileScan: &cfg}
	if err := source.Fragments(t.Context(), func(Fragment, error) error { return nil }); err != nil {
		t.Fatal(err)
	}
	return metrics.Snapshot().Digests
}

func fileScanTestBytes(size int) []byte {
	pattern := []byte("0123456789abcdefghijklmnopqrstuvwxyz\n")
	data := make([]byte, size)
	for i := range data {
		data[i] = pattern[i%len(pattern)]
	}
	return data
}

type fileScanReadStep struct {
	data []byte
	err  error
}

type fileScanScriptedReadAtCloser struct {
	mu    sync.Mutex
	steps []fileScanReadStep
}

func (s *fileScanScriptedReadAtCloser) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.steps) == 0 {
		return 0, io.EOF
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	return copy(p, step.data), step.err
}

func (s *fileScanScriptedReadAtCloser) ReadAt(p []byte, _ int64) (int, error) {
	return s.Read(p)
}

func (*fileScanScriptedReadAtCloser) Close() error { return nil }

var _ fileScanReadAtCloser = (*fileScanScriptedReadAtCloser)(nil)

func TestFileScanDigestSnapshotComparable(t *testing.T) {
	// The tournament depends on value-comparable identities for exact gates.
	if !reflect.TypeFor[FileScanDigestsSnapshot]().Comparable() {
		t.Fatal("digest snapshot must remain comparable")
	}
}
