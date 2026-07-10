package gitengine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const helperEnv = "BETTERLEAKS_GITENGINE_HELPER"

func helperCommand(scenario string) CommandBuilder {
	return func(string, ScanProfile) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=TestProcessHelper", "--", scenario)
		cmd.Env = append(os.Environ(), helperEnv+"="+scenario)
		return cmd
	}
}

func processConfig(scenario string) ProcessConfig {
	return ProcessConfig{Command: helperCommand(scenario), MaxFrame: 4 << 20, StderrTail: 128, KillGrace: 2 * time.Second}
}

func TestProcessHelper(t *testing.T) {
	scenario := os.Getenv(helperEnv)
	if scenario == "" {
		return
	}
	if err := runProcessHelper(scenario, os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(7)
	}
	os.Exit(0)
}

func runProcessHelper(scenario string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	if scenario == "unsupported" {
		return WriteFrame(stdout, ErrorFrame(0, ErrorUnsupported, "preflight", errors.New("textconv configured")), 0)
	}
	oidLength := uint16(20)
	if scenario == "sha256" {
		oidLength = 32
	}
	if err := WriteFrame(stdout, HelloFrame(Capabilities{ProtocolVersion: ProtocolVersion, OIDLength: oidLength, AllStatuses: true, FindCopies: true}), 0); err != nil {
		return err
	}
	if scenario == "early-response" {
		return runEarlyResponseHelper(stdin, stdout)
	}
	for {
		frame, err := ReadFrame(stdin, 4<<20)
		if err != nil {
			return err
		}
		if frame.Tag == TagQuit {
			return nil
		}
		request, err := DecodeBatchBegin(frame)
		if err != nil {
			return err
		}
		switch scenario {
		case "hang":
			for {
				time.Sleep(time.Hour)
			}
		case "eof":
			return nil
		case "nonzero":
			return errors.New("intentional nonzero")
		case "oversize":
			_, err := stdout.Write([]byte{0x7f, 0xff, 0xff, 0xff})
			return err
		case "bad-lifecycle":
			if err := WriteFrame(stdout, Frame{Tag: TagBatchBegin, BatchID: request.ID}, 0); err != nil {
				return err
			}
			record := Record{Kind: RecordHunk, Hunk: HunkRecord{Commit: request.Commits[0], NewPath: []byte("x"), Added: []byte("x")}}
			frame, _ := RecordFrame(request.ID, record)
			return WriteFrame(stdout, frame, 0)
		case "error":
			if err := WriteFrame(stdout, Frame{Tag: TagBatchBegin, BatchID: request.ID}, 0); err != nil {
				return err
			}
			return WriteFrame(stdout, ErrorFrame(request.ID, ErrorHelper, "lookup", errors.New("missing object")), 0)
		case "runtime-unsupported":
			if err := WriteFrame(stdout, Frame{Tag: TagBatchBegin, BatchID: request.ID}, 0); err != nil {
				return err
			}
			return WriteFrame(stdout, ErrorFrame(request.ID, ErrorUnsupported, "scan", errors.New("late unsupported feature")), 0)
		case "stderr-error":
			fmt.Fprint(stderr, stringsOf('x', 256)+"tail-marker")
			return errors.New("intentional stderr failure")
		case "records":
			if err := emitFullBatch(stdout, request); err != nil {
				return err
			}
			continue
		case "large-hunk":
			if err := emitLargeHunkBatch(stdout, request); err != nil {
				return err
			}
			continue
		case "continuation-order":
			if err := emitContinuationPrefix(stdout, request); err != nil {
				return err
			}
			return WriteFrame(stdout, Frame{Tag: TagHunkAdd, BatchID: request.ID, Payload: []byte("orphan")}, 0)
		case "continuation-truncated":
			if err := emitContinuationPrefix(stdout, request); err != nil {
				return err
			}
			hunk := HunkRecord{Commit: request.Commits[0], NewPath: []byte("large"), NewPosition: 1}
			if err := WriteFrame(stdout, Frame{Tag: TagHunkBegin, BatchID: request.ID, Payload: encodeHunkBegin(hunk)}, 0); err != nil {
				return err
			}
			if err := WriteFrame(stdout, Frame{Tag: TagHunkAdd, BatchID: request.ID, Payload: []byte("partial")}, 0); err != nil {
				return err
			}
			return errors.New("intentional truncated continuation")
		case "continuation-oversize":
			if err := emitContinuationPrefix(stdout, request); err != nil {
				return err
			}
			hunk := HunkRecord{Commit: request.Commits[0], NewPath: []byte("large"), NewPosition: 1}
			if err := WriteFrame(stdout, Frame{Tag: TagHunkBegin, BatchID: request.ID, Payload: encodeHunkBegin(hunk)}, 0); err != nil {
				return err
			}
			for range 2 {
				if err := WriteFrame(stdout, Frame{Tag: TagHunkAdd, BatchID: request.ID, Payload: bytes.Repeat([]byte{'x'}, 800)}, 0); err != nil {
					return err
				}
			}
			return nil
		case "continuation-empty":
			if err := emitContinuationPrefix(stdout, request); err != nil {
				return err
			}
			hunk := HunkRecord{Commit: request.Commits[0], NewPath: []byte("large"), NewPosition: 1}
			if err := WriteFrame(stdout, Frame{Tag: TagHunkBegin, BatchID: request.ID, Payload: encodeHunkBegin(hunk)}, 0); err != nil {
				return err
			}
			return WriteFrame(stdout, Frame{Tag: TagHunkEnd, BatchID: request.ID, Payload: []byte{0}}, 0)
		case "continuation-bad-end":
			if err := emitContinuationPrefix(stdout, request); err != nil {
				return err
			}
			hunk := HunkRecord{Commit: request.Commits[0], NewPath: []byte("large"), NewPosition: 1}
			if err := WriteFrame(stdout, Frame{Tag: TagHunkBegin, BatchID: request.ID, Payload: encodeHunkBegin(hunk)}, 0); err != nil {
				return err
			}
			if err := WriteFrame(stdout, Frame{Tag: TagHunkAdd, BatchID: request.ID, Payload: []byte("partial")}, 0); err != nil {
				return err
			}
			return WriteFrame(stdout, Frame{Tag: TagHunkEnd, BatchID: request.ID, Payload: []byte{2}}, 0)
		case "wrong-commit-width":
			if err := WriteFrame(stdout, Frame{Tag: TagBatchBegin, BatchID: request.ID}, 0); err != nil {
				return err
			}
			frame, _ := RecordFrame(request.ID, Record{Kind: RecordCommit, Commit: CommitRecord{OID: testOID(32, 1)}})
			return WriteFrame(stdout, frame, 0)
		case "wrong-file-width":
			return emitStatusBatch(stdout, request, StatusAdded, testOID(32, 2))
		case "copy-status":
			if err := emitStatusBatch(stdout, request, StatusCopied, testOID(20, 2)); err != nil {
				return err
			}
			continue
		case "delete-status":
			if err := emitStatusBatch(stdout, request, StatusDeleted, nil); err != nil {
				return err
			}
			continue
		}
		if err := emitCommitOnlyBatch(stdout, request); err != nil {
			return err
		}
	}
}

func emitStatusBatch(stdout io.Writer, request BatchRequest, status Status, newOID OID) error {
	if len(request.Commits) != 1 {
		return errors.New("status scenario expects one commit")
	}
	if err := WriteFrame(stdout, Frame{Tag: TagBatchBegin, BatchID: request.ID}, 0); err != nil {
		return err
	}
	commit, _ := RecordFrame(request.ID, Record{Kind: RecordCommit, Commit: CommitRecord{OID: request.Commits[0]}})
	if err := WriteFrame(stdout, commit, 0); err != nil {
		return err
	}
	file, err := RecordFrame(request.ID, Record{Kind: RecordFile, File: FileRecord{
		Commit: request.Commits[0], Status: status, OldOID: testOID(20, 1), NewOID: newOID,
		OldPath: []byte("old"), NewPath: []byte("new"),
	}})
	if err != nil {
		return err
	}
	if err := WriteFrame(stdout, file, 0); err != nil {
		return err
	}
	for _, tag := range []FrameTag{TagFileEnd, TagCommitEnd} {
		if err := WriteFrame(stdout, Frame{Tag: tag, BatchID: request.ID}, 0); err != nil {
			return err
		}
	}
	return WriteFrame(stdout, BatchEndFrame(BatchResult{ID: request.ID, Status: CompletionOK, Commits: 1, Files: 1}), 0)
}

func emitFullBatch(stdout io.Writer, request BatchRequest) error {
	if len(request.Commits) != 1 {
		return errors.New("records scenario expects one commit")
	}
	if err := WriteFrame(stdout, Frame{Tag: TagBatchBegin, BatchID: request.ID}, 0); err != nil {
		return err
	}
	commit := Record{Kind: RecordCommit, Commit: CommitRecord{OID: request.Commits[0], Message: []byte("message")}}
	frame, _ := RecordFrame(request.ID, commit)
	if err := WriteFrame(stdout, frame, 0); err != nil {
		return err
	}
	file := Record{Kind: RecordFile, File: FileRecord{
		Commit: request.Commits[0], Status: StatusAdded, NewMode: 0o100644,
		OldOID: testOID(20, 0), NewOID: testOID(20, 2), NewPath: []byte("path\n\xff"),
	}}
	frame, _ = RecordFrame(request.ID, file)
	if err := WriteFrame(stdout, frame, 0); err != nil {
		return err
	}
	hunk := Record{Kind: RecordHunk, Hunk: HunkRecord{
		Commit: request.Commits[0], NewPath: []byte("path\n\xff"), NewPosition: 1, Added: []byte("secret\n"),
	}}
	frame, _ = RecordFrame(request.ID, hunk)
	if err := WriteFrame(stdout, frame, 0); err != nil {
		return err
	}
	for _, tag := range []FrameTag{TagFileEnd, TagCommitEnd} {
		if err := WriteFrame(stdout, Frame{Tag: tag, BatchID: request.ID}, 0); err != nil {
			return err
		}
	}
	return WriteFrame(stdout, BatchEndFrame(BatchResult{ID: request.ID, Status: CompletionOK, Commits: 1, Files: 1, Hunks: 1}), 0)
}

func emitContinuationPrefix(stdout io.Writer, request BatchRequest) error {
	if len(request.Commits) != 1 {
		return errors.New("continuation scenario expects one commit")
	}
	if err := WriteFrame(stdout, Frame{Tag: TagBatchBegin, BatchID: request.ID}, 0); err != nil {
		return err
	}
	commit, _ := RecordFrame(request.ID, Record{Kind: RecordCommit, Commit: CommitRecord{OID: request.Commits[0]}})
	if err := WriteFrame(stdout, commit, 0); err != nil {
		return err
	}
	file, _ := RecordFrame(request.ID, Record{Kind: RecordFile, File: FileRecord{
		Commit: request.Commits[0], Status: StatusAdded, NewOID: testOID(20, 2), NewPath: []byte("large"),
	}})
	return WriteFrame(stdout, file, 0)
}

func emitLargeHunkBatch(stdout io.Writer, request BatchRequest) error {
	if err := emitContinuationPrefix(stdout, request); err != nil {
		return err
	}
	added := bytes.Repeat([]byte("0123456789abcdef"), (17<<20)/16+1)
	added = added[:17<<20]
	frames, err := HunkFrames(request.ID, HunkRecord{
		Commit: request.Commits[0], NewPath: []byte("large"), NewPosition: 7, Added: added,
		MissingFinalNewline: true,
	}, 4<<20)
	if err != nil {
		return err
	}
	for _, frame := range frames {
		if err := WriteFrame(stdout, frame, 0); err != nil {
			return err
		}
	}
	for _, tag := range []FrameTag{TagFileEnd, TagCommitEnd} {
		if err := WriteFrame(stdout, Frame{Tag: tag, BatchID: request.ID}, 0); err != nil {
			return err
		}
	}
	return WriteFrame(stdout, BatchEndFrame(BatchResult{ID: request.ID, Status: CompletionOK, Commits: 1, Files: 1, Hunks: 1}), 0)
}

func emitCommitOnlyBatch(stdout io.Writer, request BatchRequest) error {
	if err := WriteFrame(stdout, Frame{Tag: TagBatchBegin, BatchID: request.ID}, 0); err != nil {
		return err
	}
	for _, oid := range request.Commits {
		record := Record{Kind: RecordCommit, Commit: CommitRecord{OID: oid}}
		frame, err := RecordFrame(request.ID, record)
		if err != nil {
			return err
		}
		if err := WriteFrame(stdout, frame, 0); err != nil {
			return err
		}
		if err := WriteFrame(stdout, Frame{Tag: TagCommitEnd, BatchID: request.ID}, 0); err != nil {
			return err
		}
	}
	return WriteFrame(stdout, BatchEndFrame(BatchResult{ID: request.ID, Status: CompletionOK, Commits: uint64(len(request.Commits))}), 0)
}

func runEarlyResponseHelper(stdin io.Reader, stdout io.Writer) error {
	reader := bufio.NewReader(stdin)
	var header [16]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return err
	}
	length := binary.BigEndian.Uint32(header[:4])
	var tag FrameTag
	copy(tag[:], header[4:8])
	if tag != TagBatchBegin {
		return errors.New("expected BGIN")
	}
	batchID := binary.BigEndian.Uint64(header[8:16])
	var countBytes [4]byte
	if _, err := io.ReadFull(reader, countBytes[:]); err != nil {
		return err
	}
	count := binary.BigEndian.Uint32(countBytes[:])
	request := BatchRequest{ID: batchID, Commits: make([]OID, count)}
	for i := range request.Commits {
		request.Commits[i] = indexedOID(i)
	}
	if err := emitCommitOnlyBatch(stdout, request); err != nil {
		return err
	}
	remaining := int64(length) - 12 - 4
	if remaining < 0 {
		return errors.New("invalid request length")
	}
	if _, err := io.CopyN(io.Discard, reader, remaining); err != nil {
		return err
	}
	frame, err := ReadFrame(reader, 4<<20)
	if err != nil {
		return err
	}
	if frame.Tag != TagQuit {
		return errors.New("expected QUIT")
	}
	return nil
}

func stringsOf(value byte, n int) string { return string(bytes.Repeat([]byte{value}, n)) }

func indexedOID(index int) OID {
	oid := make(OID, 20)
	binary.BigEndian.PutUint64(oid[12:], uint64(index))
	return oid
}

func TestProcessFactoryPreflightBarrierAndCommitOnlyBatch(t *testing.T) {
	factory, err := NewProcessFactory(processConfig("normal"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := factory.Open(t.Context(), "repo", ProductionProfile()); err == nil {
		t.Fatal("Open succeeded before Preflight")
	}
	capabilities, err := factory.Preflight(t.Context(), "repo", ProductionProfile())
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.OIDLength != 20 {
		t.Fatalf("OIDLength = %d", capabilities.OIDLength)
	}
	worker, err := factory.Open(t.Context(), "repo", ProductionProfile())
	if err != nil {
		t.Fatal(err)
	}
	request := BatchRequest{ID: 1, Commits: []OID{testOID(20, 1), testOID(20, 2)}}
	var got []Record
	result, err := worker.ScanBatch(t.Context(), request, func(record Record) error {
		got = append(got, record)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Commits != 2 || len(got) != 2 {
		t.Fatalf("result=%#v records=%d", result, len(got))
	}
	empty, err := worker.ScanBatch(t.Context(), BatchRequest{ID: 2}, func(Record) error { return nil })
	if err != nil || empty.Status != CompletionOK || empty.Commits != 0 {
		t.Fatalf("empty batch result=%#v error=%v", empty, err)
	}
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
	if err := worker.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestProcessPreflightUnsupportedIsSafeFallback(t *testing.T) {
	factory, err := NewProcessFactory(processConfig("unsupported"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = factory.Preflight(t.Context(), "repo", ProductionProfile())
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Preflight error = %v, want ErrUnsupported", err)
	}
	if _, err := factory.Open(t.Context(), "repo", ProductionProfile()); err == nil {
		t.Fatal("Open succeeded after unsupported preflight")
	}
}

func TestProcessRejectsDuplicateBeforeWorkerIO(t *testing.T) {
	w, err := startProcess(t.Context(), processConfig("normal"), "repo", ProductionProfile())
	if err != nil {
		t.Fatal(err)
	}
	oid := testOID(20, 1)
	_, err = w.ScanBatch(t.Context(), BatchRequest{ID: 1, Commits: []OID{oid, bytes.Clone(oid)}}, func(Record) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate batch error = %v", err)
	}
	result, err := w.ScanBatch(t.Context(), BatchRequest{ID: 2, Commits: []OID{oid}}, func(Record) error { return nil })
	if err != nil || result.Commits != 1 {
		t.Fatalf("valid batch after rejection result=%#v error=%v", result, err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessNegotiatedOIDWidth(t *testing.T) {
	w, err := startProcess(t.Context(), processConfig("sha256"), "repo", ProductionProfile())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.ScanBatch(t.Context(), BatchRequest{ID: 1, Commits: []OID{testOID(20, 1)}}, func(Record) error { return nil }); err == nil {
		t.Fatal("20-byte request accepted by SHA-256 worker")
	}
	result, err := w.ScanBatch(t.Context(), BatchRequest{ID: 2, Commits: []OID{testOID(32, 2)}}, func(Record) error { return nil })
	if err != nil || result.Commits != 1 {
		t.Fatalf("SHA-256 result=%#v error=%v", result, err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessRejectsRecordOIDWidthMismatch(t *testing.T) {
	for _, scenario := range []string{"wrong-commit-width", "wrong-file-width"} {
		t.Run(scenario, func(t *testing.T) {
			w, err := startProcess(t.Context(), processConfig(scenario), "repo", ProductionProfile())
			if err != nil {
				t.Fatal(err)
			}
			_, err = w.ScanBatch(t.Context(), BatchRequest{ID: 1, Commits: []OID{testOID(20, 1)}}, func(Record) error { return nil })
			if err == nil || !strings.Contains(err.Error(), "negotiated") {
				t.Fatalf("ScanBatch error = %v", err)
			}
		})
	}
}

func TestProcessEnforcesProfileStatuses(t *testing.T) {
	tests := []struct {
		name, scenario string
		profile        ScanProfile
		wantErr        bool
	}{
		{name: "production rejects copy", scenario: "copy-status", profile: ProductionProfile(), wantErr: true},
		{name: "copy profile accepts copy", scenario: "copy-status", profile: ScanProfile{FindCopies: true}},
		{name: "production rejects delete", scenario: "delete-status", profile: ProductionProfile(), wantErr: true},
		{name: "all-status profile accepts delete", scenario: "delete-status", profile: ScanProfile{AllStatuses: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, err := startProcess(t.Context(), processConfig(tt.scenario), "repo", tt.profile)
			if err != nil {
				t.Fatal(err)
			}
			_, err = w.ScanBatch(t.Context(), BatchRequest{ID: 1, Commits: []OID{testOID(20, 1)}}, func(Record) error { return nil })
			if (err != nil) != tt.wantErr {
				t.Fatalf("ScanBatch error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestProcessConcurrentPipesDoNotDeadlock(t *testing.T) {
	w, err := startProcess(t.Context(), processConfig("early-response"), "repo", ProductionProfile())
	if err != nil {
		t.Fatal(err)
	}
	commits := make([]OID, 4096)
	for i := range commits {
		commits[i] = indexedOID(i)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := w.ScanBatch(ctx, BatchRequest{ID: 99, Commits: commits}, func(Record) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.Commits != uint64(len(commits)) {
		t.Fatalf("commits = %d", result.Commits)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessFullLifecycleAndBufferOwnership(t *testing.T) {
	w, err := startProcess(t.Context(), processConfig("records"), "repo", ProductionProfile())
	if err != nil {
		t.Fatal(err)
	}
	var records []Record
	result, err := w.ScanBatch(t.Context(), BatchRequest{ID: 8, Commits: []OID{testOID(20, 1)}}, func(record Record) error {
		records = append(records, record)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Files != 1 || result.Hunks != 1 || len(records) != 3 {
		t.Fatalf("result=%#v records=%#v", result, records)
	}
	if string(records[0].Commit.Message) != "message" || string(records[2].Hunk.Added) != "secret\n" {
		t.Fatalf("decoded buffers changed: %#v", records)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessTerminalFailures(t *testing.T) {
	tests := []struct {
		name, scenario string
		kind           ErrorKind
	}{
		{name: "bad lifecycle", scenario: "bad-lifecycle", kind: ErrorProtocol},
		{name: "oversize", scenario: "oversize", kind: ErrorProtocol},
		{name: "EOF", scenario: "eof", kind: ErrorProtocol},
		{name: "error frame", scenario: "error", kind: ErrorHelper},
		{name: "runtime unsupported", scenario: "runtime-unsupported", kind: ErrorProtocol},
		{name: "nonzero", scenario: "nonzero", kind: ErrorProtocol},
		{name: "stderr bounded", scenario: "stderr-error", kind: ErrorProtocol},
		{name: "continuation order", scenario: "continuation-order", kind: ErrorProtocol},
		{name: "continuation truncated", scenario: "continuation-truncated", kind: ErrorProtocol},
		{name: "continuation without add", scenario: "continuation-empty", kind: ErrorProtocol},
		{name: "continuation malformed end", scenario: "continuation-bad-end", kind: ErrorProtocol},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, err := startProcess(t.Context(), processConfig(tt.scenario), "repo", ProductionProfile())
			if err != nil {
				t.Fatal(err)
			}
			_, err = w.ScanBatch(t.Context(), BatchRequest{ID: 1, Commits: []OID{testOID(20, 1)}}, func(Record) error { return nil })
			if err == nil {
				t.Fatal("ScanBatch succeeded")
			}
			var engineErr *EngineError
			if !errors.As(err, &engineErr) {
				t.Fatalf("error type = %T: %v", err, err)
			}
			if engineErr.Kind != tt.kind {
				t.Fatalf("kind = %d, want %d", engineErr.Kind, tt.kind)
			}
			if len(engineErr.Stderr) > 128 {
				t.Fatalf("stderr tail length = %d", len(engineErr.Stderr))
			}
		})
	}
}

func TestProcessLargeHunkContinuation(t *testing.T) {
	w, err := startProcess(t.Context(), processConfig("large-hunk"), "repo", ProductionProfile())
	if err != nil {
		t.Fatal(err)
	}
	var got HunkRecord
	result, err := w.ScanBatch(t.Context(), BatchRequest{ID: 10, Commits: []OID{testOID(20, 1)}}, func(record Record) error {
		if record.Kind == RecordHunk {
			got = record.Hunk
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Hunks != 1 || len(got.Added) != 17<<20 || got.NewPosition != 7 || !got.MissingFinalNewline {
		t.Fatalf("result=%#v hunk bytes=%d position=%d", result, len(got.Added), got.NewPosition)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessContinuationRecordLimit(t *testing.T) {
	config := processConfig("continuation-oversize")
	config.MaxRecord = 1024
	w, err := startProcess(t.Context(), config, "repo", ProductionProfile())
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.ScanBatch(t.Context(), BatchRequest{ID: 11, Commits: []OID{testOID(20, 1)}}, func(Record) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "record limit") {
		t.Fatalf("ScanBatch error = %v", err)
	}
}

func TestProcessEmissionFailureAndCancellationAreTerminal(t *testing.T) {
	w, err := startProcess(t.Context(), processConfig("normal"), "repo", ProductionProfile())
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("stop emission")
	_, err = w.ScanBatch(t.Context(), BatchRequest{ID: 1, Commits: []OID{testOID(20, 1)}}, func(Record) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("ScanBatch error = %v", err)
	}
	if _, err := w.ScanBatch(t.Context(), BatchRequest{ID: 2, Commits: []OID{testOID(20, 2)}}, func(Record) error { return nil }); err == nil {
		t.Fatal("terminal worker accepted another batch")
	}

	hanging, err := startProcess(t.Context(), processConfig("hang"), "repo", ProductionProfile())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err = hanging.ScanBatch(ctx, BatchRequest{ID: 3, Commits: []OID{testOID(20, 3)}}, func(Record) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestProcessCooperativeCancellationWaitsForEmitter(t *testing.T) {
	w, err := startProcess(t.Context(), processConfig("normal"), "repo", ProductionProfile())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	emitterStarted := make(chan struct{})
	emitterDone := make(chan struct{})
	go func() {
		<-emitterStarted
		cancel()
	}()
	_, err = w.ScanBatch(ctx, BatchRequest{ID: 4, Commits: []OID{testOID(20, 4)}}, func(Record) error {
		defer close(emitterDone)
		close(emitterStarted)
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ScanBatch error = %v", err)
	}
	select {
	case <-emitterDone:
	case <-time.After(time.Second):
		t.Fatal("emitter remained live after cancellation")
	}
}
