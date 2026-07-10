// Command gitengine-bench measures one Git engine against an explicit commit set.
// It reports a canonical, order-independent record digest with the timing so
// output differences cannot be mistaken for performance wins.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/betterleaks/betterleaks/internal/gitengine"
	"github.com/betterleaks/betterleaks/sources"
)

type options struct {
	engine         string
	helper         string
	repo           string
	workers        int
	batchCommit    int
	recycleBatches int
	sampleCommits  int
	packedWindow   string
	packedLimit    string
	timeout        time.Duration
	wantRef        string
	wantRev        string
	wantDigest     string
	wantRecords    uint64
	wantFiles      uint64
	wantHunks      uint64
	wantBytes      uint64
}

type digest struct {
	Words          [4]uint64
	Records        uint64
	Commits        uint64
	Files          uint64
	Hunks          uint64
	CanonicalBytes uint64
}

func (d *digest) add(record gitengine.Record) error {
	canonical, err := gitengine.CanonicalRecord(record)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(canonical)
	for i := range d.Words {
		d.Words[i] += binary.BigEndian.Uint64(hash[i*8 : (i+1)*8])
	}
	d.Records++
	d.CanonicalBytes += uint64(len(canonical))
	switch record.Kind {
	case gitengine.RecordCommit:
		d.Commits++
	case gitengine.RecordFile:
		d.Files++
	case gitengine.RecordHunk:
		d.Hunks++
	}
	return nil
}

func (d *digest) merge(other digest) {
	for i := range d.Words {
		d.Words[i] += other.Words[i]
	}
	d.Records += other.Records
	d.Commits += other.Commits
	d.Files += other.Files
	d.Hunks += other.Hunks
	d.CanonicalBytes += other.CanonicalBytes
}

func (d digest) String() string {
	var bytes [32]byte
	for i, word := range d.Words {
		binary.BigEndian.PutUint64(bytes[i*8:(i+1)*8], word)
	}
	return hex.EncodeToString(bytes[:])
}

type report struct {
	Engine         string  `json:"engine"`
	Repository     string  `json:"repository"`
	Workers        int     `json:"workers"`
	BatchCommits   int     `json:"batch_commits"`
	RecycleBatches int     `json:"recycle_batches"`
	SampleCommits  int     `json:"sample_commits"`
	PackedWindow   string  `json:"packed_git_window"`
	PackedLimit    string  `json:"packed_git_limit"`
	Requested      int     `json:"requested_commits"`
	Records        uint64  `json:"records"`
	Commits        uint64  `json:"commit_records"`
	Files          uint64  `json:"file_records"`
	Hunks          uint64  `json:"hunk_records"`
	CanonicalBytes uint64  `json:"canonical_bytes"`
	Digest         string  `json:"canonical_multiset_digest"`
	RefDigest      string  `json:"ref_digest"`
	RevisionDigest string  `json:"revision_digest"`
	PreflightMS    float64 `json:"preflight_ms"`
	ScanMS         float64 `json:"scan_ms"`
}

func main() {
	var opts options
	flag.StringVar(&opts.engine, "engine", "reference", "reference, custom-git, libgit2, gix, or gix-git-rename-fallback")
	flag.StringVar(&opts.helper, "helper", "", "candidate helper executable (or patched git for custom-git)")
	flag.StringVar(&opts.repo, "repo", "", "repository to scan")
	flag.IntVar(&opts.workers, "workers", runtime.NumCPU(), "persistent engine workers")
	flag.IntVar(&opts.batchCommit, "batch-commits", 0, "maximum commits per batch (zero uses production sizing)")
	flag.IntVar(&opts.recycleBatches, "recycle-batches", 0, "restart each helper after this many batches (zero disables recycling)")
	flag.IntVar(&opts.sampleCommits, "sample-commits", 0, "deterministically sample this many commits in contiguous runs (zero scans all)")
	flag.StringVar(&opts.packedWindow, "packed-git-window", "", "custom Git core.packedGitWindowSize override")
	flag.StringVar(&opts.packedLimit, "packed-git-limit", "", "custom Git core.packedGitLimit override")
	flag.DurationVar(&opts.timeout, "timeout", 30*time.Minute, "whole-run timeout")
	flag.StringVar(&opts.wantRef, "expected-ref-digest", "", "required SHA-256 of the sorted ref snapshot")
	flag.StringVar(&opts.wantRev, "expected-revision-digest", "", "required SHA-256 of rev-list --all output")
	flag.StringVar(&opts.wantDigest, "expected-digest", "", "required canonical multiset digest")
	flag.Uint64Var(&opts.wantRecords, "expected-records", 0, "required record count (zero disables)")
	flag.Uint64Var(&opts.wantFiles, "expected-files", 0, "required file count (zero disables)")
	flag.Uint64Var(&opts.wantHunks, "expected-hunks", 0, "required hunk count (zero disables)")
	flag.Uint64Var(&opts.wantBytes, "expected-canonical-bytes", 0, "required canonical byte count (zero disables)")
	flag.Parse()
	if err := run(opts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(opts options) error {
	if err := validateOptions(opts); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()

	factory, err := newFactory(opts)
	if err != nil {
		return err
	}
	refDigest, err := refDigest(ctx, opts.repo)
	if err != nil {
		return err
	}
	commits, revisionDigest, err := listCommits(ctx, opts.repo)
	if err != nil {
		return err
	}
	commits = sampleCommitRuns(commits, opts.sampleCommits)
	if opts.wantRef != "" && !strings.EqualFold(opts.wantRef, refDigest) {
		return fmt.Errorf("ref snapshot digest %s != expected %s", refDigest, opts.wantRef)
	}
	if opts.wantRev != "" && !strings.EqualFold(opts.wantRev, revisionDigest) {
		return fmt.Errorf("revision snapshot digest %s != expected %s", revisionDigest, opts.wantRev)
	}
	if len(commits) == 0 {
		return errors.New("repository has no commits")
	}
	if opts.workers > len(commits) {
		opts.workers = len(commits)
	}

	preflightStart := time.Now()
	if _, err := factory.Preflight(ctx, opts.repo, gitengine.ProductionProfile()); err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	preflightDuration := time.Since(preflightStart)

	workers := make([]gitengine.Worker, 0, opts.workers)
	defer func() { _ = closeWorkers(workers) }()
	for range opts.workers {
		worker, err := factory.Open(ctx, opts.repo, gitengine.ProductionProfile())
		if err != nil {
			return fmt.Errorf("open worker %d: %w", len(workers), err)
		}
		workers = append(workers, worker)
	}

	batches := buildBatches(commits, opts.workers, opts.batchCommit)
	scanStart := time.Now()
	partial, err := scanBatches(ctx, factory, opts.repo, workers, batches, opts.recycleBatches)
	scanDuration := time.Since(scanStart)
	if err != nil {
		return err
	}
	if err := closeWorkers(workers); err != nil {
		return fmt.Errorf("close workers: %w", err)
	}
	var total digest
	for _, item := range partial {
		total.merge(item)
	}
	if total.Commits != uint64(len(commits)) {
		return fmt.Errorf("commit coverage %d != %d", total.Commits, len(commits))
	}
	if opts.wantDigest != "" && !strings.EqualFold(opts.wantDigest, total.String()) {
		return fmt.Errorf("canonical digest %s != expected %s", total.String(), opts.wantDigest)
	}
	if opts.wantRecords != 0 && total.Records != opts.wantRecords {
		return fmt.Errorf("record count %d != expected %d", total.Records, opts.wantRecords)
	}
	if opts.wantFiles != 0 && total.Files != opts.wantFiles {
		return fmt.Errorf("file count %d != expected %d", total.Files, opts.wantFiles)
	}
	if opts.wantHunks != 0 && total.Hunks != opts.wantHunks {
		return fmt.Errorf("hunk count %d != expected %d", total.Hunks, opts.wantHunks)
	}
	if opts.wantBytes != 0 && total.CanonicalBytes != opts.wantBytes {
		return fmt.Errorf("canonical bytes %d != expected %d", total.CanonicalBytes, opts.wantBytes)
	}

	result := report{
		Engine: opts.engine, Repository: opts.repo, Workers: opts.workers,
		BatchCommits: opts.batchCommit, RecycleBatches: opts.recycleBatches,
		SampleCommits: opts.sampleCommits,
		PackedWindow:  opts.packedWindow, PackedLimit: opts.packedLimit,
		Requested: len(commits), Records: total.Records, Commits: total.Commits,
		Files: total.Files, Hunks: total.Hunks, CanonicalBytes: total.CanonicalBytes,
		Digest: total.String(), RefDigest: refDigest, RevisionDigest: revisionDigest,
		PreflightMS: milliseconds(preflightDuration), ScanMS: milliseconds(scanDuration),
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func validateOptions(opts options) error {
	if opts.repo == "" {
		return errors.New("-repo is required")
	}
	if opts.workers < 1 {
		return errors.New("-workers must be positive")
	}
	if opts.batchCommit < 0 {
		return errors.New("-batch-commits must be nonnegative")
	}
	if opts.recycleBatches < 0 {
		return errors.New("-recycle-batches must be nonnegative")
	}
	if opts.sampleCommits < 0 {
		return errors.New("-sample-commits must be nonnegative")
	}
	if opts.engine != "custom-git" && (opts.packedWindow != "" || opts.packedLimit != "") {
		return errors.New("packed Git overrides require -engine custom-git")
	}
	return nil
}

func newFactory(opts options) (gitengine.Factory, error) {
	if opts.engine == "reference" {
		return sources.NewGitReferenceFactory(), nil
	}
	if opts.helper == "" {
		return nil, fmt.Errorf("-helper is required for engine %q", opts.engine)
	}
	var command gitengine.CommandBuilder
	switch opts.engine {
	case "custom-git":
		command = func(repo string, _ gitengine.ScanProfile) *exec.Cmd {
			return customGitCommand(opts, repo)
		}
	case "libgit2":
		command = func(repo string, _ gitengine.ScanProfile) *exec.Cmd {
			return exec.Command(opts.helper, "--repo", repo)
		}
	case "gix":
		command = func(repo string, _ gitengine.ScanProfile) *exec.Cmd {
			return gixHelperCommand(opts.helper, repo, false)
		}
	case "gix-git-rename-fallback":
		command = func(repo string, _ gitengine.ScanProfile) *exec.Cmd {
			return gixHelperCommand(opts.helper, repo, true)
		}
	default:
		return nil, fmt.Errorf("unknown engine %q", opts.engine)
	}
	return gitengine.NewProcessFactory(gitengine.ProcessConfig{Command: command})
}

func gixHelperCommand(helper, repo string, ambiguousRenameFallback bool) *exec.Cmd {
	args := []string{"--repo", repo}
	if ambiguousRenameFallback {
		args = append(args, "--ambiguous-rename-fallback")
	}
	cmd := exec.Command(helper, args...)
	cmd.Env = environmentWithoutLegacyGixFallback()
	return cmd
}

func environmentWithoutLegacyGixFallback() []string {
	const legacy = "BETTERLEAKS_GIX_AMBIGUOUS_RENAME_FALLBACK"
	env := os.Environ()
	out := env[:0]
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if key != legacy {
			out = append(out, entry)
		}
	}
	return out
}

func customGitCommand(opts options, repo string) *exec.Cmd {
	args := make([]string, 0, 8)
	if opts.packedWindow != "" {
		args = append(args, "-c", "core.packedGitWindowSize="+opts.packedWindow)
	}
	if opts.packedLimit != "" {
		args = append(args, "-c", "core.packedGitLimit="+opts.packedLimit)
	}
	args = append(args, "-C", repo, "betterleaks--diff-engine", "--protocol=1")
	cmd := exec.Command(opts.helper, args...)
	cmd.Env = sources.GitConfigIsolationEnv()
	return cmd
}

func listCommits(ctx context.Context, repo string) ([]gitengine.OID, string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "rev-list", "--all")
	cmd.Env = sources.GitConfigIsolationEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, "", fmt.Errorf("list commits: %w", err)
	}
	revisionHash := sha256.Sum256(out)
	lines := strings.Fields(string(out))
	commits := make([]gitengine.OID, len(lines))
	seen := make(map[string]struct{}, len(lines))
	for i, line := range lines {
		oid, err := hex.DecodeString(line)
		if err != nil || (len(oid) != 20 && len(oid) != 32) {
			return nil, "", fmt.Errorf("invalid rev-list object ID %q", line)
		}
		if _, exists := seen[string(oid)]; exists {
			return nil, "", fmt.Errorf("duplicate rev-list object ID %q", line)
		}
		seen[string(oid)] = struct{}{}
		commits[i] = oid
	}
	return commits, hex.EncodeToString(revisionHash[:]), nil
}

func refDigest(ctx context.Context, repo string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "for-each-ref", "--format=%(refname)%00%(objectname)")
	cmd.Env = sources.GitConfigIsolationEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("snapshot refs: %w", err)
	}
	lines := bytes.Split(bytes.TrimSuffix(out, []byte{'\n'}), []byte{'\n'})
	sort.Slice(lines, func(i, j int) bool { return bytes.Compare(lines[i], lines[j]) < 0 })
	sorted := bytes.Join(lines, []byte{'\n'})
	sorted = append(sorted, '\n')
	hash := sha256.Sum256(sorted)
	return hex.EncodeToString(hash[:]), nil
}

const batchRunLength = 16

func scanBatches(ctx context.Context, factory gitengine.Factory, repo string,
	workers []gitengine.Worker, batches [][]gitengine.OID,
	recycleBatches int) ([]digest, error) {
	work := make(chan int, len(batches))
	for i := range batches {
		work <- i
	}
	close(work)
	partial := make([]digest, len(workers))
	scanCtx, scanCancel := context.WithCancel(ctx)
	defer scanCancel()
	firstError := make(chan error, 1)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			batchesSinceOpen := 0
			for batchIndex := range work {
				if recycleBatches > 0 && batchesSinceOpen == recycleBatches {
					old := workers[i]
					err := old.Close()
					workers[i] = nil
					if err != nil {
						reportWorkerError(firstError, scanCancel,
							fmt.Errorf("worker %d recycle close: %w", i, err))
						return
					}
					worker, err := factory.Open(scanCtx, repo, gitengine.ProductionProfile())
					if err != nil {
						reportWorkerError(firstError, scanCancel,
							fmt.Errorf("worker %d recycle open: %w", i, err))
						return
					}
					workers[i] = worker
					batchesSinceOpen = 0
				}
				before := partial[i]
				result, err := workers[i].ScanBatch(scanCtx,
					gitengine.BatchRequest{ID: uint64(batchIndex + 1), Commits: batches[batchIndex]},
					partial[i].add)
				if err != nil {
					reportWorkerError(firstError, scanCancel,
						fmt.Errorf("worker %d batch %d: %w", i, batchIndex, err))
					return
				}
				observedCommits := partial[i].Commits - before.Commits
				observedFiles := partial[i].Files - before.Files
				observedHunks := partial[i].Hunks - before.Hunks
				if result.Commits != observedCommits ||
					observedCommits != uint64(len(batches[batchIndex])) ||
					result.Files != observedFiles || result.Hunks != observedHunks {
					reportWorkerError(firstError, scanCancel,
						fmt.Errorf("worker %d batch %d counters result=%d/%d/%d observed=%d/%d/%d",
							i, batchIndex, result.Commits, result.Files, result.Hunks,
							observedCommits, observedFiles, observedHunks))
					return
				}
				batchesSinceOpen++
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-firstError:
		return nil, err
	default:
		return partial, nil
	}
}

func reportWorkerError(firstError chan<- error, cancel context.CancelFunc, err error) {
	select {
	case firstError <- err:
	default:
	}
	cancel()
}

func closeWorkers(workers []gitengine.Worker) error {
	var first error
	for i, worker := range workers {
		if worker == nil {
			continue
		}
		if err := worker.Close(); err != nil && first == nil {
			first = fmt.Errorf("worker %d: %w", i, err)
		}
		workers[i] = nil
	}
	return first
}

func sampleCommitRuns(commits []gitengine.OID, requested int) []gitengine.OID {
	if requested <= 0 || requested >= len(commits) {
		return commits
	}
	runs := (requested + batchRunLength - 1) / batchRunLength
	availableRuns := (len(commits) + batchRunLength - 1) / batchRunLength
	out := make([]gitengine.OID, 0, requested)
	for run := 0; run < runs && len(out) < requested; run++ {
		start := (run * availableRuns / runs) * batchRunLength
		end := min(start+batchRunLength, len(commits))
		remaining := requested - len(out)
		end = min(end, start+remaining)
		out = append(out, commits[start:end]...)
	}
	return out
}

// buildBatches mirrors ParallelGit's locality/load-balancing shape: contiguous
// 16-commit runs are dealt across a queue with about eight batches per worker.
func buildBatches(commits []gitengine.OID, workers, batchCommit int) [][]gitengine.OID {
	batchSize := max(len(commits)/(workers*8), 64)
	if batchCommit > 0 {
		batchSize = min(batchSize, batchCommit)
		var out [][]gitengine.OID
		for start := 0; start < len(commits); start += batchSize {
			end := min(start+batchSize, len(commits))
			out = append(out, commits[start:end])
		}
		return out
	}
	batchCount := (len(commits) + batchSize - 1) / batchSize
	parts := make([][]gitengine.OID, batchCount)
	for i := range parts {
		parts[i] = make([]gitengine.OID, 0, batchSize+batchRunLength)
	}
	for run, start := 0, 0; start < len(commits); run, start = run+1, start+batchRunLength {
		end := min(start+batchRunLength, len(commits))
		parts[run%batchCount] = append(parts[run%batchCount], commits[start:end]...)
	}
	out := parts[:0]
	for _, part := range parts {
		if len(part) != 0 {
			out = append(out, part)
		}
	}
	return out
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}
