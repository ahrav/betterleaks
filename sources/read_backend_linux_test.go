//go:build linux

package sources

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

type namespaceOnlyUringTestProvider struct {
	openFileCalls   int
	openReaderCalls int
	closeCalls      int
}

func (p *namespaceOnlyUringTestProvider) openFile(
	_ context.Context,
	target ScanTarget,
	_ int,
) (*os.File, error) {
	p.openFileCalls++
	return os.Open(target.Path)
}

func (p *namespaceOnlyUringTestProvider) openReader(
	context.Context,
	ScanTarget,
	FileScanConfig,
	bool,
) (io.ReadCloser, error) {
	p.openReaderCalls++
	return nil, errors.New("unexpected io_uring content read")
}

func (p *namespaceOnlyUringTestProvider) close() error {
	p.closeCalls++
	return nil
}

func TestRunFileScanMmapHelperExactWindows(t *testing.T) {
	t.Parallel()
	page := os.Getpagesize()
	window := int64(2 * page)
	for _, sequential := range []bool{false, true} {
		for _, size := range []int{1, page - 1, page, 2*page + 17, 9*page + 113} {
			want := fileScanTestBytes(size)
			path := filepath.Join(t.TempDir(), "mapped.txt")
			if err := os.WriteFile(path, want, 0o600); err != nil {
				t.Fatal(err)
			}
			var got bytes.Buffer
			if err := RunFileScanMmapHelper(t.Context(), path, window, sequential, &got); err != nil {
				t.Fatalf("sequential=%t size=%d: %v", sequential, size, err)
			}
			if !bytes.Equal(got.Bytes(), want) {
				t.Fatalf("sequential=%t size=%d: content mismatch", sequential, size)
			}
		}
	}
}

func TestMmapProcessReaderDetectsShortOutputAndHelperFailure(t *testing.T) {
	want := fileScanTestBytes(3*os.Getpagesize() + 37)
	path := filepath.Join(t.TempDir(), "mapped.txt")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		mode    string
		wantErr string
	}{
		{name: "exact", mode: "exact"},
		{name: "short", mode: "short", wantErr: "unexpected EOF"},
		{name: "failure", mode: "failure", wantErr: "exit status"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestFileScanMmapSubprocess$")
			cmd.Env = append(os.Environ(),
				"BETTERLEAKS_FILESCAN_MMAP_TEST_HELPER=1",
				"BETTERLEAKS_FILESCAN_MMAP_TEST_MODE="+test.mode,
				"BETTERLEAKS_FILESCAN_MMAP_TEST_PATH="+path,
			)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			reader := &mmapProcessReader{cmd: cmd, stdout: stdout, expected: int64(len(want))}
			got, readErr := io.ReadAll(reader)
			closeErr := reader.Close()
			if test.wantErr == "" {
				if err := errors.Join(readErr, closeErr); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Fatal("exact helper content mismatch")
				}
				return
			}
			if readErr == nil || !strings.Contains(readErr.Error(), test.wantErr) {
				t.Fatalf("read error = %v, want substring %q", readErr, test.wantErr)
			}
		})
	}
}

func TestFileScanMmapSubprocess(t *testing.T) {
	if os.Getenv("BETTERLEAKS_FILESCAN_MMAP_TEST_HELPER") != "1" {
		return
	}
	switch os.Getenv("BETTERLEAKS_FILESCAN_MMAP_TEST_MODE") {
	case "exact":
		err := RunFileScanMmapHelper(
			context.Background(),
			os.Getenv("BETTERLEAKS_FILESCAN_MMAP_TEST_PATH"),
			int64(2*os.Getpagesize()),
			true,
			os.Stdout,
		)
		if err != nil {
			panic(err)
		}
		os.Exit(0)
	case "short":
		_, _ = io.WriteString(os.Stdout, "short")
		os.Exit(0)
	case "failure":
		os.Exit(23)
	default:
		panic("unknown mmap helper test mode")
	}
}

func TestDirectReaderPreservesUnalignedTail(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	want := fileScanTestBytes(3*128*1024 + 137)
	path := filepath.Join(root, "direct.txt")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	metrics := NewFileScanMetrics()
	cfg := (FileScanConfig{
		Backend: FileScanBackendDirect, PrefetchBytes: 128 * 1024,
		MaxInFlightBytes: 2 * 1024 * 1024, ActiveFiles: 1,
		Strict: true, Metrics: metrics,
	}).Normalize()
	backend, err := newContentBackend(t.Context(), root, cfg)
	if err != nil {
		if errors.Is(err, errBackendUnsupported) {
			t.Skipf("direct I/O unavailable: %v", err)
		}
		t.Fatal(err)
	}
	opened, err := backend.Open(t.Context(), ScanTarget{Path: path, Size: info.Size()})
	if err != nil {
		_ = backend.Close()
		if errors.Is(err, errBackendUnsupported) {
			t.Skipf("direct I/O unavailable: %v", err)
		}
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(opened.Reader)
	closeErr := opened.Close()
	backendErr := backend.Close()
	if err := errors.Join(readErr, closeErr, backendErr); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("direct content length=%d, want %d", len(got), len(want))
	}
	gauge := metrics.Gauge(FileScanGaugeInFlightBytes)
	if gauge.Current != 0 || gauge.Peak == 0 || gauge.Peak > cfg.MaxInFlightBytes {
		t.Fatalf("in-flight gauge = %+v, budget=%d", gauge, cfg.MaxInFlightBytes)
	}
}

func TestRootRelativeOpenatPreservesOutsideSymlinkTarget(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	want := []byte("outside root\n")
	target := filepath.Join(parent, "outside.txt")
	if err := os.WriteFile(target, want, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := (FileScanConfig{
		Backend: FileScanBackendBaseline, Namespace: FileScanNamespaceOpenat,
		Strict: true, Metrics: NewFileScanMetrics(),
	}).Normalize()
	backend, err := newContentBackend(t.Context(), root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := backend.Open(t.Context(), ScanTarget{
		Path: target, Symlink: filepath.Join(root, "outside-link"), Size: int64(len(want)),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(opened.Reader)
	if err := errors.Join(readErr, opened.Close(), backend.Close()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("openat content = %q, want %q", got, want)
	}
}

func TestBaselineUringNamespaceKeepsBaselineContentReader(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "content.txt")
	want := []byte("baseline content through an io_uring-opened descriptor\n")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &namespaceOnlyUringTestProvider{}
	cfg := (FileScanConfig{
		Backend:          FileScanBackendBaseline,
		Namespace:        FileScanNamespaceUringOpenat,
		QueueDepth:       4,
		MaxInFlightBytes: 1 << 20,
		ActiveFiles:      1,
		Metrics:          NewFileScanMetrics(),
	}).Normalize()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	backend := &contentBackend{
		cfg: cfg,
		platform: &linuxPlatformContentBackend{
			ctx: t.Context(), rootPath: root, rootFD: -1, uring: provider,
		},
	}
	opened, err := backend.Open(t.Context(), ScanTarget{Path: path, Size: int64(len(want))})
	if err != nil {
		t.Fatal(err)
	}
	if opened.Backend != FileScanBackendBaseline {
		t.Fatalf("selected backend = %q, want %q", opened.Backend, FileScanBackendBaseline)
	}
	if _, ok := opened.Reader.(*os.File); !ok {
		t.Fatalf("baseline content reader = %T, want *os.File", opened.Reader)
	}
	got, readErr := io.ReadAll(opened.Reader)
	if err := errors.Join(readErr, opened.Close(), backend.Close()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("content = %q, want %q", got, want)
	}
	if provider.openFileCalls != 1 || provider.openReaderCalls != 0 || provider.closeCalls != 1 {
		t.Fatalf(
			"provider calls: openFile=%d openReader=%d close=%d",
			provider.openFileCalls,
			provider.openReaderCalls,
			provider.closeCalls,
		)
	}
}

func TestMmapAlignedMeetsRequestedAlignment(t *testing.T) {
	for _, alignment := range []int{512, os.Getpagesize(), 64 * 1024} {
		raw, aligned, err := mmapAligned(123_457, alignment)
		if err != nil {
			t.Fatal(err)
		}
		if uintptr(unsafe.Pointer(unsafe.SliceData(aligned)))%uintptr(alignment) != 0 {
			t.Fatalf("buffer is not aligned to %d", alignment)
		}
		if err := unix.Munmap(raw); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDirectAlignmentMustDividePage(t *testing.T) {
	pageSize := os.Getpagesize()
	for _, alignment := range []uint32{1, uint32(pageSize / 2), uint32(pageSize)} {
		if !directAlignmentDividesPage(alignment) {
			t.Fatalf("alignment %d should divide page size %d", alignment, pageSize)
		}
	}
	for _, alignment := range []uint32{0, uint32(pageSize - 1), uint32(pageSize + 1)} {
		if directAlignmentDividesPage(alignment) {
			t.Fatalf("alignment %d should not divide page size %d", alignment, pageSize)
		}
	}
}

func TestDirectSupportedMmapMatchesPageRoundedCharge(t *testing.T) {
	for _, size := range []int{defaultBufferSize, defaultBufferSize + 1, 3*os.Getpagesize() + 17} {
		for _, alignment := range []int{512, os.Getpagesize()} {
			if os.Getpagesize()%alignment != 0 {
				continue
			}
			raw, _, err := mmapAligned(size, alignment)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := int64(len(raw)), fileScanRoundedBufferBytes(size); got != want {
				_ = unix.Munmap(raw)
				t.Fatalf("size=%d alignment=%d: mmap bytes=%d, charged=%d", size, alignment, got, want)
			}
			if err := unix.Munmap(raw); err != nil {
				t.Fatal(err)
			}
		}
	}
}
