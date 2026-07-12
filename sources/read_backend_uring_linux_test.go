//go:build linux && filescan_uring

package sources

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRawUringOpenReadClose(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input")
	want := bytes.Repeat([]byte("ordered-uring-read\n"), 64)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(rootFD)

	for _, registered := range []bool{false, true} {
		name := "unregistered"
		if registered {
			name = "registered"
		}
		t.Run(name, func(t *testing.T) {
			metrics := &FileScanMetrics{}
			cfg := FileScanConfig{
				Backend:           FileScanBackendUring,
				Namespace:         FileScanNamespaceUringOpenat,
				PrefetchBytes:     127,
				QueueDepth:        4,
				RegisteredBuffers: registered,
				MaxInFlightBytes:  1 << 20,
				ActiveFiles:       1,
				Metrics:           metrics,
			}
			provider, err := newUringProvider(context.Background(), rootFD, root, cfg)
			if err != nil {
				skipUnavailableUring(t, err)
			}
			if registered {
				gauge := metrics.Gauge(FileScanGaugePinnedBytes)
				slotBytes, err := roundUp(cfg.PrefetchBytes, os.Getpagesize())
				if err != nil {
					t.Fatal(err)
				}
				wantPinned := int64(cfg.QueueDepth * slotBytes)
				if gauge.Current != wantPinned || gauge.Peak != wantPinned {
					t.Fatalf("pinned gauge while open = %+v, want %d", gauge, wantPinned)
				}
			}

			reader, err := provider.openReader(
				context.Background(),
				ScanTarget{Path: path, Size: int64(len(want))},
				cfg,
				false,
			)
			if err != nil {
				_ = provider.close()
				t.Fatal(err)
			}
			got, err := io.ReadAll(reader)
			if err != nil {
				_ = reader.Close()
				_ = provider.close()
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("read mismatch: got %d bytes, want %d", len(got), len(want))
			}
			if err := reader.Close(); err != nil {
				_ = provider.close()
				t.Fatal(err)
			}
			if err := provider.close(); err != nil {
				t.Fatal(err)
			}
			if gauge := metrics.Gauge(FileScanGaugePinnedBytes); gauge.Current != 0 {
				t.Fatalf("pinned gauge after close = %+v", gauge)
			}
			for _, gaugeName := range []string{
				FileScanGaugeQueueDepth,
				uringGaugeSubmittedBytes,
				uringGaugeCompletedUnconsumedBytes,
				uringGaugeReorderDepth,
			} {
				if gauge := metrics.Gauge(gaugeName); gauge.Current != 0 {
					t.Fatalf("%s gauge after close = %+v", gaugeName, gauge)
				}
			}
			if gauge := metrics.Gauge(FileScanGaugeQueueDepth); gauge.Peak == 0 || gauge.Peak > int64(cfg.QueueDepth) {
				t.Fatalf("queue-depth peak = %d, want 1..%d", gauge.Peak, cfg.QueueDepth)
			}
			maxTransportBytes := int64(cfg.QueueDepth * cfg.PrefetchBytes)
			if gauge := metrics.Gauge(uringGaugeSubmittedBytes); gauge.Peak == 0 || gauge.Peak > maxTransportBytes {
				t.Fatalf("submitted-byte peak = %d, want 1..%d", gauge.Peak, maxTransportBytes)
			}
			if gauge := metrics.Gauge(uringGaugeCompletedUnconsumedBytes); gauge.Peak == 0 || gauge.Peak > maxTransportBytes {
				t.Fatalf("completed-byte peak = %d, want 1..%d", gauge.Peak, maxTransportBytes)
			}
			if gauge := metrics.Gauge(uringGaugeReorderDepth); gauge.Peak > int64(cfg.QueueDepth-1) {
				t.Fatalf("reorder-depth peak = %d, want <= %d", gauge.Peak, cfg.QueueDepth-1)
			}
		})
	}
}

func TestRunLockedUringOwnerPinsThread(t *testing.T) {
	type threadIDs struct {
		want int
		got  int
	}
	result := make(chan threadIDs, 1)
	go runLockedUringOwner(func() {
		want := unix.Gettid()
		for range 1_000 {
			runtime.Gosched()
			if got := unix.Gettid(); got != want {
				result <- threadIDs{want: want, got: got}
				return
			}
		}
		result <- threadIDs{want: want, got: want}
	})

	select {
	case ids := <-result:
		if ids.got != ids.want {
			t.Fatalf("owner thread migrated from %d to %d", ids.want, ids.got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("locked owner thread did not finish")
	}
}

func TestRawUringConcurrentOpenReadClose(t *testing.T) {
	root := t.TempDir()
	const fileCount = 64
	want := bytes.Repeat([]byte("concurrent-uring-read\n"), 64)
	targets := make([]ScanTarget, 0, fileCount)
	for i := range fileCount {
		path := filepath.Join(root, fmt.Sprintf("input-%03d", i))
		if err := os.WriteFile(path, want, 0o600); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, ScanTarget{Path: path, Size: int64(len(want))})
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(rootFD)

	cfg := FileScanConfig{
		Backend:          FileScanBackendUring,
		Namespace:        FileScanNamespaceUringOpenat,
		PrefetchBytes:    128 << 10,
		QueueDepth:       32,
		MaxInFlightBytes: 64 << 20,
		ActiveFiles:      128,
		Metrics:          NewFileScanMetrics(),
	}
	ctx := t.Context()
	provider, err := newUringProvider(ctx, rootFD, root, cfg)
	if err != nil {
		skipUnavailableUring(t, err)
	}

	const workers = 128
	const iterations = 32
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for worker := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for iteration := range iterations {
				target := targets[(worker+iteration)%len(targets)]
				reader, err := provider.openReader(ctx, target, cfg, false)
				if err != nil {
					errs <- err
					return
				}
				got, readErr := io.ReadAll(reader)
				closeErr := reader.Close()
				if err := errors.Join(readErr, closeErr); err != nil {
					errs <- err
					return
				}
				if !bytes.Equal(got, want) {
					errs <- fmt.Errorf("read %d bytes, want %d", len(got), len(want))
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	closeErr := provider.close()
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestRawUringNamespaceWithBaselineContent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input")
	want := bytes.Repeat([]byte("baseline-read\n"), 64)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	metrics := NewFileScanMetrics()
	cfg := (FileScanConfig{
		Backend:          FileScanBackendBaseline,
		Namespace:        FileScanNamespaceUringOpenat,
		QueueDepth:       4,
		MaxInFlightBytes: 1 << 20,
		ActiveFiles:      1,
		Strict:           true,
		Metrics:          metrics,
	}).Normalize()
	backend, err := newContentBackend(t.Context(), root, cfg)
	if err != nil {
		skipUnavailableUring(t, err)
	}
	platform := backend.platform.(*linuxPlatformContentBackend)
	raw := platform.uring.(*rawUringProvider)
	if len(raw.bufferArena) != 0 || raw.bufferPool != nil {
		_ = backend.Close()
		t.Fatalf("namespace-only provider allocated content buffers: arena=%d pool=%v", len(raw.bufferArena), raw.bufferPool != nil)
	}

	opened, err := backend.Open(t.Context(), ScanTarget{Path: path, Size: int64(len(want))})
	if err != nil {
		_ = backend.Close()
		t.Fatal(err)
	}
	if opened.Backend != FileScanBackendBaseline {
		_ = opened.Close()
		_ = backend.Close()
		t.Fatalf("selected backend = %q, want %q", opened.Backend, FileScanBackendBaseline)
	}
	if _, ok := opened.Reader.(*os.File); !ok {
		_ = opened.Close()
		_ = backend.Close()
		t.Fatalf("baseline content reader = %T, want *os.File", opened.Reader)
	}
	got, readErr := io.ReadAll(opened.Reader)
	if err := errors.Join(readErr, opened.Close(), backend.Close()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(want))
	}
	if gauge := metrics.Gauge(FileScanGaugeQueueDepth); gauge.Peak == 0 {
		t.Fatal("io_uring namespace open did not reach the submission queue")
	}
	if gauge := metrics.Gauge(uringGaugeSubmittedBytes); gauge.Peak != 0 {
		t.Fatalf("baseline content submitted io_uring read bytes: %+v", gauge)
	}
	if gauge := metrics.Gauge(FileScanGaugePinnedBytes); gauge.Peak != 0 {
		t.Fatalf("namespace-only provider pinned content buffers: %+v", gauge)
	}
}

func TestRawUringUnregisteredArenaMatchesReservedBytes(t *testing.T) {
	const depth = 4
	prefetch := os.Getpagesize() + 1
	slotBytes, err := roundUp(prefetch, os.Getpagesize())
	if err != nil {
		t.Fatal(err)
	}
	wantArena := depth * slotBytes

	provider := &rawUringProvider{depth: depth, bufSize: prefetch}
	if err := provider.initBuffers(FileScanConfig{MaxInFlightBytes: int64(wantArena)}); err != nil {
		t.Fatal(err)
	}
	defer provider.releaseBuffers()
	if provider.bufferDepth != depth {
		t.Fatalf("buffer depth = %d, want queue depth %d", provider.bufferDepth, depth)
	}
	if got := len(provider.bufferArena); got != wantArena {
		t.Fatalf("arena bytes = %d, want page-rounded reservation %d", got, wantArena)
	}
	if got := len(provider.bufferPool); got != depth {
		t.Fatalf("available buffers = %d, want %d", got, depth)
	}

	overBudget := &rawUringProvider{depth: depth, bufSize: prefetch}
	if err := overBudget.initBuffers(FileScanConfig{MaxInFlightBytes: int64(wantArena - 1)}); err == nil {
		overBudget.releaseBuffers()
		t.Fatal("page-rounded arena unexpectedly fit a smaller byte budget")
	}
}

func TestRawUringOpenFDErrorMatchesSynchronousErrno(t *testing.T) {
	completeOpen := func(t *testing.T, errno syscall.Errno) error {
		t.Helper()
		provider := inertUringProvider(1, true)
		provider.rootFD = -1
		done := make(chan error, 1)
		go func() {
			_, err := provider.openFD(
				context.Background(),
				ScanTarget{Path: "/permission-denied"},
				unix.O_RDONLY|unix.O_CLOEXEC,
			)
			done <- err
		}()

		var command *uringCommand
		deadline := time.Now().Add(5 * time.Second)
		for command == nil && time.Now().Before(deadline) {
			provider.queueMu.Lock()
			if len(provider.queue) > 0 {
				command = provider.queue[0]
				provider.queue = nil
			}
			provider.queueMu.Unlock()
			runtimeYield()
		}
		if command == nil {
			t.Fatal("open command was not queued")
		}
		command.finish(uringResult{res: -int32(errno)})
		return <-done
	}

	permissionErr := completeOpen(t, unix.EACCES)
	if permissionErr != unix.EACCES {
		t.Fatalf("io_uring permission error = %#v, want raw errno %#v", permissionErr, unix.EACCES)
	}
	if got, want := permissionErr.Error(), unix.EACCES.Error(); got != want {
		t.Fatalf("io_uring permission identity = %q, want synchronous identity %q", got, want)
	}
	var unsupported *backendUnsupportedError
	if errors.As(permissionErr, &unsupported) {
		t.Fatalf("ordinary permission error classified as unsupported: %v", permissionErr)
	}

	unsupportedErr := completeOpen(t, unix.EOPNOTSUPP)
	if !errors.As(unsupportedErr, &unsupported) || !errors.Is(unsupported.err, unix.EOPNOTSUPP) {
		t.Fatalf("unsupported open error = %v, want backendUnsupportedError wrapping EOPNOTSUPP", unsupportedErr)
	}
}

func TestRawUringProviderCloseClosesLiveReader(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(rootFD)
	cfg := FileScanConfig{
		Backend:          FileScanBackendUring,
		Namespace:        FileScanNamespaceUringOpenat,
		PrefetchBytes:    4096,
		QueueDepth:       4,
		MaxInFlightBytes: 1 << 20,
		ActiveFiles:      1,
	}
	provider, err := newUringProvider(context.Background(), rootFD, root, cfg)
	if err != nil {
		skipUnavailableUring(t, err)
	}
	reader, err := provider.openReader(
		context.Background(),
		ScanTarget{Path: path, Size: 1 << 20},
		cfg,
		false,
	)
	if err != nil {
		_ = provider.close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- provider.close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("provider close blocked with a live reader")
	}
	if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("read after provider close = %v, want os.ErrClosed", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRawUringDirectRead(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "direct-input")
	want := bytes.Repeat([]byte("direct-uring-read\n"), 700)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(rootFD)

	for _, registered := range []bool{false, true} {
		name := "unregistered"
		if registered {
			name = "registered"
		}
		t.Run(name, func(t *testing.T) {
			cfg := FileScanConfig{
				Backend:           FileScanBackendUringDirect,
				Namespace:         FileScanNamespaceUringOpenat,
				PrefetchBytes:     4096,
				QueueDepth:        4,
				RegisteredBuffers: registered,
				MaxInFlightBytes:  1 << 20,
				ActiveFiles:       1,
			}
			provider, err := newUringProvider(context.Background(), rootFD, root, cfg)
			if err != nil {
				skipUnavailableUring(t, err)
			}
			reader, err := provider.openReader(
				context.Background(),
				ScanTarget{Path: path, Size: int64(len(want))},
				cfg,
				true,
			)
			if err != nil {
				_ = provider.close()
				if errors.Is(err, errBackendUnsupported) {
					t.Skipf("O_DIRECT unavailable in test environment: %v", err)
				}
				t.Fatal(err)
			}
			got, err := io.ReadAll(reader)
			if err != nil {
				_ = reader.Close()
				_ = provider.close()
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("direct read mismatch: got %d bytes, want %d", len(got), len(want))
			}
			if err := reader.Close(); err != nil {
				_ = provider.close()
				t.Fatal(err)
			}
			if err := provider.close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRawUringCloseFallbackRequiresUnsubmittedCommand(t *testing.T) {
	t.Run("unsubmitted falls back", func(t *testing.T) {
		readFD, writeFD := pipeFDs(t)
		defer unix.Close(writeFD)
		provider := inertUringProvider(1, false)
		if err := provider.closeOwnedFD(readFD); !errors.Is(err, errUringProviderClosed) {
			t.Fatalf("close error = %v, want provider closed", err)
		}
		if _, err := unix.FcntlInt(uintptr(readFD), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
			t.Fatalf("unsubmitted fallback left fd open: %v", err)
		}
	})

	t.Run("submitted unknown does not retry", func(t *testing.T) {
		readFD, writeFD := pipeFDs(t)
		defer unix.Close(readFD)
		defer unix.Close(writeFD)
		provider := inertUringProvider(1, true)
		done := make(chan error, 1)
		go func() { done <- provider.closeOwnedFD(readFD) }()

		var command *uringCommand
		deadline := time.Now().Add(5 * time.Second)
		for command == nil && time.Now().Before(deadline) {
			provider.queueMu.Lock()
			if len(provider.queue) > 0 {
				command = provider.queue[0]
				provider.queue = nil
			}
			provider.queueMu.Unlock()
			runtimeYield()
		}
		if command == nil {
			t.Fatal("close command was not queued")
		}
		command.future.submitted.Store(true)
		command.finish(uringResult{err: errors.New("owner failed")})
		if err := <-done; err == nil || !strings.Contains(err.Error(), "outcome unknown") {
			t.Fatalf("close error = %v, want unknown outcome", err)
		}
		if _, err := unix.FcntlInt(uintptr(readFD), unix.F_GETFD, 0); err != nil {
			t.Fatalf("submitted unknown close retried synchronously: %v", err)
		}
	})
}

func TestRawUringOrdinaryAdmissionIsBounded(t *testing.T) {
	provider := inertUringProvider(2, true)
	first, err := provider.newOrdinaryCommand(context.Background(), ioUringSQE{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.newOrdinaryCommand(context.Background(), ioUringSQE{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := provider.newOrdinaryCommand(ctx, ioUringSQE{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("third ordinary admission = %v, want deadline", err)
	}
	// The D-slot control lane remains available when all D ordinary slots are
	// occupied, but the combined queued+pending population is bounded at 2D.
	controls := make([]*uringCommand, 0, 2)
	for _, target := range []uint64{first.future.id, second.future.id} {
		if err := provider.acquireControl(context.Background()); err != nil {
			t.Fatal(err)
		}
		control := provider.newCancelCommand(target)
		provider.bindControlAdmission(control)
		controls = append(controls, control)
	}
	controlCtx, controlCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer controlCancel()
	if err := provider.acquireControl(controlCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("third control admission = %v, want deadline", err)
	}
	first.finish(uringResult{})
	third, err := provider.newOrdinaryCommand(context.Background(), ioUringSQE{})
	if err != nil {
		t.Fatal(err)
	}
	second.finish(uringResult{})
	third.finish(uringResult{})
	for _, control := range controls {
		control.finish(uringResult{})
	}
	if got := len(provider.admission); got != cap(provider.admission) {
		t.Fatalf("ordinary admission tokens = %d, want %d", got, cap(provider.admission))
	}
	if got := len(provider.controlAdmission); got != cap(provider.controlAdmission) {
		t.Fatalf("control admission tokens = %d, want %d", got, cap(provider.controlAdmission))
	}
}

func skipUnavailableUring(t *testing.T, err error) {
	t.Helper()
	var unsupported *backendUnsupportedError
	if errors.As(err, &unsupported) && isUringDeploymentDenied(unsupported.err) {
		t.Skipf("io_uring unavailable in test environment: %v", err)
	}
	t.Fatal(err)
}

func isUringDeploymentDenied(err error) bool {
	return errors.Is(err, unix.EPERM) ||
		errors.Is(err, unix.EACCES) ||
		errors.Is(err, unix.ENOSYS)
}

func pipeFDs(t *testing.T) (int, int) {
	t.Helper()
	var fds [2]int
	if err := unix.Pipe(fds[:]); err != nil {
		t.Fatal(err)
	}
	return fds[0], fds[1]
}

func inertUringProvider(depth int, accepting bool) *rawUringProvider {
	provider := &rawUringProvider{
		depth:            depth,
		wakeFD:           -1,
		accepting:        accepting,
		ownerDone:        make(chan struct{}),
		admission:        make(chan struct{}, depth),
		controlAdmission: make(chan struct{}, depth),
	}
	for range depth {
		provider.admission <- struct{}{}
		provider.controlAdmission <- struct{}{}
	}
	return provider
}

func runtimeYield() {
	time.Sleep(time.Millisecond)
}
