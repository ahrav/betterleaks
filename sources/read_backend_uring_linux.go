//go:build linux && filescan_uring

package sources

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	ioUringOffSQRing int64 = 0
	ioUringOffCQRing int64 = 0x08000000
	ioUringOffSQEs   int64 = 0x10000000

	ioUringFeatSingleMmap uint32 = 1 << 0
	ioUringFeatNoDrop     uint32 = 1 << 1

	ioUringRegisterBuffers   uint32 = 0
	ioUringUnregisterBuffers uint32 = 1
	ioUringRegisterEventfd   uint32 = 4
	ioUringUnregisterEventfd uint32 = 5
	ioUringRegisterProbe     uint32 = 8

	ioUringOpReadFixed   uint8 = 4
	ioUringOpAsyncCancel uint8 = 14
	ioUringOpOpenat      uint8 = 18
	ioUringOpClose       uint8 = 19
	ioUringOpStatx       uint8 = 21
	ioUringOpRead        uint8 = 22

	ioUringOpSupported uint16 = 1

	uringGaugeSubmittedBytes           = "submitted_bytes"
	uringGaugeCompletedUnconsumedBytes = "completed_unconsumed_bytes"
	uringGaugeReorderDepth             = "reorder_depth"
)

var errUringProviderClosed = errors.New("io_uring provider closed")

func isUringRuntimeUnavailable(err error) bool {
	return errors.Is(err, unix.EPERM) ||
		errors.Is(err, unix.EACCES) ||
		errors.Is(err, unix.ENOSYS) ||
		errors.Is(err, unix.EOPNOTSUPP)
}

func isUringOperationUnsupported(err error) bool {
	return errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP)
}

// These definitions mirror the fixed-width Linux UAPI. Keeping them local
// avoids making either of the general filesystem scanner builds depend on an
// io_uring wrapper library.
type ioUringSQE struct {
	Opcode      uint8
	Flags       uint8
	IOPrio      uint16
	FD          int32
	Off         uint64
	Addr        uint64
	Len         uint32
	OpFlags     uint32
	UserData    uint64
	BufIndex    uint16
	Personality uint16
	SpliceFDIn  int32
	Addr3       uint64
	Pad2        uint64
}

type ioUringCQE struct {
	UserData uint64
	Res      int32
	Flags    uint32
}

type ioSqringOffsets struct {
	Head        uint32
	Tail        uint32
	RingMask    uint32
	RingEntries uint32
	Flags       uint32
	Dropped     uint32
	Array       uint32
	Resv1       uint32
	UserAddr    uint64
}

type ioCqringOffsets struct {
	Head        uint32
	Tail        uint32
	RingMask    uint32
	RingEntries uint32
	Overflow    uint32
	CQEs        uint32
	Flags       uint32
	Resv1       uint32
	UserAddr    uint64
}

type ioUringParams struct {
	SQEntries    uint32
	CQEntries    uint32
	Flags        uint32
	SQThreadCPU  uint32
	SQThreadIdle uint32
	Features     uint32
	WQFD         uint32
	Resv         [3]uint32
	SQOff        ioSqringOffsets
	CQOff        ioCqringOffsets
}

type ioUringProbeOp struct {
	Op    uint8
	Resv  uint8
	Flags uint16
	Resv2 uint32
}

type ioUringProbe64 struct {
	LastOp uint8
	OpsLen uint8
	Resv   uint16
	Resv2  [3]uint32
	Ops    [64]ioUringProbeOp
}

// Paired assertions reject both undersized and oversized UAPI layouts.
var (
	_ [64 - unsafe.Sizeof(ioUringSQE{})]byte
	_ [unsafe.Sizeof(ioUringSQE{}) - 64]byte
	_ [16 - unsafe.Sizeof(ioUringCQE{})]byte
	_ [unsafe.Sizeof(ioUringCQE{}) - 16]byte
	_ [40 - unsafe.Sizeof(ioSqringOffsets{})]byte
	_ [unsafe.Sizeof(ioSqringOffsets{}) - 40]byte
	_ [40 - unsafe.Sizeof(ioCqringOffsets{})]byte
	_ [unsafe.Sizeof(ioCqringOffsets{}) - 40]byte
	_ [120 - unsafe.Sizeof(ioUringParams{})]byte
	_ [unsafe.Sizeof(ioUringParams{}) - 120]byte
	_ [8 - unsafe.Sizeof(ioUringProbeOp{})]byte
	_ [unsafe.Sizeof(ioUringProbeOp{}) - 8]byte
	_ [40 - unsafe.Offsetof(ioUringSQE{}.BufIndex)]byte
	_ [unsafe.Offsetof(ioUringSQE{}.BufIndex) - 40]byte
	_ [40 - unsafe.Offsetof(ioUringParams{}.SQOff)]byte
	_ [unsafe.Offsetof(ioUringParams{}.SQOff) - 40]byte
	_ [80 - unsafe.Offsetof(ioUringParams{}.CQOff)]byte
	_ [unsafe.Offsetof(ioUringParams{}.CQOff) - 80]byte
	_ [16 - unsafe.Offsetof(ioUringProbe64{}.Ops)]byte
	_ [unsafe.Offsetof(ioUringProbe64{}.Ops) - 16]byte
)

type rawUring struct {
	fd int

	sqRingMap []byte
	cqRingMap []byte
	sqesMap   []byte

	sqHead    *uint32
	sqTail    *uint32
	sqMask    uint32
	sqEntries uint32
	sqDropped *uint32
	sqArray   []uint32
	sqes      []ioUringSQE

	cqHead     *uint32
	cqTail     *uint32
	cqMask     uint32
	cqEntries  uint32
	cqOverflow *uint32
	cqes       []ioUringCQE

	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

type uringResult struct {
	res int32
	err error
}

type uringFuture struct {
	id        uint64
	done      chan uringResult
	submitted atomic.Bool
	once      sync.Once
}

func (f *uringFuture) complete(result uringResult) {
	f.once.Do(func() { f.done <- result })
}

type uringCommand struct {
	future *uringFuture
	sqe    ioUringSQE

	ordinary         bool
	control          bool
	releaseAdmission func()
	readChunk        *uringChunk
	readRequested    int64
	readLogical      int
	queueGauged      bool
	bytesGauged      bool

	path   []byte
	statx  *unix.Statx_t
	pinner runtime.Pinner
	pinned bool

	finishOnce sync.Once
}

func (c *uringCommand) finish(result uringResult) {
	c.finishOnce.Do(func() {
		if c.pinned {
			c.pinner.Unpin()
			c.pinned = false
		}
		c.path = nil
		c.statx = nil
		if c.releaseAdmission != nil {
			c.releaseAdmission()
			c.releaseAdmission = nil
		}
		c.future.complete(result)
	})
}

type uringBuffer struct {
	index uint16
	data  []byte
}

type rawUringProvider struct {
	ring     *rawUring
	rootFD   int
	rootPath string
	depth    int
	bufSize  int

	registered  bool
	bufferDepth int
	bufferPool  chan *uringBuffer
	bufferArena []byte

	wakeFD           int
	nextID           atomic.Uint64
	admission        chan struct{}
	controlAdmission chan struct{}

	queueMu   sync.Mutex
	accepting bool
	shutdown  bool
	queue     []*uringCommand

	ownerDone  chan struct{}
	ownerErrMu sync.Mutex
	ownerErr   error

	readersMu      sync.Mutex
	closingReaders bool
	readers        map[*uringReader]struct{}

	metrics     *FileScanMetrics
	pinnedBytes int64

	closeOnce sync.Once
	closeErr  error
}

func newUringProvider(
	_ context.Context,
	rootFD int,
	rootPath string,
	cfg FileScanConfig,
) (uringProvider, error) {
	contentUring := isFileScanUringBackend(cfg.Backend)
	if cfg.QueueDepth <= 0 || uint64(cfg.QueueDepth) > math.MaxUint32 {
		return nil, &backendUnsupportedError{reason: "invalid io_uring queue depth"}
	}
	if contentUring && (cfg.PrefetchBytes <= 0 || uint64(cfg.PrefetchBytes) > math.MaxUint32) {
		return nil, &backendUnsupportedError{reason: "invalid io_uring buffer size"}
	}
	if cfg.RegisteredBuffers && cfg.QueueDepth > math.MaxUint16 {
		return nil, &backendUnsupportedError{reason: "registered io_uring queue depth exceeds buffer-index range"}
	}
	if cfg.RegisteredBuffers && int64(cfg.QueueDepth) > math.MaxInt64/int64(cfg.PrefetchBytes) {
		return nil, &backendUnsupportedError{reason: "registered io_uring byte count overflow"}
	}
	if cfg.RegisteredBuffers && cfg.MaxInFlightBytes > 0 &&
		int64(cfg.QueueDepth)*int64(cfg.PrefetchBytes) > cfg.MaxInFlightBytes {
		return nil, &backendUnsupportedError{reason: "registered io_uring buffers exceed the byte budget"}
	}

	ring, err := newRawUring(uint32(cfg.QueueDepth))
	if err != nil {
		return nil, &backendUnsupportedError{reason: "io_uring setup", err: err}
	}
	fail := func(reason string, cause error) (uringProvider, error) {
		ring.unmap()
		_ = ring.close()
		return nil, &backendUnsupportedError{reason: reason, err: cause}
	}
	if ring.sqEntries < uint32(cfg.QueueDepth) {
		return fail("io_uring queue depth was clamped", nil)
	}
	if uint64(ring.cqEntries) < 2*uint64(cfg.QueueDepth) {
		return fail("io_uring completion ring cannot cover ordinary and control lanes", nil)
	}
	if err := requireUringOperations(ring, cfg); err != nil {
		return fail("probe io_uring operations", err)
	}

	wakeFD, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		return fail("create io_uring eventfd", err)
	}
	wakeFD32 := int32(wakeFD)
	if err := ring.register(ioUringRegisterEventfd, unsafe.Pointer(&wakeFD32), 1); err != nil {
		_ = unix.Close(wakeFD)
		return fail("register io_uring eventfd", err)
	}

	p := &rawUringProvider{
		ring:      ring,
		rootFD:    rootFD,
		rootPath:  rootPath,
		depth:     cfg.QueueDepth,
		bufSize:   cfg.PrefetchBytes,
		wakeFD:    wakeFD,
		accepting: true,
		ownerDone: make(chan struct{}),
		readers:   make(map[*uringReader]struct{}),
		metrics:   cfg.Metrics,
	}
	p.admission = make(chan struct{}, p.depth)
	p.controlAdmission = make(chan struct{}, p.depth)
	for range p.depth {
		p.admission <- struct{}{}
		p.controlAdmission <- struct{}{}
	}
	if contentUring {
		if err := p.initBuffers(cfg); err != nil {
			_ = ring.register(ioUringUnregisterEventfd, nil, 0)
			_ = unix.Close(wakeFD)
			ring.unmap()
			_ = ring.close()
			return nil, &backendUnsupportedError{reason: "allocate io_uring buffers", err: err}
		}
	}
	if cfg.RegisteredBuffers {
		p.pinnedBytes = int64(len(p.bufferArena))
		p.metrics.AddGauge(FileScanGaugePinnedBytes, p.pinnedBytes)
	}

	go runLockedUringOwner(p.runOwner)
	return p, nil
}

func runLockedUringOwner(run func()) {
	// io_uring task work belongs to the submitting task. Keep the sole submitter
	// on one OS thread until every queued and pending request has been retired.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	run()
}

func requireUringOperations(ring *rawUring, cfg FileScanConfig) error {
	var probe ioUringProbe64
	if err := ring.register(ioUringRegisterProbe, unsafe.Pointer(&probe), uint32(len(probe.Ops))); err != nil {
		return err
	}
	supported := make(map[uint8]bool, min(int(probe.OpsLen), len(probe.Ops)))
	for i := 0; i < min(int(probe.OpsLen), len(probe.Ops)); i++ {
		op := probe.Ops[i]
		if op.Flags&ioUringOpSupported != 0 {
			supported[op.Op] = true
		}
	}
	required := []uint8{ioUringOpAsyncCancel, ioUringOpClose, ioUringOpStatx}
	if isFileScanUringBackend(cfg.Backend) {
		required = append(required, ioUringOpRead)
		if cfg.RegisteredBuffers {
			required = append(required, ioUringOpReadFixed)
		}
	}
	if cfg.Namespace == FileScanNamespaceUringOpenat {
		required = append(required, ioUringOpOpenat)
	}
	for _, opcode := range required {
		if !supported[opcode] {
			return fmt.Errorf("required opcode %d is unavailable", opcode)
		}
	}
	return nil
}

func (p *rawUringProvider) initBuffers(cfg FileScanConfig) error {
	p.bufferDepth = p.depth
	pageSize := os.Getpagesize()
	slotSize, err := roundUp(p.bufSize, pageSize)
	if err != nil {
		return err
	}
	if p.bufferDepth > math.MaxInt/slotSize {
		return fmt.Errorf("io_uring buffer arena size overflow")
	}
	arenaBytes := p.bufferDepth * slotSize
	if cfg.MaxInFlightBytes > 0 && int64(arenaBytes) > cfg.MaxInFlightBytes {
		return fmt.Errorf("page-rounded io_uring buffer arena exceeds the byte budget")
	}
	arena, err := unix.Mmap(
		-1,
		0,
		arenaBytes,
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_SHARED|unix.MAP_ANONYMOUS,
	)
	if err != nil {
		return err
	}
	p.bufferArena = arena
	p.bufferPool = make(chan *uringBuffer, p.bufferDepth)
	iovecs := make([]unix.Iovec, p.bufferDepth)
	for i := range p.bufferDepth {
		start := i * slotSize
		buf := &uringBuffer{
			index: uint16(i),
			data:  arena[start : start+p.bufSize],
		}
		iovecs[i].Base = unsafe.SliceData(buf.data)
		iovecs[i].SetLen(len(buf.data))
		p.bufferPool <- buf
	}
	if !cfg.RegisteredBuffers {
		return nil
	}
	if err := p.ring.register(
		ioUringRegisterBuffers,
		unsafe.Pointer(unsafe.SliceData(iovecs)),
		uint32(len(iovecs)),
	); err != nil {
		p.releaseBuffers()
		return err
	}
	p.registered = true
	return nil
}

func (p *rawUringProvider) acquireBuffer(ctx context.Context) (*uringBuffer, error) {
	select {
	case buffer := <-p.bufferPool:
		return buffer, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.ownerDone:
		return nil, p.providerError()
	}
}

func (p *rawUringProvider) releaseBuffer(buffer *uringBuffer) {
	if buffer == nil {
		return
	}
	p.bufferPool <- buffer
}

func (p *rawUringProvider) releaseBuffers() {
	if len(p.bufferArena) > 0 {
		_ = unix.Munmap(p.bufferArena)
		p.bufferArena = nil
	}
	p.bufferPool = nil
}

func (p *rawUringProvider) addReader(reader *uringReader) error {
	p.readersMu.Lock()
	defer p.readersMu.Unlock()
	if p.closingReaders {
		return errUringProviderClosed
	}
	p.readers[reader] = struct{}{}
	return nil
}

func (p *rawUringProvider) removeReader(reader *uringReader) {
	p.readersMu.Lock()
	delete(p.readers, reader)
	p.readersMu.Unlock()
}

func (p *rawUringProvider) beginReaderShutdown() []*uringReader {
	p.readersMu.Lock()
	p.closingReaders = true
	readers := make([]*uringReader, 0, len(p.readers))
	for reader := range p.readers {
		readers = append(readers, reader)
	}
	p.readersMu.Unlock()
	return readers
}

func (p *rawUringProvider) openFile(
	ctx context.Context,
	target ScanTarget,
	flags int,
) (*os.File, error) {
	fd, err := p.openFD(ctx, target, flags)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), target.Path), nil
}

func (p *rawUringProvider) openFD(
	ctx context.Context,
	target ScanTarget,
	flags int,
) (int, error) {
	dirFD, path, err := p.resolveOpen(target)
	if err != nil {
		return -1, err
	}
	future, err := p.submitOpen(ctx, dirFD, path, flags)
	if err != nil {
		return -1, err
	}
	result, waitErr := p.waitFuture(ctx, future)
	if waitErr != nil {
		if result.err == nil && result.res >= 0 {
			_ = p.closeOwnedFD(int(result.res))
		}
		return -1, waitErr
	}
	if result.err != nil {
		return -1, result.err
	}
	if result.res < 0 {
		err := syscall.Errno(-result.res)
		if errors.Is(err, unix.EINVAL) || isUringOperationUnsupported(err) {
			return -1, &backendUnsupportedError{reason: "io_uring openat", err: err}
		}
		// Match unix.Open/Openat error semantics used by every other namespace
		// arm; syscall labels are diagnostics, not observable scan differences.
		return -1, err
	}
	return int(result.res), nil
}

func (p *rawUringProvider) resolveOpen(target ScanTarget) (int, string, error) {
	dirFD := unix.AT_FDCWD
	path := target.Path
	if p.rootFD >= 0 {
		abs, err := filepath.Abs(target.Path)
		if err != nil {
			return -1, "", err
		}
		path, err = filepath.Rel(p.rootPath, abs)
		if err != nil {
			return -1, "", err
		}
		dirFD = p.rootFD
	}
	if strings.IndexByte(path, 0) >= 0 {
		return -1, "", fmt.Errorf("io_uring openat path contains NUL")
	}
	return dirFD, path, nil
}

func (p *rawUringProvider) openReader(
	ctx context.Context,
	target ScanTarget,
	cfg FileScanConfig,
	direct bool,
) (io.ReadCloser, error) {
	flags := unix.O_RDONLY | unix.O_CLOEXEC
	if direct {
		flags |= unix.O_DIRECT
	}
	var fd int
	var err error
	if cfg.Namespace == FileScanNamespaceUringOpenat {
		fd, err = p.openFD(ctx, target, flags)
	} else {
		fd, err = unix.Open(target.Path, flags, 0)
	}
	if err != nil {
		if direct && (errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP)) {
			return nil, &backendUnsupportedError{reason: "io_uring O_DIRECT open", err: err}
		}
		return nil, err
	}
	closeOnError := func(openErr error) (io.ReadCloser, error) {
		closeErr := p.closeOwnedFD(fd)
		if closeErr != nil {
			return nil, errors.Join(openErr, fmt.Errorf("close io_uring reader fd: %w", closeErr))
		}
		return nil, openErr
	}

	mask := unix.STATX_SIZE
	if direct {
		mask |= unix.STATX_DIOALIGN
	}
	stat, err := p.statFD(ctx, fd, mask)
	if err != nil {
		return closeOnError(err)
	}
	if stat.Size > math.MaxInt64 {
		return closeOnError(fmt.Errorf("file too large for io_uring reader: %q", target.Path))
	}

	chunkSize := p.bufSize
	if direct {
		if stat.Mask&unix.STATX_DIOALIGN == 0 || stat.Dio_mem_align == 0 {
			return closeOnError(&backendUnsupportedError{reason: "io_uring direct alignment unavailable"})
		}
		offsetAlign := stat.Dio_read_offset_align
		if offsetAlign == 0 {
			offsetAlign = stat.Dio_offset_align
		}
		if offsetAlign == 0 || uint64(offsetAlign) > uint64(math.MaxInt) {
			return closeOnError(&backendUnsupportedError{reason: "io_uring direct offset alignment unavailable"})
		}
		chunkSize, err = roundUp(chunkSize, int(offsetAlign))
		if err != nil {
			return closeOnError(err)
		}
		if chunkSize != p.bufSize || os.Getpagesize()%int(stat.Dio_mem_align) != 0 {
			return closeOnError(&backendUnsupportedError{reason: "io_uring buffers do not satisfy direct alignment"})
		}
	}

	perFileDepth := max(1, cfg.QueueDepth/max(1, cfg.ActiveFiles))
	perFileDepth = min(perFileDepth, p.bufferDepth)
	size := int64(stat.Size)
	totalChunks := size / int64(chunkSize)
	if size%int64(chunkSize) != 0 {
		totalChunks++
	}
	readerContext, cancelReader := context.WithCancel(ctx)
	r := &uringReader{
		ctx:         readerContext,
		cancel:      cancelReader,
		provider:    p,
		fd:          fd,
		size:        size,
		chunkSize:   chunkSize,
		direct:      direct,
		depth:       perFileDepth,
		pending:     make(map[int64]*uringChunk, perFileDepth),
		totalChunks: totalChunks,
	}
	r.mu.Lock()
	if err := p.addReader(r); err != nil {
		r.mu.Unlock()
		cancelReader()
		return closeOnError(err)
	}
	err = r.submitMore()
	r.mu.Unlock()
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}

func (p *rawUringProvider) submitOpen(
	ctx context.Context,
	dirFD int,
	path string,
	flags int,
) (*uringFuture, error) {
	command, err := p.newOrdinaryCommand(ctx, ioUringSQE{
		Opcode:  ioUringOpOpenat,
		FD:      int32(dirFD),
		Len:     0,
		OpFlags: uint32(flags),
	})
	if err != nil {
		return nil, err
	}
	command.path = append([]byte(path), 0)
	command.pinner.Pin(&command.path[0])
	command.pinned = true
	command.sqe.Addr = uint64(uintptr(unsafe.Pointer(unsafe.SliceData(command.path))))
	if err := p.enqueue(ctx, command); err != nil {
		command.finish(uringResult{err: err})
		return nil, err
	}
	return command.future, nil
}

func (p *rawUringProvider) submitRead(
	ctx context.Context,
	fd int,
	chunk *uringChunk,
	buffer *uringBuffer,
	start int,
	length int,
	logical int,
	offset uint64,
) (*uringFuture, error) {
	opcode := ioUringOpRead
	if p.registered {
		opcode = ioUringOpReadFixed
	}
	command, err := p.newOrdinaryCommand(ctx, ioUringSQE{
		Opcode:   opcode,
		FD:       int32(fd),
		Off:      offset,
		Addr:     uint64(uintptr(unsafe.Pointer(unsafe.SliceData(buffer.data[start:])))),
		Len:      uint32(length),
		BufIndex: buffer.index,
	})
	if err != nil {
		return nil, err
	}
	command.readRequested = int64(length)
	command.readLogical = logical
	command.readChunk = chunk
	if err := p.enqueue(ctx, command); err != nil {
		command.finish(uringResult{err: err})
		return nil, err
	}
	return command.future, nil
}

func (p *rawUringProvider) statFD(
	ctx context.Context,
	fd int,
	mask int,
) (*unix.Statx_t, error) {
	future, stat, err := p.submitStatx(ctx, fd, mask)
	if err != nil {
		return nil, err
	}
	result, waitErr := p.waitFuture(ctx, future)
	if waitErr != nil {
		return nil, waitErr
	}
	if result.err != nil {
		return nil, result.err
	}
	if result.res < 0 {
		err := syscall.Errno(-result.res)
		if isUringOperationUnsupported(err) || errors.Is(err, unix.EINVAL) {
			return nil, &backendUnsupportedError{reason: "io_uring statx", err: err}
		}
		return nil, os.NewSyscallError("io_uring statx", err)
	}
	if result.res != 0 {
		return nil, fmt.Errorf("io_uring statx returned unexpected result %d", result.res)
	}
	return stat, nil
}

func (p *rawUringProvider) submitStatx(
	ctx context.Context,
	fd int,
	mask int,
) (*uringFuture, *unix.Statx_t, error) {
	stat := new(unix.Statx_t)
	command, err := p.newOrdinaryCommand(ctx, ioUringSQE{
		Opcode:  ioUringOpStatx,
		FD:      int32(fd),
		Len:     uint32(mask),
		OpFlags: uint32(unix.AT_EMPTY_PATH),
	})
	if err != nil {
		return nil, nil, err
	}
	command.path = []byte{0}
	command.statx = stat
	command.pinner.Pin(&command.path[0])
	command.pinner.Pin(stat)
	command.pinned = true
	command.sqe.Addr = uint64(uintptr(unsafe.Pointer(unsafe.SliceData(command.path))))
	command.sqe.Off = uint64(uintptr(unsafe.Pointer(stat)))
	if err := p.enqueue(ctx, command); err != nil {
		command.finish(uringResult{err: err})
		return nil, nil, err
	}
	return command.future, stat, nil
}

func (p *rawUringProvider) submitCancel(target uint64) (*uringFuture, error) {
	if err := p.acquireControl(context.Background()); err != nil {
		return nil, err
	}
	command := p.newCancelCommand(target)
	p.bindControlAdmission(command)
	if err := p.enqueue(context.Background(), command); err != nil {
		command.finish(uringResult{err: err})
		return nil, err
	}
	return command.future, nil
}

func (p *rawUringProvider) newCancelCommand(target uint64) *uringCommand {
	command := p.newCommand(ioUringSQE{
		Opcode: ioUringOpAsyncCancel,
		FD:     -1,
		Addr:   target,
	})
	command.control = true
	return command
}

func (p *rawUringProvider) submitClose(fd int) (*uringFuture, error) {
	if err := p.acquireControl(context.Background()); err != nil {
		return nil, err
	}
	command := p.newCommand(ioUringSQE{Opcode: ioUringOpClose, FD: int32(fd)})
	command.control = true
	p.bindControlAdmission(command)
	if err := p.enqueue(context.Background(), command); err != nil {
		command.finish(uringResult{err: err})
		return nil, err
	}
	return command.future, nil
}

func (p *rawUringProvider) newCommand(sqe ioUringSQE) *uringCommand {
	id := p.nextID.Add(1)
	future := &uringFuture{id: id, done: make(chan uringResult, 1)}
	sqe.UserData = id
	return &uringCommand{future: future, sqe: sqe}
}

func (p *rawUringProvider) newOrdinaryCommand(
	ctx context.Context,
	sqe ioUringSQE,
) (*uringCommand, error) {
	select {
	case <-p.admission:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.ownerDone:
		return nil, p.providerError()
	}
	command := p.newCommand(sqe)
	command.ordinary = true
	command.releaseAdmission = func() { p.admission <- struct{}{} }
	return command, nil
}

func (p *rawUringProvider) acquireControl(ctx context.Context) error {
	select {
	case <-p.controlAdmission:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ownerDone:
		return p.providerError()
	}
}

func (p *rawUringProvider) bindControlAdmission(command *uringCommand) {
	command.releaseAdmission = func() { p.controlAdmission <- struct{}{} }
}

func (p *rawUringProvider) tryShutdownCancel(target uint64) (*uringCommand, bool) {
	select {
	case <-p.controlAdmission:
		command := p.newCancelCommand(target)
		p.bindControlAdmission(command)
		return command, true
	default:
		return nil, false
	}
}

func (p *rawUringProvider) enqueue(ctx context.Context, command *uringCommand) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.queueMu.Lock()
	if !p.accepting {
		p.queueMu.Unlock()
		return p.providerError()
	}
	p.queue = append(p.queue, command)
	p.queueMu.Unlock()
	p.wake()
	return nil
}

func (p *rawUringProvider) takeQueue() ([]*uringCommand, bool) {
	p.queueMu.Lock()
	defer p.queueMu.Unlock()
	queued := p.queue
	p.queue = nil
	return queued, p.shutdown
}

func (p *rawUringProvider) stopAccepting(shutdown bool) []*uringCommand {
	p.queueMu.Lock()
	defer p.queueMu.Unlock()
	p.accepting = false
	if shutdown {
		p.shutdown = true
	}
	queued := p.queue
	p.queue = nil
	return queued
}

func (p *rawUringProvider) wake() {
	var value [8]byte
	binary.NativeEndian.PutUint64(value[:], 1)
	for {
		_, err := unix.Write(p.wakeFD, value[:])
		if err == nil || errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EBADF) {
			return
		}
		if !errors.Is(err, unix.EINTR) {
			return
		}
	}
}

func (p *rawUringProvider) drainWake() error {
	var value [8]byte
	for {
		_, err := unix.Read(p.wakeFD, value[:])
		if err == nil || errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EAGAIN) {
			return nil
		}
		return err
	}
}

func (p *rawUringProvider) waitFuture(
	ctx context.Context,
	future *uringFuture,
) (uringResult, error) {
	select {
	case result := <-future.done:
		return result, nil
	case <-ctx.Done():
	}
	cancel, cancelErr := p.submitCancel(future.id)
	if cancelErr == nil {
		<-cancel.done
	}
	result := <-future.done
	return result, ctx.Err()
}

func tryUringFuture(future *uringFuture) (uringResult, bool) {
	select {
	case result := <-future.done:
		return result, true
	default:
		return uringResult{}, false
	}
}

func (p *rawUringProvider) closeOwnedFD(fd int) error {
	future, submitErr := p.submitClose(fd)
	if submitErr == nil {
		result := <-future.done
		if result.err == nil && result.res == 0 {
			return nil
		}
		if result.err != nil {
			if future.submitted.Load() {
				return fmt.Errorf("io_uring close outcome unknown: %w", result.err)
			}
			submitErr = result.err
		} else if result.res < 0 {
			err := syscall.Errno(-result.res)
			if isUringOperationUnsupported(err) {
				return &backendUnsupportedError{reason: "io_uring close", err: err}
			}
			return os.NewSyscallError("io_uring close", err)
		} else {
			return fmt.Errorf("io_uring close returned unexpected result %d", result.res)
		}
	}
	// Fallback is safe only when the CLOSE command never reached the SQ.
	fallbackErr := unix.Close(fd)
	if fallbackErr == nil {
		return submitErr
	}
	return errors.Join(submitErr, fallbackErr)
}

func (p *rawUringProvider) runOwner() {
	defer close(p.ownerDone)
	pending := make(map[uint64]*uringCommand, p.depth)
	var local []*uringCommand
	shuttingDown := false
	shutdownCancelRequested := make(map[uint64]struct{}, p.depth)

	for {
		if err := p.drainCompletions(pending); err != nil {
			p.failOwner(err, local, pending)
			return
		}

		incoming, shutdown := p.takeQueue()
		if shutdown && !shuttingDown {
			shuttingDown = true
			for _, command := range local {
				command.finish(uringResult{err: errUringProviderClosed})
			}
			local = nil
			for _, command := range incoming {
				command.finish(uringResult{err: errUringProviderClosed})
			}
		} else if shuttingDown {
			for _, command := range incoming {
				command.finish(uringResult{err: errUringProviderClosed})
			}
		} else {
			local = append(local, incoming...)
		}
		if shuttingDown {
			for id, command := range pending {
				if !command.ordinary {
					continue
				}
				if _, exists := shutdownCancelRequested[id]; exists {
					continue
				}
				cancel, ok := p.tryShutdownCancel(id)
				if !ok {
					break
				}
				shutdownCancelRequested[id] = struct{}{}
				local = append(local, cancel)
			}
		}

		if len(local) > 0 {
			submitted, err := p.ring.submit(local, pending)
			p.recordSubmitted(local[:submitted])
			local = local[submitted:]
			if err != nil {
				p.failOwner(err, local, pending)
				return
			}
		}
		if err := p.drainCompletions(pending); err != nil {
			p.failOwner(err, local, pending)
			return
		}
		if shuttingDown && len(local) == 0 && len(pending) == 0 {
			return
		}

		pollFD := []unix.PollFd{{Fd: int32(p.wakeFD), Events: unix.POLLIN}}
		for {
			_, err := unix.Poll(pollFD, -1)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				p.failOwner(err, local, pending)
				return
			}
			break
		}
		if pollFD[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			p.failOwner(fmt.Errorf("io_uring eventfd poll revents %#x", pollFD[0].Revents), local, pending)
			return
		}
		if err := p.drainWake(); err != nil {
			p.failOwner(err, local, pending)
			return
		}
	}
}

func (p *rawUringProvider) failOwner(
	err error,
	local []*uringCommand,
	pending map[uint64]*uringCommand,
) {
	reported := err
	if isUringRuntimeUnavailable(err) {
		reported = &backendUnsupportedError{reason: "io_uring enter", err: err}
	}
	p.setOwnerError(reported)
	queued := p.stopAccepting(true)
	_ = p.ring.close()
	result := uringResult{err: fmt.Errorf("io_uring owner: %w", reported)}
	for _, command := range local {
		command.finish(result)
	}
	for _, command := range queued {
		command.finish(result)
	}
	for _, command := range pending {
		p.retireCommand(command, result)
	}
}

func (p *rawUringProvider) recordSubmitted(commands []*uringCommand) {
	for _, command := range commands {
		if command.ordinary {
			command.queueGauged = true
			p.metrics.AddGauge(FileScanGaugeQueueDepth, 1)
		}
		if command.readRequested > 0 {
			command.bytesGauged = true
			p.metrics.AddGauge(uringGaugeSubmittedBytes, command.readRequested)
		}
	}
}

func (p *rawUringProvider) drainCompletions(pending map[uint64]*uringCommand) error {
	return p.ring.drainCompletions(pending, func(command *uringCommand, result int32) {
		p.retireSubmissionGauges(command)
		if result > 0 && command.readChunk != nil {
			completed := min(int64(result), command.readRequested)
			command.readChunk.completedBytes.Add(completed)
			p.metrics.AddGauge(uringGaugeCompletedUnconsumedBytes, completed)
			reader := command.readChunk.reader
			reader.reorderMu.Lock()
			if int64(result) >= int64(command.readLogical) &&
				command.readChunk.seq > reader.nextSequence.Load() &&
				command.readChunk.reordered.CompareAndSwap(false, true) {
				p.metrics.AddGauge(uringGaugeReorderDepth, 1)
			}
			reader.reorderMu.Unlock()
		}
		command.finish(uringResult{res: result})
	})
}

func (p *rawUringProvider) retireSubmissionGauges(command *uringCommand) {
	if command.queueGauged {
		command.queueGauged = false
		p.metrics.AddGauge(FileScanGaugeQueueDepth, -1)
	}
	if command.bytesGauged {
		command.bytesGauged = false
		p.metrics.AddGauge(uringGaugeSubmittedBytes, -command.readRequested)
	}
}

func (p *rawUringProvider) retireCommand(command *uringCommand, result uringResult) {
	p.retireSubmissionGauges(command)
	command.finish(result)
}

func (p *rawUringProvider) setOwnerError(err error) {
	p.ownerErrMu.Lock()
	if p.ownerErr == nil {
		p.ownerErr = err
	}
	p.ownerErrMu.Unlock()
}

func (p *rawUringProvider) providerError() error {
	p.ownerErrMu.Lock()
	defer p.ownerErrMu.Unlock()
	if p.ownerErr != nil {
		return fmt.Errorf("io_uring owner: %w", p.ownerErr)
	}
	return errUringProviderClosed
}

func (p *rawUringProvider) close() error {
	p.closeOnce.Do(func() {
		var errs []error
		for _, reader := range p.beginReaderShutdown() {
			errs = append(errs, reader.Close())
		}

		queued := p.stopAccepting(true)
		for _, command := range queued {
			command.finish(uringResult{err: errUringProviderClosed})
		}
		p.wake()
		<-p.ownerDone

		if !p.ring.closed.Load() {
			errs = append(errs, p.ring.register(ioUringUnregisterEventfd, nil, 0))
			if p.registered {
				errs = append(errs, p.ring.register(ioUringUnregisterBuffers, nil, 0))
			}
		}
		errs = append(errs, p.ring.close())
		p.ring.unmap()
		errs = append(errs, unix.Close(p.wakeFD))
		p.releaseBuffers()
		if p.pinnedBytes > 0 {
			p.metrics.AddGauge(FileScanGaugePinnedBytes, -p.pinnedBytes)
			p.pinnedBytes = 0
		}
		p.ownerErrMu.Lock()
		errs = append(errs, p.ownerErr)
		p.ownerErrMu.Unlock()
		p.closeErr = errors.Join(errs...)
	})
	return p.closeErr
}

type uringChunk struct {
	seq        int64
	reader     *uringReader
	buffer     *uringBuffer
	expected   int
	received   int
	requestLen int
	future     *uringFuture

	completedBytes atomic.Int64
	reordered      atomic.Bool
}

type uringReader struct {
	ctx       context.Context
	cancel    context.CancelFunc
	provider  *rawUringProvider
	fd        int
	size      int64
	chunkSize int
	direct    bool
	depth     int

	pending      map[int64]*uringChunk
	totalChunks  int64
	submitted    int64
	next         int64
	current      *uringChunk
	currentPos   int
	terminalErr  error
	nextSequence atomic.Int64
	reorderMu    sync.Mutex

	mu        sync.Mutex
	closing   atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

func (r *uringReader) Read(dst []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(dst) == 0 {
		return 0, nil
	}
	if r.closing.Load() {
		return 0, os.ErrClosed
	}
	total := 0
	for total < len(dst) {
		if r.closing.Load() {
			if total > 0 {
				return total, nil
			}
			return 0, os.ErrClosed
		}
		if r.current == nil {
			if r.terminalErr != nil {
				break
			}
			if r.next >= r.totalChunks {
				break
			}
			chunk := r.pending[r.next]
			if chunk == nil {
				r.terminalErr = fmt.Errorf("io_uring reader lost chunk %d", r.next)
				break
			}
			if err := r.completeChunk(chunk); err != nil {
				r.releaseChunkAccounting(chunk)
				r.provider.releaseBuffer(chunk.buffer)
				chunk.buffer = nil
				delete(r.pending, chunk.seq)
				r.terminalErr = err
				break
			}
			r.current = chunk
			r.currentPos = 0
		}

		n := copy(dst[total:], r.current.buffer.data[r.currentPos:r.current.expected])
		r.consumeCompletedBytes(r.current, n)
		r.currentPos += n
		total += n
		if r.currentPos == r.current.expected {
			r.releaseChunkAccounting(r.current)
			r.provider.releaseBuffer(r.current.buffer)
			r.current.buffer = nil
			delete(r.pending, r.current.seq)
			r.current = nil
			r.next++
			r.reorderMu.Lock()
			r.nextSequence.Store(r.next)
			if next := r.pending[r.next]; next != nil && next.reordered.CompareAndSwap(true, false) {
				r.provider.metrics.AddGauge(uringGaugeReorderDepth, -1)
			}
			r.reorderMu.Unlock()
			if err := r.submitMore(); err != nil && r.terminalErr == nil {
				r.terminalErr = err
			}
		}
	}
	if total > 0 {
		return total, nil
	}
	if r.terminalErr != nil {
		return 0, r.terminalErr
	}
	return 0, io.EOF
}

func (r *uringReader) consumeCompletedBytes(chunk *uringChunk, count int) {
	if count <= 0 {
		return
	}
	chunk.completedBytes.Add(-int64(count))
	r.provider.metrics.AddGauge(uringGaugeCompletedUnconsumedBytes, -int64(count))
}

func (r *uringReader) releaseChunkAccounting(chunk *uringChunk) {
	if completed := chunk.completedBytes.Swap(0); completed > 0 {
		r.provider.metrics.AddGauge(uringGaugeCompletedUnconsumedBytes, -completed)
	}
	r.reorderMu.Lock()
	if chunk.reordered.CompareAndSwap(true, false) {
		r.provider.metrics.AddGauge(uringGaugeReorderDepth, -1)
	}
	r.reorderMu.Unlock()
}

func (r *uringReader) submitMore() error {
	for len(r.pending) < r.depth && r.submitted < r.totalChunks {
		buffer, err := r.provider.acquireBuffer(r.ctx)
		if err != nil {
			return err
		}
		offset := r.submitted * int64(r.chunkSize)
		expected := min(r.chunkSize, int(r.size-offset))
		chunk := &uringChunk{
			seq:      r.submitted,
			reader:   r,
			buffer:   buffer,
			expected: expected,
		}
		if err := r.submitChunkRead(chunk); err != nil {
			r.provider.releaseBuffer(buffer)
			return err
		}
		r.pending[chunk.seq] = chunk
		r.submitted++
	}
	return nil
}

func (r *uringReader) submitChunkRead(chunk *uringChunk) error {
	requestLen := chunk.expected - chunk.received
	if r.direct {
		requestLen = r.chunkSize
	}
	future, err := r.provider.submitRead(
		r.ctx,
		r.fd,
		chunk,
		chunk.buffer,
		chunk.received,
		requestLen,
		chunk.expected-chunk.received,
		uint64(chunk.seq*int64(r.chunkSize))+uint64(chunk.received),
	)
	if err != nil {
		return fmt.Errorf("submit io_uring read: %w", err)
	}
	chunk.requestLen = requestLen
	chunk.future = future
	return nil
}

func (r *uringReader) completeChunk(chunk *uringChunk) error {
	for {
		result, waitErr := r.provider.waitFuture(r.ctx, chunk.future)
		chunk.future = nil
		if waitErr != nil {
			return waitErr
		}
		if result.err != nil {
			return result.err
		}
		if result.res < 0 {
			err := syscall.Errno(-result.res)
			if isUringOperationUnsupported(err) || (r.direct && errors.Is(err, unix.EINVAL)) {
				return &backendUnsupportedError{reason: "io_uring read", err: err}
			}
			return os.NewSyscallError("io_uring read", err)
		}
		if int64(result.res) > int64(chunk.requestLen) {
			return fmt.Errorf(
				"io_uring read returned %d bytes for a %d-byte request",
				result.res,
				chunk.requestLen,
			)
		}
		if r.direct {
			if int(result.res) < chunk.expected {
				return io.ErrUnexpectedEOF
			}
			chunk.received = chunk.expected
			return nil
		}
		if result.res == 0 {
			return io.ErrUnexpectedEOF
		}
		chunk.received += int(result.res)
		if chunk.received == chunk.expected {
			return nil
		}
		if err := r.submitChunkRead(chunk); err != nil {
			return err
		}
	}
}

func (r *uringReader) Close() error {
	r.closeOnce.Do(func() {
		r.closing.Store(true)
		r.cancel()
		r.mu.Lock()
		r.closeErr = r.closeLocked()
		r.mu.Unlock()
		r.provider.removeReader(r)
	})
	return r.closeErr
}

func (r *uringReader) closeLocked() error {
	type closeWait struct {
		chunk  *uringChunk
		read   *uringFuture
		cancel *uringFuture
		result uringResult
		done   bool
	}
	waits := make([]closeWait, 0, len(r.pending))
	var errs []error
	for _, chunk := range r.pending {
		wait := closeWait{chunk: chunk, read: chunk.future}
		if chunk.future == nil {
			wait.done = true
		} else if result, ok := tryUringFuture(chunk.future); ok {
			wait.result = result
			wait.done = true
		} else {
			cancel, err := r.provider.submitCancel(chunk.future.id)
			if err != nil {
				errs = append(errs, err)
			} else {
				wait.cancel = cancel
			}
		}
		waits = append(waits, wait)
	}
	for i := range waits {
		if waits[i].cancel != nil {
			result := <-waits[i].cancel.done
			if result.err != nil && !errors.Is(result.err, errUringProviderClosed) {
				errs = append(errs, result.err)
			} else if result.res < 0 {
				err := syscall.Errno(-result.res)
				if !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.EALREADY) {
					if isUringOperationUnsupported(err) {
						errs = append(errs, &backendUnsupportedError{reason: "io_uring cancel", err: err})
					} else {
						errs = append(errs, os.NewSyscallError("io_uring cancel", err))
					}
				}
			}
		}
	}
	for i := range waits {
		if !waits[i].done && waits[i].read != nil {
			waits[i].result = <-waits[i].read.done
			waits[i].done = true
		}
		if waits[i].chunk.buffer != nil {
			r.releaseChunkAccounting(waits[i].chunk)
			r.provider.releaseBuffer(waits[i].chunk.buffer)
			waits[i].chunk.buffer = nil
		}
	}
	clear(r.pending)
	r.current = nil
	errs = append(errs, r.provider.closeOwnedFD(r.fd))
	return errors.Join(errs...)
}

func newRawUring(entries uint32) (*rawUring, error) {
	var params ioUringParams
	fd, err := ioUringSetup(entries, &params)
	if err != nil {
		return nil, err
	}
	ring := &rawUring{fd: fd}
	fail := func(cause error) (*rawUring, error) {
		ring.unmap()
		_ = ring.close()
		return nil, cause
	}
	if params.SQEntries == 0 || params.CQEntries == 0 {
		return fail(fmt.Errorf("io_uring returned empty rings"))
	}
	if params.Features&ioUringFeatNoDrop == 0 {
		return fail(fmt.Errorf("kernel lacks IORING_FEAT_NODROP"))
	}

	sqBytes, err := checkedRingBytes(params.SQOff.Array, params.SQEntries, 4)
	if err != nil {
		return fail(err)
	}
	cqBytes, err := checkedRingBytes(params.CQOff.CQEs, params.CQEntries, uint64(unsafe.Sizeof(ioUringCQE{})))
	if err != nil {
		return fail(err)
	}
	sqeBytes, err := checkedRingBytes(0, params.SQEntries, uint64(unsafe.Sizeof(ioUringSQE{})))
	if err != nil {
		return fail(err)
	}
	if params.Features&ioUringFeatSingleMmap != 0 {
		ring.sqRingMap, err = unix.Mmap(
			fd,
			ioUringOffSQRing,
			max(sqBytes, cqBytes),
			unix.PROT_READ|unix.PROT_WRITE,
			unix.MAP_SHARED,
		)
		if err != nil {
			return fail(err)
		}
		ring.cqRingMap = ring.sqRingMap
	} else {
		ring.sqRingMap, err = unix.Mmap(
			fd,
			ioUringOffSQRing,
			sqBytes,
			unix.PROT_READ|unix.PROT_WRITE,
			unix.MAP_SHARED,
		)
		if err != nil {
			return fail(err)
		}
		ring.cqRingMap, err = unix.Mmap(
			fd,
			ioUringOffCQRing,
			cqBytes,
			unix.PROT_READ|unix.PROT_WRITE,
			unix.MAP_SHARED,
		)
		if err != nil {
			return fail(err)
		}
	}
	ring.sqesMap, err = unix.Mmap(
		fd,
		ioUringOffSQEs,
		sqeBytes,
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_SHARED,
	)
	if err != nil {
		return fail(err)
	}

	if err := ring.bind(params); err != nil {
		return fail(err)
	}
	return ring, nil
}

func (r *rawUring) bind(params ioUringParams) error {
	var err error
	if r.sqHead, err = mappedUint32(r.sqRingMap, params.SQOff.Head); err != nil {
		return err
	}
	if r.sqTail, err = mappedUint32(r.sqRingMap, params.SQOff.Tail); err != nil {
		return err
	}
	sqMask, err := mappedUint32(r.sqRingMap, params.SQOff.RingMask)
	if err != nil {
		return err
	}
	sqEntries, err := mappedUint32(r.sqRingMap, params.SQOff.RingEntries)
	if err != nil {
		return err
	}
	if r.sqDropped, err = mappedUint32(r.sqRingMap, params.SQOff.Dropped); err != nil {
		return err
	}
	r.sqMask = atomic.LoadUint32(sqMask)
	r.sqEntries = atomic.LoadUint32(sqEntries)
	if r.sqEntries != params.SQEntries || r.sqMask+1 != r.sqEntries {
		return fmt.Errorf("invalid io_uring submission ring geometry")
	}
	if r.sqArray, err = mappedUint32Slice(r.sqRingMap, params.SQOff.Array, r.sqEntries); err != nil {
		return err
	}
	if r.sqes, err = mappedSQESlice(r.sqesMap, r.sqEntries); err != nil {
		return err
	}

	if r.cqHead, err = mappedUint32(r.cqRingMap, params.CQOff.Head); err != nil {
		return err
	}
	if r.cqTail, err = mappedUint32(r.cqRingMap, params.CQOff.Tail); err != nil {
		return err
	}
	cqMask, err := mappedUint32(r.cqRingMap, params.CQOff.RingMask)
	if err != nil {
		return err
	}
	cqEntries, err := mappedUint32(r.cqRingMap, params.CQOff.RingEntries)
	if err != nil {
		return err
	}
	if r.cqOverflow, err = mappedUint32(r.cqRingMap, params.CQOff.Overflow); err != nil {
		return err
	}
	r.cqMask = atomic.LoadUint32(cqMask)
	r.cqEntries = atomic.LoadUint32(cqEntries)
	if r.cqEntries != params.CQEntries || r.cqMask+1 != r.cqEntries {
		return fmt.Errorf("invalid io_uring completion ring geometry")
	}
	if r.cqes, err = mappedCQESlice(r.cqRingMap, params.CQOff.CQEs, r.cqEntries); err != nil {
		return err
	}
	return nil
}

func (r *rawUring) submit(
	commands []*uringCommand,
	pending map[uint64]*uringCommand,
) (int, error) {
	head := atomic.LoadUint32(r.sqHead)
	tail := atomic.LoadUint32(r.sqTail)
	space := r.sqEntries - (tail - head)
	count := min(len(commands), int(space))
	if count == 0 {
		return 0, nil
	}
	for i := 0; i < count; i++ {
		command := commands[i]
		index := (tail + uint32(i)) & r.sqMask
		r.sqes[index] = ioUringSQE{}
		r.sqes[index] = command.sqe
		r.sqArray[index] = index
		if _, exists := pending[command.future.id]; exists {
			return i, fmt.Errorf("duplicate io_uring request id %d", command.future.id)
		}
		pending[command.future.id] = command
		command.future.submitted.Store(true)
	}
	atomic.StoreUint32(r.sqTail, tail+uint32(count))
	for attempts := 0; ; attempts++ {
		queued := atomic.LoadUint32(r.sqTail) - atomic.LoadUint32(r.sqHead)
		if queued == 0 {
			break
		}
		_, err := ioUringEnter(r.fd, queued, 0, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return count, err
		}
		if attempts == 8 {
			return count, fmt.Errorf("io_uring submission made no progress")
		}
	}
	if dropped := atomic.LoadUint32(r.sqDropped); dropped != 0 {
		return count, fmt.Errorf("io_uring dropped %d submissions", dropped)
	}
	return count, nil
}

func (r *rawUring) drainCompletions(
	pending map[uint64]*uringCommand,
	complete func(*uringCommand, int32),
) error {
	head := atomic.LoadUint32(r.cqHead)
	tail := atomic.LoadUint32(r.cqTail)
	var correlationErr error
	for head != tail {
		cqe := r.cqes[head&r.cqMask]
		command := pending[cqe.UserData]
		if command == nil {
			if correlationErr == nil {
				correlationErr = fmt.Errorf("completion for unknown io_uring request id %d", cqe.UserData)
			}
		} else {
			delete(pending, cqe.UserData)
			complete(command, cqe.Res)
		}
		head++
	}
	atomic.StoreUint32(r.cqHead, head)
	if overflow := atomic.LoadUint32(r.cqOverflow); overflow != 0 {
		return fmt.Errorf("io_uring completion queue overflowed %d times", overflow)
	}
	return correlationErr
}

func (r *rawUring) register(opcode uint32, arg unsafe.Pointer, count uint32) error {
	if r.closed.Load() {
		return errUringProviderClosed
	}
	return ioUringRegister(r.fd, opcode, arg, count)
}

func (r *rawUring) close() error {
	r.closeOnce.Do(func() {
		r.closed.Store(true)
		r.closeErr = unix.Close(r.fd)
	})
	return r.closeErr
}

func (r *rawUring) unmap() {
	if len(r.sqesMap) > 0 {
		_ = unix.Munmap(r.sqesMap)
		r.sqesMap = nil
	}
	if len(r.sqRingMap) > 0 {
		_ = unix.Munmap(r.sqRingMap)
		if len(r.cqRingMap) > 0 && unsafe.SliceData(r.cqRingMap) == unsafe.SliceData(r.sqRingMap) {
			r.cqRingMap = nil
		}
		r.sqRingMap = nil
	}
	if len(r.cqRingMap) > 0 {
		_ = unix.Munmap(r.cqRingMap)
		r.cqRingMap = nil
	}
}

func ioUringSetup(entries uint32, params *ioUringParams) (int, error) {
	result, _, errno := unix.Syscall(
		unix.SYS_IO_URING_SETUP,
		uintptr(entries),
		uintptr(unsafe.Pointer(params)),
		0,
	)
	runtime.KeepAlive(params)
	if errno != 0 {
		return -1, errno
	}
	return int(result), nil
}

func ioUringEnter(fd int, submit uint32, minComplete uint32, flags uint32) (uint32, error) {
	result, _, errno := unix.Syscall6(
		unix.SYS_IO_URING_ENTER,
		uintptr(fd),
		uintptr(submit),
		uintptr(minComplete),
		uintptr(flags),
		0,
		0,
	)
	if errno != 0 {
		return 0, errno
	}
	return uint32(result), nil
}

func ioUringRegister(fd int, opcode uint32, arg unsafe.Pointer, count uint32) error {
	_, _, errno := unix.Syscall6(
		unix.SYS_IO_URING_REGISTER,
		uintptr(fd),
		uintptr(opcode),
		uintptr(arg),
		uintptr(count),
		0,
		0,
	)
	runtime.KeepAlive(arg)
	if errno != 0 {
		return errno
	}
	return nil
}

func checkedRingBytes(offset uint32, count uint32, elementSize uint64) (int, error) {
	bytes := uint64(offset) + uint64(count)*elementSize
	if bytes > uint64(math.MaxInt) {
		return 0, fmt.Errorf("io_uring mapping size overflow")
	}
	return int(bytes), nil
}

func mappedUint32(mapping []byte, offset uint32) (*uint32, error) {
	if uint64(offset)+4 > uint64(len(mapping)) || offset%4 != 0 {
		return nil, fmt.Errorf("invalid io_uring uint32 offset %d", offset)
	}
	return (*uint32)(unsafe.Pointer(&mapping[offset])), nil
}

func mappedUint32Slice(mapping []byte, offset uint32, count uint32) ([]uint32, error) {
	bytes, err := checkedRingBytes(offset, count, 4)
	if err != nil || bytes > len(mapping) || offset%4 != 0 {
		return nil, fmt.Errorf("invalid io_uring uint32 slice at offset %d", offset)
	}
	return unsafe.Slice((*uint32)(unsafe.Pointer(&mapping[offset])), count), nil
}

func mappedSQESlice(mapping []byte, count uint32) ([]ioUringSQE, error) {
	bytes, err := checkedRingBytes(0, count, uint64(unsafe.Sizeof(ioUringSQE{})))
	if err != nil || bytes > len(mapping) {
		return nil, fmt.Errorf("invalid io_uring SQE mapping")
	}
	return unsafe.Slice((*ioUringSQE)(unsafe.Pointer(unsafe.SliceData(mapping))), count), nil
}

func mappedCQESlice(mapping []byte, offset uint32, count uint32) ([]ioUringCQE, error) {
	bytes, err := checkedRingBytes(offset, count, uint64(unsafe.Sizeof(ioUringCQE{})))
	if err != nil || bytes > len(mapping) || offset%8 != 0 {
		return nil, fmt.Errorf("invalid io_uring CQE mapping at offset %d", offset)
	}
	return unsafe.Slice((*ioUringCQE)(unsafe.Pointer(&mapping[offset])), count), nil
}
