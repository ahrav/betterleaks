package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/betterleaks/betterleaks/internal/gitengine"
	"github.com/betterleaks/betterleaks/sources"
)

type fakeWorker struct {
	mu       sync.Mutex
	batches  int
	closes   int
	closeErr error
	scan     func(context.Context, gitengine.BatchRequest, gitengine.EmitFunc) (gitengine.BatchResult, error)
}

func (w *fakeWorker) ScanBatch(ctx context.Context, request gitengine.BatchRequest, emit gitengine.EmitFunc) (gitengine.BatchResult, error) {
	w.mu.Lock()
	w.batches++
	w.mu.Unlock()
	if w.scan != nil {
		return w.scan(ctx, request, emit)
	}
	for _, oid := range request.Commits {
		if err := emit(gitengine.Record{Kind: gitengine.RecordCommit, Commit: gitengine.CommitRecord{OID: oid}}); err != nil {
			return gitengine.BatchResult{}, err
		}
	}
	return gitengine.BatchResult{ID: request.ID, Status: gitengine.CompletionOK, Commits: uint64(len(request.Commits))}, nil
}

func (w *fakeWorker) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closes++
	return w.closeErr
}

func (w *fakeWorker) counts() (int, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.batches, w.closes
}

type fakeFactory struct {
	mu      sync.Mutex
	workers []gitengine.Worker
	openErr error
	opens   int
}

func (f *fakeFactory) Preflight(context.Context, string, gitengine.ScanProfile) (gitengine.Capabilities, error) {
	return gitengine.Capabilities{}, nil
}

func (f *fakeFactory) Open(context.Context, string, gitengine.ScanProfile) (gitengine.Worker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	if f.openErr != nil {
		return nil, f.openErr
	}
	if len(f.workers) == 0 {
		return nil, errors.New("unexpected fake worker open")
	}
	worker := f.workers[0]
	f.workers = f.workers[1:]
	return worker, nil
}

func oneCommitBatches(n int) [][]gitengine.OID {
	batches := make([][]gitengine.OID, n)
	for i := range batches {
		oid := bytes.Repeat([]byte{byte(i + 1)}, 20)
		batches[i] = []gitengine.OID{oid}
	}
	return batches
}

func TestDigestIsOrderIndependentAndRetainsMultiplicity(t *testing.T) {
	a := gitengine.Record{Kind: gitengine.RecordCommit, Commit: gitengine.CommitRecord{OID: bytes.Repeat([]byte{1}, 20)}}
	b := gitengine.Record{Kind: gitengine.RecordCommit, Commit: gitengine.CommitRecord{OID: bytes.Repeat([]byte{2}, 20)}}
	var left, right, duplicate digest
	for _, record := range []gitengine.Record{a, b} {
		if err := left.add(record); err != nil {
			t.Fatal(err)
		}
	}
	for _, record := range []gitengine.Record{b, a} {
		if err := right.add(record); err != nil {
			t.Fatal(err)
		}
	}
	for _, record := range []gitengine.Record{a, b, b} {
		if err := duplicate.add(record); err != nil {
			t.Fatal(err)
		}
	}
	if left.String() != right.String() || left.Records != right.Records {
		t.Fatal("digest depends on record order")
	}
	if left.String() == duplicate.String() || left.Records == duplicate.Records {
		t.Fatal("digest hid duplicate multiplicity")
	}
}

func TestBuildBatchesCoversInputWithoutOverlap(t *testing.T) {
	commits := make([]gitengine.OID, 1041)
	for i := range commits {
		commits[i] = gitengine.OID{byte(i >> 8), byte(i)}
	}
	for _, cap := range []int{1, 15, 17, 20, 31, 33, 63, 128} {
		parts := buildBatches(commits, 4, cap)
		var got []gitengine.OID
		for _, part := range parts {
			if len(part) > cap {
				t.Fatalf("cap %d: batch length = %d", cap, len(part))
			}
			got = append(got, part...)
		}
		if len(got) != len(commits) {
			t.Fatalf("cap %d: coverage = %d, want %d", cap, len(got), len(commits))
		}
		seen := make(map[string]bool, len(got))
		for _, oid := range got {
			if seen[string(oid)] {
				t.Fatalf("cap %d: duplicate %x", cap, oid)
			}
			seen[string(oid)] = true
		}
		for _, oid := range commits {
			if !seen[string(oid)] {
				t.Fatalf("cap %d: missing %x", cap, oid)
			}
		}
	}
}

func TestSampleCommitRunsIsDeterministicAndUnique(t *testing.T) {
	commits := make([]gitengine.OID, 1000)
	for i := range commits {
		commits[i] = gitengine.OID{byte(i >> 8), byte(i)}
	}
	left := sampleCommitRuns(commits, 101)
	right := sampleCommitRuns(commits, 101)
	if len(left) != 101 || len(right) != 101 {
		t.Fatalf("sample lengths = %d/%d, want 101", len(left), len(right))
	}
	seen := make(map[string]bool, len(left))
	for i := range left {
		if !bytes.Equal(left[i], right[i]) {
			t.Fatalf("sample differs at %d", i)
		}
		if seen[string(left[i])] {
			t.Fatalf("duplicate sample %x", left[i])
		}
		seen[string(left[i])] = true
	}
	if bytes.Equal(left[:16][0], left[16]) {
		t.Fatal("sample did not stride between contiguous runs")
	}
}

func TestValidateOptionsRejectsMislabelledPackedOverrides(t *testing.T) {
	base := options{repo: ".", workers: 1}
	for _, engine := range []string{"reference", "libgit2", "gix"} {
		opts := base
		opts.engine = engine
		opts.packedWindow = "16m"
		if err := validateOptions(opts); err == nil {
			t.Fatalf("engine %s accepted packed override", engine)
		}
	}
	opts := base
	opts.engine = "custom-git"
	opts.packedWindow = "16m"
	opts.packedLimit = "256m"
	if err := validateOptions(opts); err != nil {
		t.Fatalf("custom Git options: %v", err)
	}
}

func TestCustomGitCommandMatchesProductionEnvironmentAndComposesPackedOverrides(t *testing.T) {
	t.Setenv("GIT_DIFF_OPTS", "-U99")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'diff.algorithm=patience'")
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "core.deltaBaseCacheLimit")
	t.Setenv("GIT_CONFIG_VALUE_0", "1g")
	t.Setenv("GIT_CONFIG_KEY_1", "diff.algorithm")
	t.Setenv("GIT_CONFIG_VALUE_1", "patience")
	opts := options{
		helper:       "/tmp/patched-git",
		packedWindow: "64m",
		packedLimit:  "512m",
	}
	cmd := customGitCommand(opts, "/tmp/repository")
	wantArgs := []string{
		opts.helper,
		"-c", "core.packedGitWindowSize=64m",
		"-c", "core.packedGitLimit=512m",
		"-C", "/tmp/repository", "betterleaks--diff-engine", "--protocol=1",
	}
	if !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Fatalf("argv = %#v, want %#v", cmd.Args, wantArgs)
	}
	gotEnv := environmentMap(cmd.Env)
	wantEnv := environmentMap(sources.GitConfigIsolationEnv())
	if !reflect.DeepEqual(gotEnv, wantEnv) {
		t.Fatalf("custom Git environment differs from production Git environment")
	}
	if _, inherited := gotEnv["GIT_DIFF_OPTS"]; inherited {
		t.Fatal("custom Git environment inherited GIT_DIFF_OPTS")
	}
	for _, key := range []string{"GIT_CONFIG_PARAMETERS", "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1"} {
		if _, inherited := gotEnv[key]; inherited {
			t.Fatalf("custom Git environment inherited %s", key)
		}
	}
	for key, want := range map[string]string{
		"GIT_CONFIG_GLOBAL":      os.DevNull,
		"GIT_CONFIG_NOSYSTEM":    "1",
		"GIT_CONFIG_SYSTEM":      os.DevNull,
		"GIT_NO_REPLACE_OBJECTS": "1",
		"GIT_TERMINAL_PROMPT":    "0",
		"GIT_CONFIG_COUNT":       "1",
		"GIT_CONFIG_KEY_0":       "core.deltaBaseCacheLimit",
		"GIT_CONFIG_VALUE_0":     "128m",
	} {
		if gotEnv[key] != want {
			t.Errorf("%s = %q, want %q", key, gotEnv[key], want)
		}
	}
}

func TestCustomGitCommandDefaultArgv(t *testing.T) {
	opts := options{helper: "patched-git"}
	cmd := customGitCommand(opts, "repository")
	want := []string{"patched-git", "-C", "repository", "betterleaks--diff-engine", "--protocol=1"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("argv = %#v, want %#v", cmd.Args, want)
	}
}

func TestGixHelperCommandSeparatesPureAndGitFallbackModes(t *testing.T) {
	t.Setenv("BETTERLEAKS_GIX_AMBIGUOUS_RENAME_FALLBACK", "1")
	for _, tc := range []struct {
		name     string
		fallback bool
		want     []string
	}{
		{name: "pure gix", want: []string{"gix-helper", "--repo", "repository"}},
		{
			name:     "Git rename fallback",
			fallback: true,
			want: []string{
				"gix-helper", "--repo", "repository", "--ambiguous-rename-fallback",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := gixHelperCommand("gix-helper", "repository", tc.fallback)
			if !reflect.DeepEqual(cmd.Args, tc.want) {
				t.Fatalf("argv = %#v, want %#v", cmd.Args, tc.want)
			}
			if _, inherited := environmentMap(cmd.Env)["BETTERLEAKS_GIX_AMBIGUOUS_RENAME_FALLBACK"]; inherited {
				t.Fatal("gix helper inherited legacy ambient fallback activation")
			}
		})
	}
}

func environmentMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, item := range env {
		key, value, found := strings.Cut(item, "=")
		if found {
			out[key] = value
		}
	}
	return out
}

func TestScanBatchesRecyclesGenerationsAndClosesOnce(t *testing.T) {
	generations := []*fakeWorker{{}, {}, {}}
	workers := []gitengine.Worker{generations[0]}
	factory := &fakeFactory{workers: []gitengine.Worker{generations[1], generations[2]}}
	partial, err := scanBatches(t.Context(), factory, ".", workers, oneCommitBatches(5), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(partial) != 1 || partial[0].Commits != 5 {
		t.Fatalf("partial commit count = %+v", partial)
	}
	if err := closeWorkers(workers); err != nil {
		t.Fatal(err)
	}
	for i, want := range []int{2, 2, 1} {
		batches, closes := generations[i].counts()
		if batches != want || closes != 1 {
			t.Fatalf("generation %d = %d batches/%d closes, want %d/1", i, batches, closes, want)
		}
	}
}

func TestScanBatchesReturnsRecycleCloseError(t *testing.T) {
	want := errors.New("recycle close")
	worker := &fakeWorker{closeErr: want}
	workers := []gitengine.Worker{worker}
	_, err := scanBatches(t.Context(), &fakeFactory{}, ".", workers, oneCommitBatches(3), 2)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	_, closes := worker.counts()
	if closes != 1 || workers[0] != nil {
		t.Fatalf("close state = %d/%v, want 1/nil", closes, workers[0])
	}
}

func TestScanBatchesReturnsRecycleOpenError(t *testing.T) {
	want := errors.New("recycle open")
	worker := &fakeWorker{}
	workers := []gitengine.Worker{worker}
	_, err := scanBatches(t.Context(), &fakeFactory{openErr: want}, ".", workers, oneCommitBatches(3), 2)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	_, closes := worker.counts()
	if closes != 1 || workers[0] != nil {
		t.Fatalf("close state = %d/%v, want 1/nil", closes, workers[0])
	}
}

func TestScanBatchesCancelsPeerAfterScanFailure(t *testing.T) {
	want := errors.New("scan failed")
	peerStarted := make(chan struct{})
	peerCanceled := make(chan struct{})
	failing := &fakeWorker{scan: func(context.Context, gitengine.BatchRequest, gitengine.EmitFunc) (gitengine.BatchResult, error) {
		<-peerStarted
		return gitengine.BatchResult{}, want
	}}
	peer := &fakeWorker{scan: func(ctx context.Context, _ gitengine.BatchRequest, _ gitengine.EmitFunc) (gitengine.BatchResult, error) {
		close(peerStarted)
		<-ctx.Done()
		close(peerCanceled)
		return gitengine.BatchResult{}, ctx.Err()
	}}
	workers := []gitengine.Worker{failing, peer}
	_, err := scanBatches(t.Context(), &fakeFactory{}, ".", workers, oneCommitBatches(2), 0)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	select {
	case <-peerCanceled:
	default:
		t.Fatal("peer did not observe cancellation")
	}
	if err := closeWorkers(workers); err != nil {
		t.Fatal(err)
	}
	for i, worker := range []*fakeWorker{failing, peer} {
		_, closes := worker.counts()
		if closes != 1 {
			t.Fatalf("worker %d closes = %d, want 1", i, closes)
		}
	}
}

func TestCloseWorkersReturnsFinalCloseErrorAndClosesPeers(t *testing.T) {
	want := errors.New("final close")
	first := &fakeWorker{closeErr: want}
	second := &fakeWorker{}
	workers := []gitengine.Worker{first, second}
	if err := closeWorkers(workers); !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	for i, worker := range []*fakeWorker{first, second} {
		_, closes := worker.counts()
		if closes != 1 || workers[i] != nil {
			t.Fatalf("worker %d close state = %d/%v, want 1/nil", i, closes, workers[i])
		}
	}
}
