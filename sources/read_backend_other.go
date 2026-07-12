//go:build !linux

package sources

import (
	"context"
	"io"
	"os"
)

type portablePlatformContentBackend struct{}

func newPlatformContentBackend(
	_ context.Context,
	_ string,
	_ FileScanConfig,
) (platformContentBackend, error) {
	return &portablePlatformContentBackend{}, nil
}

func (p *portablePlatformContentBackend) openFile(
	target ScanTarget,
	namespace FileScanNamespace,
) (*os.File, error) {
	if namespace == FileScanNamespaceOpenat || namespace == FileScanNamespaceUringOpenat {
		return nil, &backendUnsupportedError{reason: "openat is only available on Linux"}
	}
	return os.Open(target.Path)
}

func (p *portablePlatformContentBackend) advise(_ *os.File, cfg FileScanConfig) error {
	if len(cfg.Advice) > 0 {
		return &backendUnsupportedError{reason: "fadvise is only available on Linux"}
	}
	return nil
}

func (p *portablePlatformContentBackend) openDirect(
	context.Context,
	ScanTarget,
	FileScanConfig,
) (io.ReadCloser, error) {
	return nil, &backendUnsupportedError{reason: "direct I/O is only available on Linux"}
}

func (p *portablePlatformContentBackend) openMmap(
	context.Context,
	ScanTarget,
	FileScanConfig,
) (io.ReadCloser, error) {
	return nil, &backendUnsupportedError{reason: "mmap helper is only available on Linux"}
}

func (p *portablePlatformContentBackend) openUring(
	context.Context,
	ScanTarget,
	FileScanConfig,
	bool,
) (io.ReadCloser, error) {
	return nil, &backendUnsupportedError{reason: "io_uring is only available on Linux"}
}

func (p *portablePlatformContentBackend) close() error { return nil }

// RunFileScanMmapHelper reports that the helper is unavailable on this platform.
func RunFileScanMmapHelper(context.Context, string, int64, bool, io.Writer) error {
	return &backendUnsupportedError{reason: "mmap helper is only available on Linux"}
}

// RunFileScanMmapHelperFile reports that the descriptor-based helper is
// unavailable on this platform.
func RunFileScanMmapHelperFile(context.Context, string, *os.File, int64, bool, io.Writer) error {
	return &backendUnsupportedError{reason: "mmap helper is only available on Linux"}
}
