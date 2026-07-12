//go:build linux

package sources

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

type uringProvider interface {
	openFile(context.Context, ScanTarget, int) (*os.File, error)
	openReader(context.Context, ScanTarget, FileScanConfig, bool) (io.ReadCloser, error)
	close() error
}

type linuxPlatformContentBackend struct {
	ctx      context.Context
	rootPath string
	rootFD   int
	uring    uringProvider
}

func newPlatformContentBackend(
	ctx context.Context,
	root string,
	cfg FileScanConfig,
) (platformContentBackend, error) {
	rootPath, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve file scan root: %w", err)
	}
	info, err := os.Stat(rootPath)
	if err != nil {
		return nil, fmt.Errorf("stat file scan root: %w", err)
	}
	if !info.IsDir() {
		rootPath = filepath.Dir(rootPath)
	}

	p := &linuxPlatformContentBackend{
		ctx:      ctx,
		rootPath: rootPath,
		rootFD:   -1,
	}
	if cfg.Namespace == FileScanNamespaceOpenat || cfg.Namespace == FileScanNamespaceUringOpenat {
		p.rootFD, err = unix.Open(rootPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, fmt.Errorf("open file scan root: %w", err)
		}
	}
	if cfg.Backend == FileScanBackendUring ||
		cfg.Backend == FileScanBackendUringDirect ||
		cfg.Namespace == FileScanNamespaceUringOpenat {
		p.uring, err = newUringProvider(ctx, p.rootFD, p.rootPath, cfg)
		if err != nil && cfg.Strict {
			_ = p.close()
			return nil, err
		}
	}
	return p, nil
}

func (p *linuxPlatformContentBackend) close() error {
	var errs []error
	if p.uring != nil {
		errs = append(errs, p.uring.close())
		p.uring = nil
	}
	if p.rootFD >= 0 {
		errs = append(errs, unix.Close(p.rootFD))
		p.rootFD = -1
	}
	return errors.Join(errs...)
}

func (p *linuxPlatformContentBackend) openFile(
	target ScanTarget,
	namespace FileScanNamespace,
) (*os.File, error) {
	if namespace == FileScanNamespaceUringOpenat {
		if p.uring == nil {
			return nil, &backendUnsupportedError{reason: "io_uring openat unavailable"}
		}
		return p.uring.openFile(p.ctx, target, unix.O_RDONLY|unix.O_CLOEXEC)
	}
	fd, err := p.openFD(target, namespace, unix.O_RDONLY|unix.O_CLOEXEC)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), target.Path), nil
}

func (p *linuxPlatformContentBackend) openFD(
	target ScanTarget,
	namespace FileScanNamespace,
	flags int,
) (int, error) {
	if namespace != FileScanNamespaceOpenat {
		return unix.Open(target.Path, flags, 0)
	}
	if p.rootFD < 0 {
		return -1, &backendUnsupportedError{reason: "openat root unavailable"}
	}
	abs, err := filepath.Abs(target.Path)
	if err != nil {
		return -1, err
	}
	rel, err := filepath.Rel(p.rootPath, abs)
	if err != nil {
		return -1, err
	}
	return unix.Openat(p.rootFD, rel, flags, 0)
}

func (p *linuxPlatformContentBackend) advise(file *os.File, cfg FileScanConfig) error {
	for _, advice := range cfg.Advice {
		var value int
		switch FileScanAdvice(advice) {
		case FileScanAdviceFadvSequential:
			value = unix.FADV_SEQUENTIAL
		case FileScanAdviceFadvNoReuse:
			value = unix.FADV_NOREUSE
		case FileScanAdviceFadvWillNeed:
			value = unix.FADV_WILLNEED
		default:
			return fmt.Errorf("unknown fadvise mode %q", advice)
		}
		length := int64(0)
		if FileScanAdvice(advice) == FileScanAdviceFadvWillNeed {
			length = int64(cfg.PrefetchBytes)
		}
		if err := unix.Fadvise(int(file.Fd()), 0, length, value); err != nil {
			return err
		}
	}
	return nil
}

func (p *linuxPlatformContentBackend) openDirect(
	_ context.Context,
	target ScanTarget,
	cfg FileScanConfig,
) (io.ReadCloser, error) {
	if cfg.Namespace == FileScanNamespaceUringOpenat {
		if p.uring == nil {
			return nil, &backendUnsupportedError{reason: "io_uring openat unavailable"}
		}
		file, err := p.uring.openFile(p.ctx, target, unix.O_RDONLY|unix.O_DIRECT|unix.O_CLOEXEC)
		if err != nil {
			return nil, err
		}
		return newDirectReader(int(file.Fd()), target.Path, file, cfg.PrefetchBytes)
	}
	fd, err := p.openFD(target, cfg.Namespace, unix.O_RDONLY|unix.O_DIRECT|unix.O_CLOEXEC)
	if err != nil {
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil, &backendUnsupportedError{reason: "O_DIRECT open", err: err}
		}
		return nil, err
	}
	return newDirectReader(fd, target.Path, nil, cfg.PrefetchBytes)
}

func newDirectReader(fd int, path string, file *os.File, requested int) (io.ReadCloser, error) {
	closeFD := func() {
		if file != nil {
			_ = file.Close()
		} else {
			_ = unix.Close(fd)
		}
	}
	var stat unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_SIZE|unix.STATX_DIOALIGN, &stat); err != nil {
		closeFD()
		return nil, &backendUnsupportedError{reason: "STATX_DIOALIGN", err: err}
	}
	if stat.Mask&unix.STATX_DIOALIGN == 0 || stat.Dio_mem_align == 0 {
		closeFD()
		return nil, &backendUnsupportedError{reason: "STATX_DIOALIGN unavailable"}
	}
	offsetAlign := stat.Dio_read_offset_align
	if offsetAlign == 0 {
		offsetAlign = stat.Dio_offset_align
	}
	if offsetAlign == 0 {
		closeFD()
		return nil, &backendUnsupportedError{reason: "direct read offset alignment unavailable"}
	}
	if !directAlignmentDividesPage(stat.Dio_mem_align) {
		closeFD()
		return nil, &backendUnsupportedError{reason: "direct memory alignment exceeds page alignment"}
	}
	if !directAlignmentDividesPage(offsetAlign) {
		closeFD()
		return nil, &backendUnsupportedError{reason: "direct read offset alignment exceeds page alignment"}
	}
	chunk, err := roundUp(max(requested, defaultBufferSize), int(offsetAlign))
	if err != nil {
		closeFD()
		return nil, err
	}
	raw, aligned, err := mmapAligned(chunk, int(stat.Dio_mem_align))
	if err != nil {
		closeFD()
		return nil, err
	}
	return &directReader{
		fd:          fd,
		file:        file,
		path:        path,
		raw:         raw,
		buf:         aligned,
		size:        int64(stat.Size),
		offsetAlign: int64(offsetAlign),
	}, nil
}

func directAlignmentDividesPage(alignment uint32) bool {
	pageSize := os.Getpagesize()
	return alignment > 0 &&
		uint64(alignment) <= uint64(pageSize) &&
		pageSize%int(alignment) == 0
}

func roundUp(value, alignment int) (int, error) {
	if value <= 0 || alignment <= 0 {
		return 0, fmt.Errorf("invalid alignment value=%d alignment=%d", value, alignment)
	}
	rem := value % alignment
	if rem == 0 {
		return value, nil
	}
	if value > int(^uint(0)>>1)-(alignment-rem) {
		return 0, fmt.Errorf("aligned size overflow")
	}
	return value + alignment - rem, nil
}

func mmapAligned(size, alignment int) ([]byte, []byte, error) {
	pageSize := os.Getpagesize()
	if pageSize%alignment == 0 {
		rawSize, err := roundUp(size, pageSize)
		if err != nil {
			return nil, nil, err
		}
		raw, err := unix.Mmap(-1, 0, rawSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_ANONYMOUS)
		if err != nil {
			return nil, nil, err
		}
		return raw, raw[:size], nil
	}
	rawSize, err := roundUp(size+alignment-1, pageSize)
	if err != nil {
		return nil, nil, err
	}
	raw, err := unix.Mmap(-1, 0, rawSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_ANONYMOUS)
	if err != nil {
		return nil, nil, err
	}
	base := uintptr(unsafe.Pointer(unsafe.SliceData(raw)))
	shift := int((uintptr(alignment) - base%uintptr(alignment)) % uintptr(alignment))
	return raw, raw[shift : shift+size], nil
}

type directReader struct {
	fd          int
	file        *os.File
	path        string
	raw         []byte
	buf         []byte
	start       int
	end         int
	offset      int64
	size        int64
	offsetAlign int64
	eof         bool
	closed      bool
}

func (r *directReader) Read(p []byte) (int, error) {
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

func (r *directReader) fill() error {
	if r.offset >= r.size {
		return io.EOF
	}
	if r.offset%r.offsetAlign != 0 {
		return fmt.Errorf("direct read offset %d is not aligned to %d", r.offset, r.offsetAlign)
	}
	r.start = 0
	r.end = 0
	n, err := unix.Pread(r.fd, r.buf, r.offset)
	if err != nil {
		return fmt.Errorf("direct read %s at %d: %w", r.path, r.offset, err)
	}
	if n == 0 {
		return io.ErrUnexpectedEOF
	}
	remaining := r.size - r.offset
	if int64(n) > remaining {
		n = int(remaining)
	}
	r.offset += int64(n)
	r.end = n
	return nil
}

func (r *directReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	var errs []error
	if len(r.raw) > 0 {
		errs = append(errs, unix.Munmap(r.raw))
		r.raw = nil
		r.buf = nil
	}
	if r.file != nil {
		errs = append(errs, r.file.Close())
	} else if r.fd >= 0 {
		errs = append(errs, unix.Close(r.fd))
	}
	r.fd = -1
	return errors.Join(errs...)
}

func (p *linuxPlatformContentBackend) openMmap(
	ctx context.Context,
	target ScanTarget,
	cfg FileScanConfig,
) (io.ReadCloser, error) {
	file, err := p.openFile(target, cfg.Namespace)
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	cmd := exec.CommandContext(
		ctx,
		executable,
		"_filescan-mmap-helper",
		fmt.Sprintf("--window-bytes=%d", cfg.MmapWindowBytes),
		fmt.Sprintf("--sequential=%t", slices.Contains(cfg.Advice, string(FileScanAdviceMadvSequential))),
		"--fd=3",
		"--",
		target.Path,
	)
	// Passing the already-open descriptor preserves the exact inode selected by
	// enumeration and makes permission/open failures match the baseline before
	// any bytes are emitted. The helper contains SIGBUS without a path-reopen
	// race.
	cmd.ExtraFiles = []*os.File{file}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = file.Close()
		return nil, &backendUnsupportedError{reason: "start mmap helper", err: err}
	}
	_ = file.Close()
	return &mmapProcessReader{
		cmd:      cmd,
		stdout:   stdout,
		expected: target.Size,
	}, nil
}

type mmapProcessReader struct {
	cmd         *exec.Cmd
	stdout      io.ReadCloser
	expected    int64
	read        int64
	waitOnce    sync.Once
	waitErr     error
	terminalErr error
	eof         bool
	closed      bool
}

func (r *mmapProcessReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.eof {
		if r.terminalErr != nil {
			return 0, r.terminalErr
		}
		return 0, io.EOF
	}
	total := 0
	for total < len(p) {
		n, err := r.stdout.Read(p[total:])
		r.read += int64(n)
		total += n
		if err == nil {
			if n == 0 {
				r.eof = true
				r.terminalErr = io.ErrNoProgress
				break
			}
			continue
		}
		if !errors.Is(err, io.EOF) {
			r.eof = true
			r.terminalErr = err
			break
		}
		r.eof = true
		r.wait()
		switch {
		case r.waitErr != nil:
			r.terminalErr = fmt.Errorf("mmap helper: %w", r.waitErr)
		case r.read != r.expected:
			r.terminalErr = fmt.Errorf(
				"mmap helper emitted %d bytes, expected %d: %w",
				r.read,
				r.expected,
				io.ErrUnexpectedEOF,
			)
		}
		break
	}
	if total > 0 {
		return total, nil
	}
	if r.terminalErr != nil {
		return 0, r.terminalErr
	}
	return 0, io.EOF
}

func (r *mmapProcessReader) wait() {
	r.waitOnce.Do(func() {
		r.waitErr = r.cmd.Wait()
	})
}

func (r *mmapProcessReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	_ = r.stdout.Close()
	if r.read < r.expected && r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
		r.wait()
		return nil
	}
	r.wait()
	return r.waitErr
}

func (p *linuxPlatformContentBackend) openUring(
	ctx context.Context,
	target ScanTarget,
	cfg FileScanConfig,
	direct bool,
) (io.ReadCloser, error) {
	if p.uring == nil {
		return nil, &backendUnsupportedError{reason: "io_uring build tag or runtime support unavailable"}
	}
	return p.uring.openReader(ctx, target, cfg, direct)
}

// RunFileScanMmapHelper streams a file through bounded sequential mmap windows.
// It is intended only for the hidden crash-containment helper subprocess.
func RunFileScanMmapHelper(
	ctx context.Context,
	path string,
	windowBytes int64,
	sequential bool,
	dst io.Writer,
) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return RunFileScanMmapHelperFile(ctx, path, file, windowBytes, sequential, dst)
}

// RunFileScanMmapHelperFile streams an already-open regular file through
// bounded sequential mmap windows. The caller owns file and must keep it open
// until this function returns.
func RunFileScanMmapHelperFile(
	ctx context.Context,
	path string,
	file *os.File,
	windowBytes int64,
	sequential bool,
	dst io.Writer,
) error {
	if file == nil {
		return os.ErrInvalid
	}
	if windowBytes <= 0 || windowBytes%int64(os.Getpagesize()) != 0 {
		return fmt.Errorf("mmap window must be a positive page-size multiple")
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("mmap helper requires a regular file")
	}

	for offset := int64(0); offset < info.Size(); {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		length := min(windowBytes, info.Size()-offset)
		window, err := unix.Mmap(int(file.Fd()), offset, int(length), unix.PROT_READ, unix.MAP_PRIVATE)
		if err != nil {
			return fmt.Errorf("mmap %s at %d: %w", path, offset, err)
		}
		if sequential {
			if err := unix.Madvise(window, unix.MADV_SEQUENTIAL); err != nil {
				_ = unix.Munmap(window)
				return fmt.Errorf("madvise %s at %d: %w", path, offset, err)
			}
		}
		remaining := window
		for len(remaining) > 0 {
			n, writeErr := dst.Write(remaining)
			if writeErr != nil {
				_ = unix.Munmap(window)
				return writeErr
			}
			if n == 0 {
				_ = unix.Munmap(window)
				return io.ErrShortWrite
			}
			remaining = remaining[n:]
		}
		if err := unix.Madvise(window, unix.MADV_DONTNEED); err != nil {
			_ = unix.Munmap(window)
			return err
		}
		if err := unix.Munmap(window); err != nil {
			return err
		}
		offset += length
	}
	return nil
}
