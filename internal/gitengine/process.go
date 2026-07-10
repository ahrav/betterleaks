package gitengine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// CommandBuilder constructs a fresh helper command for one repository.
type CommandBuilder func(repoPath string, profile ScanProfile) *exec.Cmd

// ProcessConfig configures bounded helper process orchestration.
type ProcessConfig struct {
	Command    CommandBuilder
	MaxFrame   uint32
	MaxRecord  uint64
	StderrTail int
	KillGrace  time.Duration
}

// ProcessFactory enforces a successful repository-wide preflight before any
// worker can be opened. It only owns transport and process lifecycle; helper
// discovery, packaging, and the Git/libgit2/gix implementations remain
// candidate-specific work and are intentionally not simulated here.
type ProcessFactory struct {
	config ProcessConfig

	mu         sync.Mutex
	preflight  bool
	repoPath   string
	profile    ScanProfile
	capability Capabilities
}

// NewProcessFactory returns a concrete helper factory.
func NewProcessFactory(config ProcessConfig) (*ProcessFactory, error) {
	if config.Command == nil {
		return nil, errors.New("git engine helper command is nil")
	}
	if config.MaxFrame == 0 {
		config.MaxFrame = DefaultMaxFrame
	}
	if config.MaxRecord == 0 {
		config.MaxRecord = 256 << 20
	}
	if config.StderrTail <= 0 {
		config.StderrTail = 64 << 10
	}
	if config.KillGrace <= 0 {
		config.KillGrace = 500 * time.Millisecond
	}
	return &ProcessFactory{config: config}, nil
}

// Preflight negotiates the protocol in isolation. ErrUnsupported is safe to
// use for fallback because this method stores success only after the helper
// exits cleanly and before Open is enabled.
func (f *ProcessFactory) Preflight(ctx context.Context, repoPath string, profile ScanProfile) (Capabilities, error) {
	worker, err := startProcess(ctx, f.config, repoPath, profile)
	if err != nil {
		return Capabilities{}, err
	}
	capabilities := worker.capabilities
	if profile.AllStatuses && !capabilities.AllStatuses {
		_ = worker.Close()
		return Capabilities{}, fmt.Errorf("all-status profile: %w", ErrUnsupported)
	}
	if profile.FindCopies && !capabilities.FindCopies {
		_ = worker.Close()
		return Capabilities{}, fmt.Errorf("copy profile: %w", ErrUnsupported)
	}
	if err := worker.Close(); err != nil {
		return Capabilities{}, err
	}
	f.mu.Lock()
	f.preflight = true
	f.repoPath = repoPath
	f.profile = profile
	f.capability = capabilities
	f.mu.Unlock()
	return capabilities, nil
}

// Open starts a worker only after matching repository-wide preflight.
func (f *ProcessFactory) Open(ctx context.Context, repoPath string, profile ScanProfile) (Worker, error) {
	f.mu.Lock()
	ready := f.preflight && f.repoPath == repoPath && f.profile == profile
	expected := f.capability
	f.mu.Unlock()
	if !ready {
		return nil, errors.New("git engine preflight has not completed for repository and profile")
	}
	worker, err := startProcess(ctx, f.config, repoPath, profile)
	if err != nil {
		return nil, err
	}
	if worker.capabilities != expected {
		_ = worker.Close()
		return nil, &EngineError{Kind: ErrorProtocol, Op: "open", Err: errors.New("helper capabilities changed after preflight")}
	}
	return worker, nil
}

type processWorker struct {
	config       ProcessConfig
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	stdout       io.ReadCloser
	stderr       *tailBuffer
	capabilities Capabilities
	profile      ScanProfile
	waitDone     chan struct{}

	scanMu  sync.Mutex
	mu      sync.Mutex
	closed  bool
	final   error
	waitErr error
}

func startProcess(ctx context.Context, config ProcessConfig, repoPath string, profile ScanProfile) (*processWorker, error) {
	if config.MaxFrame == 0 {
		config.MaxFrame = DefaultMaxFrame
	}
	if config.MaxRecord == 0 {
		config.MaxRecord = 256 << 20
	}
	if config.StderrTail <= 0 {
		config.StderrTail = 64 << 10
	}
	if config.KillGrace <= 0 {
		config.KillGrace = 500 * time.Millisecond
	}
	cmd := config.Command(repoPath, profile)
	if cmd == nil {
		return nil, errors.New("git engine helper command builder returned nil")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("helper stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("helper stdout: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("helper stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start helper: %w", err)
	}
	w := &processWorker{
		config:   config,
		cmd:      cmd,
		stdin:    stdin,
		stdout:   stdout,
		stderr:   newTailBuffer(config.StderrTail),
		waitDone: make(chan struct{}),
		profile:  profile,
	}
	go func() {
		_, _ = io.Copy(w.stderr, stderrPipe)
	}()
	go func() {
		err := cmd.Wait()
		w.mu.Lock()
		w.waitErr = err
		w.mu.Unlock()
		close(w.waitDone)
	}()

	stop := context.AfterFunc(ctx, func() { w.interrupt() })
	frame, readErr := ReadFrame(stdout, config.MaxFrame)
	stop()
	if readErr != nil {
		err := w.terminal("handshake", 0, ErrorProtocol, readErr)
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, w.terminal("handshake", 0, ErrorCanceled, err)
	}
	if frame.Tag == TagError {
		helperErr, decodeErr := DecodeError(frame)
		if decodeErr != nil {
			return nil, w.terminal("handshake", 0, ErrorProtocol, decodeErr)
		}
		if frame.BatchID != 0 || helperErr.Kind != ErrorUnsupported {
			return nil, w.terminal("handshake", 0, ErrorProtocol,
				fmt.Errorf("unexpected pre-HELO error kind %d", helperErr.Kind))
		}
		w.interrupt()
		return nil, fmt.Errorf("%s: %w", helperErr.Err, ErrUnsupported)
	}
	capabilities, err := DecodeHello(frame)
	if err != nil {
		err = w.terminal("handshake", 0, ErrorProtocol, err)
		return nil, err
	}
	w.capabilities = capabilities
	return w, nil
}

// ScanBatch writes and reads concurrently so neither pipe can deadlock when a
// helper starts producing output before consuming a large request.
func (w *processWorker) ScanBatch(ctx context.Context, request BatchRequest, emit EmitFunc) (BatchResult, error) {
	w.scanMu.Lock()
	defer w.scanMu.Unlock()
	if emit == nil {
		return BatchResult{}, errors.New("git engine emit callback is nil")
	}
	if err := w.available(); err != nil {
		return BatchResult{}, err
	}
	if err := request.Validate(int(w.capabilities.OIDLength)); err != nil {
		return BatchResult{}, err
	}
	begin, err := BatchBeginFrame(request)
	if err != nil {
		return BatchResult{}, err
	}

	type readResult struct {
		result BatchResult
		err    error
	}
	readCh := make(chan readResult, 1)
	writeCh := make(chan error, 1)
	go func() { writeCh <- WriteFrame(w.stdin, begin, w.config.MaxFrame) }()
	go func() {
		result, err := w.readBatch(request, emit)
		readCh <- readResult{result: result, err: err}
	}()

	var (
		written bool
		read    bool
		result  BatchResult
	)
	for !written || !read {
		select {
		case err := <-writeCh:
			written = true
			writeCh = nil
			if err != nil {
				return BatchResult{}, w.terminal("write request", request.ID, ErrorHelper, err)
			}
		case response := <-readCh:
			read = true
			readCh = nil
			if response.err != nil {
				return BatchResult{}, w.terminal("read response", request.ID, errorKind(response.err), response.err)
			}
			result = response.result
		case <-ctx.Done():
			terminalErr := w.terminal("cancel", request.ID, ErrorCanceled, ctx.Err())
			// EmitFunc is cooperatively cancellable by contract. Waiting here
			// proves the protocol reader has exited before ScanBatch returns.
			if writeCh != nil {
				<-writeCh
			}
			if readCh != nil {
				<-readCh
			}
			return BatchResult{}, terminalErr
		}
	}
	return result, nil
}

func (w *processWorker) readBatch(request BatchRequest, emit EmitFunc) (BatchResult, error) {
	started := false
	inCommit := false
	inFile := false
	var currentCommit OID
	var currentPath []byte
	var continuedHunk *HunkRecord
	var commits, files, hunks uint64
	for {
		frame, err := ReadFrame(w.stdout, w.config.MaxFrame)
		if err != nil {
			return BatchResult{}, err
		}
		if frame.BatchID != request.ID {
			return BatchResult{}, fmt.Errorf("frame batch ID %d != request %d", frame.BatchID, request.ID)
		}
		if !started {
			if frame.Tag != TagBatchBegin || len(frame.Payload) != 0 {
				return BatchResult{}, errors.New("response does not begin with empty BGIN")
			}
			started = true
			continue
		}
		switch frame.Tag {
		case TagCommit:
			if inCommit || continuedHunk != nil {
				return BatchResult{}, errors.New("CMIT while commit is open")
			}
			record, err := DecodeRecord(frame)
			if err != nil {
				return BatchResult{}, err
			}
			if err := w.validateNegotiatedRecord(record); err != nil {
				return BatchResult{}, err
			}
			if commits >= uint64(len(request.Commits)) || !bytes.Equal(record.Commit.OID, request.Commits[commits]) {
				return BatchResult{}, errors.New("CMIT object ID does not match requested order")
			}
			if err := emit(record); err != nil {
				return BatchResult{}, &EngineError{Kind: ErrorEmission, BatchID: request.ID, Op: "emit", Err: err}
			}
			inCommit = true
			currentCommit = bytes.Clone(record.Commit.OID)
			commits++
		case TagFileBegin:
			if !inCommit || inFile || continuedHunk != nil {
				return BatchResult{}, errors.New("FBEG outside commit or while file is open")
			}
			record, err := DecodeRecord(frame)
			if err != nil {
				return BatchResult{}, err
			}
			if err := w.validateNegotiatedRecord(record); err != nil {
				return BatchResult{}, err
			}
			if !bytes.Equal(record.File.Commit, currentCommit) {
				return BatchResult{}, errors.New("FBEG commit does not match open commit")
			}
			if err := emit(record); err != nil {
				return BatchResult{}, &EngineError{Kind: ErrorEmission, BatchID: request.ID, Op: "emit", Err: err}
			}
			inFile = true
			currentPath = bytes.Clone(record.File.NewPath)
			files++
		case TagHunk:
			if !inFile || continuedHunk != nil {
				return BatchResult{}, errors.New("HUNK outside file")
			}
			record, err := DecodeRecord(frame)
			if err != nil {
				return BatchResult{}, err
			}
			if err := w.validateNegotiatedRecord(record); err != nil {
				return BatchResult{}, err
			}
			if !bytes.Equal(record.Hunk.Commit, currentCommit) || !bytes.Equal(record.Hunk.NewPath, currentPath) {
				return BatchResult{}, errors.New("HUNK does not match open commit and file")
			}
			if err := emit(record); err != nil {
				return BatchResult{}, &EngineError{Kind: ErrorEmission, BatchID: request.ID, Op: "emit", Err: err}
			}
			hunks++
		case TagHunkBegin:
			if !inFile || continuedHunk != nil {
				return BatchResult{}, errors.New("HBGN outside file or while continuation is open")
			}
			hunk, err := DecodeHunkBegin(frame)
			if err != nil {
				return BatchResult{}, err
			}
			if err := w.validateNegotiatedRecord(Record{Kind: RecordHunk, Hunk: hunk}); err != nil {
				return BatchResult{}, err
			}
			if !bytes.Equal(hunk.Commit, currentCommit) || !bytes.Equal(hunk.NewPath, currentPath) {
				return BatchResult{}, errors.New("HBGN does not match open commit and file")
			}
			continuedHunk = &hunk
		case TagHunkAdd:
			if continuedHunk == nil || len(frame.Payload) == 0 {
				return BatchResult{}, errors.New("HADD without continuation or with empty payload")
			}
			if uint64(len(continuedHunk.Added))+uint64(len(frame.Payload)) > w.config.MaxRecord {
				return BatchResult{}, fmt.Errorf("continued hunk exceeds record limit %d", w.config.MaxRecord)
			}
			continuedHunk.Added = append(continuedHunk.Added, frame.Payload...)
		case TagHunkEnd:
			if continuedHunk == nil {
				return BatchResult{}, errors.New("HEND without continuation")
			}
			if len(continuedHunk.Added) == 0 {
				return BatchResult{}, errors.New("HEND before any HADD payload")
			}
			missingFinalNewline, err := DecodeHunkEnd(frame)
			if err != nil {
				return BatchResult{}, err
			}
			continuedHunk.MissingFinalNewline = missingFinalNewline
			record := Record{Kind: RecordHunk, Hunk: *continuedHunk}
			if err := emit(record); err != nil {
				return BatchResult{}, &EngineError{Kind: ErrorEmission, BatchID: request.ID, Op: "emit", Err: err}
			}
			hunks++
			continuedHunk = nil
		case TagFileEnd:
			if !inFile || continuedHunk != nil || len(frame.Payload) != 0 {
				return BatchResult{}, errors.New("invalid FEND")
			}
			inFile = false
			currentPath = nil
		case TagCommitEnd:
			if !inCommit || inFile || continuedHunk != nil || len(frame.Payload) != 0 {
				return BatchResult{}, errors.New("invalid CEND")
			}
			inCommit = false
			currentCommit = nil
		case TagBatchEnd:
			if inCommit || inFile || continuedHunk != nil {
				return BatchResult{}, errors.New("BEND with open record")
			}
			result, err := DecodeBatchEnd(frame)
			if err != nil {
				return BatchResult{}, err
			}
			if result.Status != CompletionOK {
				return BatchResult{}, errors.New("batch rejected after records may have been emitted")
			}
			if result.Commits != commits || result.Files != files || result.Hunks != hunks {
				return BatchResult{}, fmt.Errorf("BEND counters %d/%d/%d != observed %d/%d/%d", result.Commits, result.Files, result.Hunks, commits, files, hunks)
			}
			if result.Commits != uint64(len(request.Commits)) {
				return BatchResult{}, fmt.Errorf("BEND commit count %d != requested %d", result.Commits, len(request.Commits))
			}
			return result, nil
		case TagError:
			helperErr, err := DecodeError(frame)
			if err != nil {
				return BatchResult{}, err
			}
			if helperErr.Kind == ErrorUnsupported {
				return BatchResult{}, errors.New("unsupported error is only valid before HELO")
			}
			return BatchResult{}, helperErr
		default:
			return BatchResult{}, fmt.Errorf("unexpected frame %.4s", frame.Tag)
		}
	}
}

func (w *processWorker) validateNegotiatedRecord(record Record) error {
	width := int(w.capabilities.OIDLength)
	check := func(name string, oid OID, allowEmpty bool) error {
		if allowEmpty && len(oid) == 0 {
			return nil
		}
		if len(oid) != width {
			return fmt.Errorf("%s object ID length %d != negotiated %d", name, len(oid), width)
		}
		return nil
	}
	switch record.Kind {
	case RecordCommit:
		return check("commit", record.Commit.OID, false)
	case RecordFile:
		if !w.profile.AllowsStatus(record.File.Status) {
			return fmt.Errorf("status %q is not allowed by scan profile", record.File.Status)
		}
		if err := check("file commit", record.File.Commit, false); err != nil {
			return err
		}
		if err := check("old", record.File.OldOID, true); err != nil {
			return err
		}
		return check("new", record.File.NewOID, true)
	case RecordHunk:
		if uint64(len(record.Hunk.Added)) > w.config.MaxRecord {
			return fmt.Errorf("hunk exceeds record limit %d", w.config.MaxRecord)
		}
		return check("hunk commit", record.Hunk.Commit, false)
	default:
		return fmt.Errorf("record kind %d", record.Kind)
	}
}

// Close is idempotent, requests clean shutdown, and reaps the helper exactly once.
func (w *processWorker) Close() error {
	w.scanMu.Lock()
	defer w.scanMu.Unlock()
	w.mu.Lock()
	if w.closed {
		err := w.final
		w.mu.Unlock()
		return err
	}
	w.closed = true
	final := w.final
	w.mu.Unlock()
	if final != nil {
		w.interrupt()
		return final
	}
	writeErr := WriteFrame(w.stdin, Frame{Tag: TagQuit}, w.config.MaxFrame)
	closeErr := w.stdin.Close()
	waitErr := w.wait(true)
	final = errors.Join(writeErr, closeErr, waitErr)
	w.mu.Lock()
	w.final = final
	w.mu.Unlock()
	return final
}

func (w *processWorker) available() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		if w.final != nil {
			return w.final
		}
		return errors.New("git engine worker is closed")
	}
	return w.final
}

func (w *processWorker) terminal(op string, batchID uint64, kind ErrorKind, cause error) error {
	w.interrupt()
	engineErr := &EngineError{Kind: kind, BatchID: batchID, Op: op, Stderr: w.stderr.String(), Err: cause}
	w.mu.Lock()
	if w.final == nil {
		w.final = engineErr
	}
	final := w.final
	w.mu.Unlock()
	return final
}

func (w *processWorker) interrupt() {
	w.mu.Lock()
	closed := w.closed
	w.mu.Unlock()
	if !closed {
		_ = w.stdin.Close()
	}
	if w.cmd.Process != nil {
		_ = w.cmd.Process.Signal(os.Interrupt)
	}
	_ = w.wait(false)
}

func (w *processWorker) wait(clean bool) error {
	timer := time.NewTimer(w.config.KillGrace)
	defer timer.Stop()
	select {
	case <-w.waitDone:
		w.mu.Lock()
		err := w.waitErr
		w.mu.Unlock()
		if clean {
			return err
		}
		return nil
	case <-timer.C:
		if w.cmd.Process != nil {
			_ = w.cmd.Process.Kill()
		}
		<-w.waitDone
		w.mu.Lock()
		err := w.waitErr
		w.mu.Unlock()
		return err
	}
}

func errorKind(err error) ErrorKind {
	var engineErr *EngineError
	if errors.As(err, &engineErr) {
		return engineErr.Kind
	}
	return ErrorProtocol
}

type tailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func newTailBuffer(limit int) *tailBuffer { return &tailBuffer{limit: limit} }

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if n >= b.limit {
		b.data = append(b.data[:0], p[n-b.limit:]...)
		return n, nil
	}
	overflow := len(b.data) + n - b.limit
	if overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(bytes.Clone(b.data))
}
