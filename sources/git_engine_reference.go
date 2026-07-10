package sources

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/betterleaks/betterleaks/internal/gitengine"
)

// GitReferenceFactory creates canonical stock-Git workers for correctness
// comparisons. It is additive and is not used by ParallelGit production
// scans. Version 1 intentionally supports only the production A/M/R profile;
// the test-only copy/all-status profile is a preflight ErrUnsupported until its
// patch projection and scanner semantics have an independent oracle.
type GitReferenceFactory struct {
	mu         sync.Mutex
	preflight  bool
	repoPath   string
	profile    gitengine.ScanProfile
	capability gitengine.Capabilities
}

// NewGitReferenceFactory returns a stock-Git reference factory.
func NewGitReferenceFactory() *GitReferenceFactory { return &GitReferenceFactory{} }

// Preflight validates the repository object format and the pinned v1 profile.
func (f *GitReferenceFactory) Preflight(ctx context.Context, repoPath string, profile gitengine.ScanProfile) (gitengine.Capabilities, error) {
	if profile.AllStatuses || profile.FindCopies {
		return gitengine.Capabilities{}, fmt.Errorf("reference test-only status profile: %w", gitengine.ErrUnsupported)
	}
	cmd := exec.CommandContext(ctx, gitBinary(), "-C", repoPath, "rev-parse", "--show-object-format")
	cmd.Env = referenceGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return gitengine.Capabilities{}, fmt.Errorf("git object format: %w", err)
	}
	var oidLength uint16
	switch strings.TrimSpace(string(out)) {
	case "sha1":
		oidLength = 20
	case "sha256":
		oidLength = 32
	default:
		return gitengine.Capabilities{}, fmt.Errorf("git object format %q: %w", strings.TrimSpace(string(out)), gitengine.ErrUnsupported)
	}
	capabilities := gitengine.Capabilities{ProtocolVersion: gitengine.ProtocolVersion, OIDLength: oidLength}
	f.mu.Lock()
	f.preflight = true
	f.repoPath = repoPath
	f.profile = profile
	f.capability = capabilities
	f.mu.Unlock()
	return capabilities, nil
}

// Open returns a reference worker only after matching preflight succeeds.
func (f *GitReferenceFactory) Open(_ context.Context, repoPath string, profile gitengine.ScanProfile) (gitengine.Worker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.preflight || f.repoPath != repoPath || f.profile != profile {
		return nil, errors.New("stock Git reference preflight has not completed for repository and profile")
	}
	return &gitReferenceWorker{repoPath: repoPath, profile: profile, oidLength: int(f.capability.OIDLength)}, nil
}

type gitReferenceWorker struct {
	repoPath  string
	profile   gitengine.ScanProfile
	oidLength int

	mu     sync.Mutex
	closed bool
}

func (w *gitReferenceWorker) ScanBatch(ctx context.Context, request gitengine.BatchRequest, emit gitengine.EmitFunc) (gitengine.BatchResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return gitengine.BatchResult{}, errors.New("stock Git reference worker is closed")
	}
	if emit == nil {
		return gitengine.BatchResult{}, errors.New("stock Git reference emit callback is nil")
	}
	if err := request.Validate(w.oidLength); err != nil {
		return gitengine.BatchResult{}, err
	}
	if len(request.Commits) == 0 {
		return gitengine.BatchResult{ID: request.ID, Status: gitengine.CompletionOK}, nil
	}
	commitHex := make([]string, len(request.Commits))
	for i, oid := range request.Commits {
		commitHex[i] = hex.EncodeToString(oid)
	}
	rawByCommit, err := w.rawBatch(ctx, commitHex)
	if err != nil {
		return gitengine.BatchResult{}, &gitengine.EngineError{Kind: gitengine.ErrorHelper, BatchID: request.ID, Op: "raw diff", Err: err}
	}
	metadata, err := w.metadata(ctx, commitHex)
	if err != nil {
		return gitengine.BatchResult{}, &gitengine.EngineError{Kind: gitengine.ErrorHelper, BatchID: request.ID, Op: "commit metadata", Err: err}
	}
	var (
		currentCommit string
		requestIndex  int
		rawIndex      map[string]int
		commits       uint64
		files         uint64
		hunks         uint64
	)
	rawIndex = make(map[string]int, len(request.Commits))
	emitCommitsThrough := func(target string) error {
		for requestIndex < len(commitHex) {
			sha := commitHex[requestIndex]
			record, ok := metadata[sha]
			if !ok {
				return fmt.Errorf("metadata missing requested commit %s", sha)
			}
			if err := emit(gitengine.Record{Kind: gitengine.RecordCommit, Commit: record}); err != nil {
				return err
			}
			commits++
			requestIndex++
			if sha == target {
				return nil
			}
		}
		if target != "" {
			return fmt.Errorf("patch emitted unrequested or reordered commit %s", target)
		}
		return nil
	}
	err = w.scanPatch(ctx, commitHex, func(header *fastGitHeader) error {
		if err := emitCommitsThrough(header.sha); err != nil {
			return err
		}
		currentCommit = header.sha
		return nil
	}, func(file fastGitFile) error {
		if currentCommit == "" || file.header == nil || file.header.sha != currentCommit {
			return errors.New("patch file without current commit header")
		}
		index := rawIndex[currentCommit]
		raw := rawByCommit[currentCommit]
		if index >= len(raw) {
			return fmt.Errorf("patch emitted extra file %q for %s", file.newName, currentCommit)
		}
		fileRecord := raw[index]
		rawIndex[currentCommit] = index + 1
		if !bytes.Equal(fileRecord.NewPath, []byte(file.newName)) {
			return fmt.Errorf("patch path %q != raw path %q for %s", file.newName, fileRecord.NewPath, currentCommit)
		}
		fileRecord.Binary = file.isBinary
		if err := emit(gitengine.Record{Kind: gitengine.RecordFile, File: fileRecord}); err != nil {
			return err
		}
		files++
		for _, fragment := range file.fragments {
			hunk := gitengine.Record{Kind: gitengine.RecordHunk, Hunk: gitengine.HunkRecord{
				Commit: fileRecord.Commit, NewPath: bytes.Clone(fileRecord.NewPath),
				NewPosition: uint64(fragment.newPosition), Added: []byte(fragment.raw),
				MissingFinalNewline: fragment.missingFinalNewline,
			}}
			if err := emit(hunk); err != nil {
				return err
			}
			hunks++
		}
		return nil
	})
	if err != nil {
		return gitengine.BatchResult{}, &gitengine.EngineError{Kind: gitengine.ErrorEmission, BatchID: request.ID, Op: "parse reference", Err: err}
	}
	if err := emitCommitsThrough(""); err != nil {
		return gitengine.BatchResult{}, &gitengine.EngineError{Kind: gitengine.ErrorEmission, BatchID: request.ID, Op: "emit commit", Err: err}
	}
	if commits != uint64(len(request.Commits)) {
		return gitengine.BatchResult{}, &gitengine.EngineError{Kind: gitengine.ErrorProtocol, BatchID: request.ID, Op: "commit coverage", Err: fmt.Errorf("emitted %d of %d", commits, len(request.Commits))}
	}
	for commit, raw := range rawByCommit {
		if rawIndex[commit] != len(raw) {
			return gitengine.BatchResult{}, &gitengine.EngineError{Kind: gitengine.ErrorProtocol, BatchID: request.ID, Op: "file coverage", Err: fmt.Errorf("commit %s emitted %d of %d files", commit, rawIndex[commit], len(raw))}
		}
	}
	return gitengine.BatchResult{ID: request.ID, Status: gitengine.CompletionOK, Commits: commits, Files: files, Hunks: hunks}, nil
}

func (w *gitReferenceWorker) metadata(ctx context.Context, commits []string) (map[string]gitengine.CommitRecord, error) {
	args := []string{"-C", w.repoPath, "-c", "core.quotePath=true", "log",
		"--no-walk=unsorted", "--stdin", "--no-patch", "--pretty=medium", "--encoding=UTF-8",
		"--use-mailmap", "--date=default", "--no-decorate", "--no-notes", "--no-show-signature"}
	cmd := exec.CommandContext(ctx, gitBinary(), args...)
	cmd.Env = referenceGitEnv()
	cmd.Stdin = strings.NewReader(strings.Join(commits, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		return nil, commandError(err)
	}
	records := make(map[string]gitengine.CommitRecord, len(commits))
	err = parseFastGitLogRecords(bytes.NewReader(out), func(header *fastGitHeader) error {
		oid, err := hex.DecodeString(header.sha)
		if err != nil || len(oid) != w.oidLength {
			return fmt.Errorf("metadata object ID %q", header.sha)
		}
		_, offsetSeconds := header.authorDate.Zone()
		hasAuthorTime := !header.authorDate.IsZero()
		record := gitengine.CommitRecord{
			OID: oid, Message: []byte(header.message), AuthorName: []byte(header.author.name),
			AuthorEmail: []byte(header.author.email), HasAuthor: header.author.valid,
			HasAuthorTime: hasAuthorTime,
		}
		if hasAuthorTime {
			record.AuthorUnixSeconds = header.authorDate.Unix()
			record.AuthorUTCOffsetMinutes = int32(offsetSeconds / 60)
		}
		records[header.sha] = record
		return nil
	}, func(fastGitFile) error { return errors.New("metadata command emitted a patch file") })
	if err != nil {
		return nil, err
	}
	if len(records) != len(commits) {
		return nil, fmt.Errorf("metadata emitted %d of %d commits", len(records), len(commits))
	}
	return records, nil
}

func (w *gitReferenceWorker) Close() error {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	return nil
}

func (w *gitReferenceWorker) scanPatch(ctx context.Context, commits []string, emitHeader func(*fastGitHeader) error, emitFile func(fastGitFile) error) error {
	args := []string{"-C", w.repoPath, "-c", "core.quotePath=true", "log",
		"-p", "-U0", "--no-walk=unsorted", "--stdin", "--diff-filter=tuxdb",
		"--pretty=medium", "--encoding=UTF-8", "--root", "--no-diff-merges", "-M50%",
		"--full-index", "--src-prefix=a/", "--dst-prefix=b/", "--no-color", "--no-ext-diff",
		"--textconv", "--diff-algorithm=myers", "--indent-heuristic", "--use-mailmap",
		"--date=default", "--no-decorate", "--no-notes", "--no-show-signature"}
	return runReferenceGitStream(ctx, args, commits, func(stdout io.Reader) error {
		return parseFastGitLogRecords(stdout, emitHeader, emitFile)
	})
}

func (w *gitReferenceWorker) rawBatch(ctx context.Context, commits []string) (map[string][]gitengine.FileRecord, error) {
	args := []string{"-C", w.repoPath, "-c", "core.quotePath=true", "log",
		"--no-walk=unsorted", "--stdin", "--raw", "-z", "--full-index", "--no-abbrev",
		"--pretty=format:%x00%H%x00", "--root", "--no-diff-merges", "-M50%", "--no-ext-diff",
		"--diff-filter=tuxdb", "--diff-algorithm=myers", "--indent-heuristic"}
	var records map[string][]gitengine.FileRecord
	err := runReferenceGitStream(ctx, args, commits, func(stdout io.Reader) error {
		var err error
		records, err = parseRawLog(stdout, w.oidLength, commits)
		return err
	})
	return records, err
}

const maxRawToken = 1 << 20

func parseRawLog(r io.Reader, oidLength int, expected []string) (map[string][]gitengine.FileRecord, error) {
	reader := bufio.NewReaderSize(r, 64<<10)
	records := make(map[string][]gitengine.FileRecord, len(expected))
	positions := make(map[string]int, len(expected))
	for i, sha := range expected {
		records[sha] = nil
		positions[sha] = i
	}
	marker, terminated, err := readRawToken(reader, maxRawToken)
	if errors.Is(err, io.EOF) {
		return records, nil
	}
	if err != nil {
		return nil, err
	}
	if !terminated || len(marker) != 0 {
		return nil, errors.New("raw log does not begin with NUL commit marker")
	}
	lastPosition := -1
	for {
		shaToken, terminated, err := readRawToken(reader, maxRawToken)
		if err != nil {
			return nil, err
		}
		sha := string(shaToken)
		position, ok := positions[sha]
		if !terminated || !ok || position <= lastPosition {
			return nil, fmt.Errorf("raw commit %q is not an ordered requested commit", shaToken)
		}
		lastPosition = position
		commitOID, err := hex.DecodeString(string(shaToken))
		if err != nil || len(commitOID) != oidLength {
			return nil, fmt.Errorf("raw commit object ID %q", shaToken)
		}
		var files []gitengine.FileRecord
		for {
			token, terminated, err := readRawToken(reader, maxRawToken)
			if errors.Is(err, io.EOF) {
				records[sha] = files
				return records, nil
			}
			if err != nil {
				return nil, err
			}
			token = bytes.TrimPrefix(token, []byte{'\n'})
			if len(token) == 0 {
				records[sha] = files
				if !terminated {
					return records, nil
				}
				break
			}
			if !terminated {
				return nil, io.ErrUnexpectedEOF
			}
			record, rename, err := parseRawMetadata(token, oidLength)
			if err != nil {
				return nil, err
			}
			record.Commit = bytes.Clone(commitOID)
			path, pathTerminated, err := readRawToken(reader, maxRawToken)
			if err != nil || !pathTerminated {
				if err == nil {
					err = io.ErrUnexpectedEOF
				}
				return nil, err
			}
			record.OldPath = path
			record.NewPath = bytes.Clone(path)
			if rename {
				newPath, newPathTerminated, err := readRawToken(reader, maxRawToken)
				if err != nil || !newPathTerminated {
					if err == nil {
						err = io.ErrUnexpectedEOF
					}
					return nil, err
				}
				record.NewPath = newPath
			}
			if gitengine.ProductionStatus(record.Status) {
				files = append(files, record)
			}
		}
		marker, markerTerminated, err := readRawToken(reader, maxRawToken)
		if errors.Is(err, io.EOF) {
			return records, nil
		}
		if err != nil || !markerTerminated || len(marker) != 0 {
			if err == nil {
				err = errors.New("raw log missing NUL commit marker")
			}
			return nil, err
		}
	}
}

func readRawToken(reader *bufio.Reader, max int) ([]byte, bool, error) {
	var token []byte
	for {
		part, err := reader.ReadSlice(0)
		if len(part) > 0 {
			terminated := part[len(part)-1] == 0
			if terminated {
				part = part[:len(part)-1]
			}
			if len(token)+len(part) > max {
				return nil, false, fmt.Errorf("raw token exceeds limit %d", max)
			}
			token = append(token, part...)
			if terminated {
				return token, true, nil
			}
		}
		switch err {
		case nil:
			continue
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if len(token) == 0 {
				return nil, false, io.EOF
			}
			return token, false, nil
		default:
			return nil, false, err
		}
	}
}

func parseRawDiffTree(data []byte, oidLength int) ([]gitengine.FileRecord, error) {
	tokens := bytes.Split(data, []byte{0})
	var records []gitengine.FileRecord
	for i := 0; i < len(tokens)-1; {
		meta := tokens[i]
		i++
		record, rename, err := parseRawMetadata(meta, oidLength)
		if err != nil {
			return nil, err
		}
		if i >= len(tokens)-1 {
			return nil, errors.New("raw diff missing path")
		}
		record.OldPath = bytes.Clone(tokens[i])
		record.NewPath = bytes.Clone(tokens[i])
		i++
		if rename {
			if i >= len(tokens)-1 {
				return nil, errors.New("raw rename missing destination path")
			}
			record.NewPath = bytes.Clone(tokens[i])
			i++
		}
		records = append(records, record)
	}
	return records, nil
}

func parseRawMetadata(meta []byte, oidLength int) (gitengine.FileRecord, bool, error) {
	fields := bytes.Fields(meta)
	if len(fields) != 5 || len(fields[0]) < 2 || fields[0][0] != ':' || len(fields[4]) < 1 {
		return gitengine.FileRecord{}, false, fmt.Errorf("invalid raw diff metadata %q", meta)
	}
	oldMode, err := strconv.ParseUint(string(fields[0][1:]), 8, 32)
	if err != nil {
		return gitengine.FileRecord{}, false, fmt.Errorf("old mode %q: %w", fields[0], err)
	}
	newMode, err := strconv.ParseUint(string(fields[1]), 8, 32)
	if err != nil {
		return gitengine.FileRecord{}, false, fmt.Errorf("new mode %q: %w", fields[1], err)
	}
	oldOID, err := hex.DecodeString(string(fields[2]))
	if err != nil || len(oldOID) != oidLength {
		return gitengine.FileRecord{}, false, fmt.Errorf("old object ID %q", fields[2])
	}
	newOID, err := hex.DecodeString(string(fields[3]))
	if err != nil || len(newOID) != oidLength {
		return gitengine.FileRecord{}, false, fmt.Errorf("new object ID %q", fields[3])
	}
	status := gitengine.Status(fields[4][0])
	return gitengine.FileRecord{
		Status: status, OldMode: uint32(oldMode), NewMode: uint32(newMode),
		OldOID: oldOID, NewOID: newOID,
	}, status == gitengine.StatusRenamed || status == gitengine.StatusCopied, nil
}

func runReferenceGitStream(ctx context.Context, args, commits []string, consume func(io.Reader) error) error {
	cmd := exec.CommandContext(ctx, gitBinary(), args...)
	cmd.Env = referenceGitEnv()
	cmd.Stdin = strings.NewReader(strings.Join(commits, "\n") + "\n")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := &referenceTail{limit: 64 << 10}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	consumeErr := consume(stdout)
	if consumeErr != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if consumeErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return consumeErr
	}
	if waitErr != nil {
		if tail := stderr.String(); tail != "" {
			return fmt.Errorf("%w: %s", waitErr, tail)
		}
		return waitErr
	}
	return nil
}

type referenceTail struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (b *referenceTail) Write(p []byte) (int, error) {
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

func (b *referenceTail) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(bytes.TrimSpace(b.data))
}

func referenceGitEnv() []string {
	return gitConfigIsolationEnv()
}

func commandError(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(exitErr.Stderr))
	}
	return err
}
