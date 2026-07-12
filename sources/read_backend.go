package sources

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/mholt/archives"
	"golang.org/x/sync/semaphore"
)

var errBackendUnsupported = errors.New("file scan backend unsupported")

type backendUnsupportedError struct {
	reason string
	err    error
}

func (e *backendUnsupportedError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("%v: %s", errBackendUnsupported, e.reason)
	}
	return fmt.Sprintf("%v: %s: %v", errBackendUnsupported, e.reason, e.err)
}

func (e *backendUnsupportedError) Unwrap() error {
	return errBackendUnsupported
}

type openedContent struct {
	Reader   io.Reader
	Backend  FileScanBackend
	Fallback string
	close    func() error
	once     sync.Once
	err      error
}

func (o *openedContent) Close() error {
	o.once.Do(func() {
		if o.close != nil {
			o.err = o.close()
		}
	})
	return o.err
}

type platformContentBackend interface {
	openFile(ScanTarget, FileScanNamespace) (*os.File, error)
	advise(*os.File, FileScanConfig) error
	openDirect(context.Context, ScanTarget, FileScanConfig) (io.ReadCloser, error)
	openMmap(context.Context, ScanTarget, FileScanConfig) (io.ReadCloser, error)
	openUring(context.Context, ScanTarget, FileScanConfig, bool) (io.ReadCloser, error)
	close() error
}

type contentBackend struct {
	cfg            FileScanConfig
	platform       platformContentBackend
	budget         *semaphore.Weighted
	transportBytes int64
	closeOnce      sync.Once
	closeErr       error
}

func newContentBackend(ctx context.Context, root string, cfg FileScanConfig) (*contentBackend, error) {
	cfg = cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	platform, err := newPlatformContentBackend(ctx, root, cfg)
	if err != nil {
		return nil, err
	}
	b := &contentBackend{cfg: cfg, platform: platform}
	budgetBytes := cfg.MaxInFlightBytes
	if cfg.Backend == FileScanBackendUring || cfg.Backend == FileScanBackendUringDirect {
		b.transportBytes = saturatingFileScanMul(
			int64(cfg.QueueDepth),
			fileScanRoundedBufferBytes(cfg.PrefetchBytes),
		)
		budgetBytes -= b.transportBytes
	}
	if cfg.MaxInFlightBytes > 0 {
		if cost := fileScanSessionMemoryCost(cfg); cost > budgetBytes {
			_ = platform.close()
			return nil, fmt.Errorf(
				"file scan session requires %d bytes, exceeds available in-flight bytes %d",
				cost,
				budgetBytes,
			)
		}
		b.budget = semaphore.NewWeighted(budgetBytes)
	}
	cfg.Metrics.AddGauge(FileScanGaugeInFlightBytes, b.transportBytes)
	return b, nil
}

func (b *contentBackend) Close() error {
	if b == nil {
		return nil
	}
	b.closeOnce.Do(func() {
		if b.platform != nil {
			b.closeErr = b.platform.close()
		}
		b.cfg.Metrics.AddGauge(FileScanGaugeInFlightBytes, -b.transportBytes)
	})
	return b.closeErr
}

func (b *contentBackend) Open(ctx context.Context, target ScanTarget) (*openedContent, error) {
	cost := fileScanSessionMemoryCost(b.cfg)
	if b.budget != nil {
		if err := b.budget.Acquire(ctx, cost); err != nil {
			return nil, err
		}
	}
	b.cfg.Metrics.AddGauge(FileScanGaugeInFlightBytes, cost)
	release := func() {
		b.cfg.Metrics.AddGauge(FileScanGaugeInFlightBytes, -cost)
		if b.budget != nil {
			b.budget.Release(cost)
		}
	}

	reader, selected, fallback, err := b.openCandidate(ctx, target)
	if err != nil && errors.Is(err, errBackendUnsupported) && !b.cfg.Strict {
		fallback = err.Error()
		reader, err = b.platform.openFile(target, b.fallbackNamespace())
		selected = FileScanBackendBaseline
	}
	if err != nil {
		release()
		return nil, err
	}
	backendGauge, backendBytes := fileScanBackendResource(selected, b.cfg)
	if backendBytes > 0 {
		b.cfg.Metrics.AddGauge(backendGauge, backendBytes)
	}

	return &openedContent{
		Reader:   reader,
		Backend:  selected,
		Fallback: fallback,
		close: func() error {
			err := reader.Close()
			if backendBytes > 0 {
				b.cfg.Metrics.AddGauge(backendGauge, -backendBytes)
			}
			release()
			return err
		},
	}, nil
}

func fileScanBackendResource(backend FileScanBackend, cfg FileScanConfig) (string, int64) {
	switch backend {
	case FileScanBackendMmap:
		return FileScanGaugeMappedBytes, cfg.MmapWindowBytes
	default:
		return "", 0
	}
}

func fileScanSessionMemoryCost(cfg FileScanConfig) int64 {
	cfg = cfg.Normalize()
	cost := int64(defaultBufferSize + 4096)
	switch cfg.Backend {
	case FileScanBackendBuffered:
		cost += int64(cfg.PrefetchBytes)
	case FileScanBackendDirect:
		cost += fileScanRoundedBufferBytes(max(cfg.PrefetchBytes, defaultBufferSize))
	case FileScanBackendMmap:
		cost += cfg.MmapWindowBytes
	}
	return cost
}

func (b *contentBackend) fallbackNamespace() FileScanNamespace {
	if b.cfg.Namespace == FileScanNamespaceUringOpenat {
		return FileScanNamespaceOpenat
	}
	return b.cfg.Namespace
}

func (b *contentBackend) openCandidate(
	ctx context.Context,
	target ScanTarget,
) (io.ReadCloser, FileScanBackend, string, error) {
	if b.cfg.Backend != FileScanBackendBaseline && requiresRandomAccess(ctx, target.Path) {
		file, err := b.platform.openFile(target, b.fallbackNamespace())
		// ZIP and 7z require ReaderAt and Seeker. Selecting the production
		// os.File route for them is part of the candidate design, not an
		// unsupported-backend fallback.
		return file, FileScanBackendBaseline, "", err
	}

	switch b.cfg.Backend {
	case FileScanBackendBaseline:
		file, err := b.platform.openFile(target, b.cfg.Namespace)
		return file, FileScanBackendBaseline, "", err
	case FileScanBackendBuffered:
		file, err := b.platform.openFile(target, b.cfg.Namespace)
		if err != nil {
			return nil, b.cfg.Backend, "", err
		}
		if err := b.platform.advise(file, b.cfg); err != nil {
			_ = file.Close()
			return nil, b.cfg.Backend, "", &backendUnsupportedError{reason: "fadvise", err: err}
		}
		reader := newPrefetchReader(file, b.cfg.PrefetchBytes, b.cfg.ReadMode)
		return reader, FileScanBackendBuffered, "", nil
	case FileScanBackendMmap:
		reader, err := b.platform.openMmap(ctx, target, b.cfg)
		return reader, FileScanBackendMmap, "", err
	case FileScanBackendDirect:
		reader, err := b.platform.openDirect(ctx, target, b.cfg)
		return reader, FileScanBackendDirect, "", err
	case FileScanBackendUring:
		reader, err := b.platform.openUring(ctx, target, b.cfg, false)
		return reader, FileScanBackendUring, "", err
	case FileScanBackendUringDirect:
		reader, err := b.platform.openUring(ctx, target, b.cfg, true)
		return reader, FileScanBackendUringDirect, "", err
	default:
		return nil, b.cfg.Backend, "", fmt.Errorf("unknown file scan backend %q", b.cfg.Backend)
	}
}

func requiresRandomAccess(ctx context.Context, path string) bool {
	format, _, err := archives.Identify(ctx, path, nil)
	if err != nil {
		return false
	}
	switch format.(type) {
	case archives.SevenZip, archives.Zip:
		return true
	default:
		return false
	}
}

type prefetchReader struct {
	file       fileScanReadAtCloser
	buf        []byte
	start      int
	end        int
	offset     int64
	pread      bool
	eof        bool
	pendingErr error
}

type fileScanReadAtCloser interface {
	io.Reader
	io.ReaderAt
	io.Closer
}

func newPrefetchReader(file *os.File, size int, mode string) *prefetchReader {
	return &prefetchReader{
		file:  file,
		buf:   make([]byte, size),
		pread: mode == "pread",
	}
}

func (r *prefetchReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	total := 0
	for total < len(p) {
		if r.start == r.end {
			if r.eof {
				break
			}
			if err := r.fill(); err != nil {
				if errors.Is(err, io.EOF) {
					r.eof = true
					break
				}
				if total > 0 {
					// Preserve a read error observed after bytes already copied;
					// Reader permits returning the bytes first, but the next call
					// must still observe the error.
					r.pendingErr = err
					return total, nil
				}
				return 0, err
			}
		}
		n := copy(p[total:], r.buf[r.start:r.end])
		r.start += n
		total += n
	}
	if total > 0 {
		return total, nil
	}
	return 0, io.EOF
}

func (r *prefetchReader) fill() error {
	if r.pendingErr != nil {
		err := r.pendingErr
		r.pendingErr = nil
		return err
	}
	r.start = 0
	r.end = 0
	var (
		n   int
		err error
	)
	if r.pread {
		n, err = r.file.ReadAt(r.buf, r.offset)
	} else {
		n, err = r.file.Read(r.buf)
	}
	r.offset += int64(n)
	r.end = n
	if n > 0 {
		r.pendingErr = err
		return nil
	}
	return err
}

func (r *prefetchReader) Close() error {
	return r.file.Close()
}
