// Command gitengine-compare locates commit-level native-record differences
// between stock Git and one experimental helper in a single pass per engine.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/betterleaks/betterleaks/internal/gitengine"
	"github.com/betterleaks/betterleaks/sources"
)

type options struct {
	engine      string
	helper      string
	repo        string
	workers     int
	batchCommit int
	maxDetails  int
	commit      string
	start       int
	limit       int
}

type commitSummary struct {
	words   [4]uint64
	records uint64
	bytes   uint64
}

func (s *commitSummary) add(record gitengine.Record) error {
	canonical, err := gitengine.CanonicalRecord(record)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(canonical)
	for i := range s.words {
		s.words[i] += binary.BigEndian.Uint64(hash[i*8 : (i+1)*8])
	}
	s.records++
	s.bytes += uint64(len(canonical))
	return nil
}

func main() {
	var opts options
	flag.StringVar(&opts.engine, "engine", "libgit2", "libgit2, gix, gix-git-rename-fallback, or custom-git")
	flag.StringVar(&opts.helper, "helper", "", "candidate helper executable")
	flag.StringVar(&opts.repo, "repo", "", "repository to compare")
	flag.IntVar(&opts.workers, "workers", runtime.NumCPU(), "parallel workers per engine")
	flag.IntVar(&opts.batchCommit, "batch-commits", 256, "maximum commits per summary batch")
	flag.IntVar(&opts.maxDetails, "max-details", 20, "maximum mismatching commits to rescan in detail")
	flag.StringVar(&opts.commit, "commit", "", "optional single commit OID")
	flag.IntVar(&opts.start, "start", 0, "zero-based rev-list offset")
	flag.IntVar(&opts.limit, "limit", 0, "maximum commits to compare; zero means all")
	flag.Parse()
	if err := run(opts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(opts options) error {
	if opts.repo == "" || opts.helper == "" {
		return fmt.Errorf("-repo and -helper are required")
	}
	if opts.workers < 1 {
		return fmt.Errorf("-workers must be positive")
	}
	if opts.batchCommit < 1 {
		return fmt.Errorf("-batch-commits must be positive")
	}
	if opts.maxDetails < 0 {
		return fmt.Errorf("-max-details must be nonnegative")
	}
	if opts.start < 0 || opts.limit < 0 {
		return fmt.Errorf("-start and -limit must be nonnegative")
	}
	ctx := context.Background()
	var commits []gitengine.OID
	if opts.commit != "" {
		oid, err := hex.DecodeString(opts.commit)
		if err != nil || (len(oid) != 20 && len(oid) != 32) {
			return fmt.Errorf("invalid -commit object ID %q", opts.commit)
		}
		commits = []gitengine.OID{oid}
	} else {
		var err error
		commits, err = listCommits(opts.repo)
		if err != nil {
			return err
		}
		if opts.start > len(commits) {
			return fmt.Errorf("-start %d exceeds %d commits", opts.start, len(commits))
		}
		commits = commits[opts.start:]
		if opts.limit != 0 && opts.limit < len(commits) {
			commits = commits[:opts.limit]
		}
		if len(commits) == 0 {
			return fmt.Errorf("selected commit range is empty")
		}
	}
	reference := sources.NewGitReferenceFactory()
	candidate, err := candidateFactory(opts)
	if err != nil {
		return err
	}
	left, err := collectSummaries(ctx, reference, opts.repo, commits, opts.workers, opts.batchCommit)
	if err != nil {
		return fmt.Errorf("reference: %w", err)
	}
	right, err := collectSummaries(ctx, candidate, opts.repo, commits, opts.workers, opts.batchCommit)
	if err != nil {
		return fmt.Errorf("candidate: %w", err)
	}
	var mismatchOIDs []gitengine.OID
	for _, oid := range commits {
		key := string(oid)
		if left[key] == right[key] {
			continue
		}
		mismatchOIDs = append(mismatchOIDs, oid)
		fmt.Printf("MISMATCH-SUMMARY commit %s reference=%d/%d candidate=%d/%d\n",
			hex.EncodeToString(oid), left[key].records, left[key].bytes,
			right[key].records, right[key].bytes)
	}
	if len(mismatchOIDs) == 0 {
		fmt.Printf("compared=%d mismatches=0\n", len(commits))
		return nil
	}
	detailOIDs := mismatchOIDs
	if len(detailOIDs) > opts.maxDetails {
		detailOIDs = detailOIDs[:opts.maxDetails]
	}
	leftRecords, err := collectRecords(ctx, reference, opts.repo, detailOIDs)
	if err != nil {
		return fmt.Errorf("reference mismatch rescan: %w", err)
	}
	rightRecords, err := collectRecords(ctx, candidate, opts.repo, detailOIDs)
	if err != nil {
		return fmt.Errorf("candidate mismatch rescan: %w", err)
	}
	for _, oid := range detailOIDs {
		key := string(oid)
		fmt.Printf("MISMATCH commit %s reference=%d candidate=%d\n",
			hex.EncodeToString(oid), len(leftRecords[key]), len(rightRecords[key]))
		printDifference(leftRecords[key], rightRecords[key])
	}
	fmt.Printf("compared=%d mismatches=%d details=%d\n", len(commits), len(mismatchOIDs), len(detailOIDs))
	return fmt.Errorf("record parity failed for %d commits", len(mismatchOIDs))
}

func candidateFactory(opts options) (gitengine.Factory, error) {
	var command gitengine.CommandBuilder
	switch opts.engine {
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
	case "custom-git":
		command = func(repo string, _ gitengine.ScanProfile) *exec.Cmd {
			return isolatedGitCommand(opts.helper, "-C", repo, "betterleaks--diff-engine", "--protocol=1")
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

func isolatedGitCommand(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Env = sources.GitConfigIsolationEnv()
	return cmd
}

func collectSummaries(ctx context.Context, factory gitengine.Factory, repo string, commits []gitengine.OID, workers, batchCommit int) (map[string]commitSummary, error) {
	if _, err := factory.Preflight(ctx, repo, gitengine.ProductionProfile()); err != nil {
		return nil, err
	}
	workers = min(workers, len(commits))
	engineWorkers := make([]gitengine.Worker, 0, workers)
	defer func() {
		for _, worker := range engineWorkers {
			_ = worker.Close()
		}
	}()
	for range workers {
		worker, err := factory.Open(ctx, repo, gitengine.ProductionProfile())
		if err != nil {
			return nil, err
		}
		engineWorkers = append(engineWorkers, worker)
	}
	batches := buildBatches(commits, workers, batchCommit)
	work := make(chan int, len(batches))
	for i := range batches {
		work <- i
	}
	close(work)
	partials := make([]map[string]commitSummary, workers)
	errCh := make(chan error, 1)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for i, worker := range engineWorkers {
		partials[i] = make(map[string]commitSummary, len(commits)/workers+1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for batchIndex := range work {
				request := gitengine.BatchRequest{ID: uint64(batchIndex + 1), Commits: batches[batchIndex]}
				_, err := worker.ScanBatch(ctx, request, func(record gitengine.Record) error {
					oid, err := recordOID(record)
					if err != nil {
						return err
					}
					key := string(oid)
					summary := partials[i][key]
					if err := summary.add(record); err != nil {
						return err
					}
					partials[i][key] = summary
					return nil
				})
				if err != nil {
					select {
					case errCh <- err:
					default:
					}
					cancel()
					return
				}
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-errCh:
		return nil, err
	default:
	}
	all := make(map[string]commitSummary, len(commits))
	for _, partial := range partials {
		for key, summary := range partial {
			if _, exists := all[key]; exists {
				return nil, fmt.Errorf("duplicate commit summary %s", hex.EncodeToString([]byte(key)))
			}
			all[key] = summary
		}
	}
	return all, nil
}

func collectRecords(ctx context.Context, factory gitengine.Factory, repo string, commits []gitengine.OID) (map[string][]gitengine.Record, error) {
	if _, err := factory.Preflight(ctx, repo, gitengine.ProductionProfile()); err != nil {
		return nil, err
	}
	worker, err := factory.Open(ctx, repo, gitengine.ProductionProfile())
	if err != nil {
		return nil, err
	}
	defer worker.Close()
	groups := make(map[string][]gitengine.Record, len(commits))
	_, err = worker.ScanBatch(ctx, gitengine.BatchRequest{ID: 1, Commits: commits}, func(record gitengine.Record) error {
		oid, err := recordOID(record)
		if err != nil {
			return err
		}
		groups[string(oid)] = append(groups[string(oid)], record)
		return nil
	})
	return groups, err
}

func recordOID(record gitengine.Record) (gitengine.OID, error) {
	switch record.Kind {
	case gitengine.RecordCommit:
		return record.Commit.OID, nil
	case gitengine.RecordFile:
		return record.File.Commit, nil
	case gitengine.RecordHunk:
		return record.Hunk.Commit, nil
	default:
		return nil, fmt.Errorf("unknown record kind %d", record.Kind)
	}
}

func buildBatches(commits []gitengine.OID, workers, batchCommit int) [][]gitengine.OID {
	batchSize := min(max(len(commits)/(workers*8), 64), batchCommit)
	var out [][]gitengine.OID
	for start := 0; start < len(commits); start += batchSize {
		end := min(start+batchSize, len(commits))
		out = append(out, commits[start:end])
	}
	return out
}

func printDifference(reference, candidate []gitengine.Record) {
	left := recordBag(reference)
	right := recordBag(candidate)
	var keys []string
	for key := range left {
		keys = append(keys, key)
	}
	for key := range right {
		if _, ok := left[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if left[key] != right[key] {
			fmt.Printf("  reference=%d candidate=%d %s\n", left[key], right[key], key)
		}
	}
}

func recordBag(records []gitengine.Record) map[string]int {
	bag := make(map[string]int, len(records))
	for _, record := range records {
		bag[describe(record)]++
	}
	return bag
}

func describe(record gitengine.Record) string {
	switch record.Kind {
	case gitengine.RecordCommit:
		return fmt.Sprintf("CMIT message=%q author=%q <%s> time=%d zone=%d has_author=%t has_time=%t",
			record.Commit.Message, record.Commit.AuthorName, record.Commit.AuthorEmail,
			record.Commit.AuthorUnixSeconds, record.Commit.AuthorUTCOffsetMinutes,
			record.Commit.HasAuthor, record.Commit.HasAuthorTime)
	case gitengine.RecordFile:
		return fmt.Sprintf("FILE %c %o->%o oid=%x->%x %q -> %q binary=%t", record.File.Status,
			record.File.OldMode, record.File.NewMode, record.File.OldOID, record.File.NewOID,
			record.File.OldPath, record.File.NewPath, record.File.Binary)
	case gitengine.RecordHunk:
		added := record.Hunk.Added
		preview := added
		if len(preview) > 160 {
			preview = preview[:160]
		}
		return fmt.Sprintf("HUNK path=%q pos=%d bytes=%d missing=%t preview=%q discriminator=%x",
			record.Hunk.NewPath, record.Hunk.NewPosition, len(added), record.Hunk.MissingFinalNewline,
			preview, discriminator(added))
	default:
		return fmt.Sprintf("unknown record kind %d", record.Kind)
	}
}

func discriminator(data []byte) []byte {
	var out [8]byte
	for i, b := range data {
		out[i%len(out)] ^= b + byte(i)
	}
	return bytes.TrimRight(out[:], "\x00")
}

func listCommits(repo string) ([]gitengine.OID, error) {
	out, err := isolatedGitCommand("git", "-C", repo, "rev-list", "--all").Output()
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(out))
	commits := make([]gitengine.OID, len(fields))
	for i, field := range fields {
		oid, err := hex.DecodeString(field)
		if err != nil {
			return nil, err
		}
		commits[i] = oid
	}
	return commits, nil
}
