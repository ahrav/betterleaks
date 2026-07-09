package sources

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/fatih/semgroup"
	"github.com/gitleaks/go-gitdiff/gitdiff"
	"golang.org/x/sync/errgroup"

	"github.com/betterleaks/betterleaks/logging"
	"github.com/betterleaks/betterleaks/sources/scm"
)

// ParallelGit scans a git repo by running multiple `git log -p` processes
// in parallel, each covering a distinct set of commits. Commit SHAs are
// enumerated once via `git rev-list` and then partitioned across workers,
// guaranteeing deterministic, non-overlapping coverage regardless of
// timestamp ties in the commit graph.
type ParallelGit struct {
	RepoPath        string
	ShouldSkip      SkipFunc
	Platform        scm.Platform
	RemoteURL       string
	Sema            *semgroup.Group
	MaxArchiveDepth int
	LogOpts         string
	Workers         int // 0 means auto (NumCPU)
}

func (s *ParallelGit) workers() int {
	if s.Workers > 0 {
		return s.Workers
	}
	return runtime.NumCPU()
}

// Fragments implements Source by partitioning commits across
// multiple parallel git log workers.
func (s *ParallelGit) Fragments(ctx context.Context, yield FragmentsFunc) error {
	commits, err := listCommits(ctx, s.RepoPath, s.LogOpts)
	if err != nil {
		// Some --log-opts are valid for `git log` but not `git rev-list`
		// (e.g. `-n 5` with no ref). Fall back to a single `git log -p`
		// process, which preserves the exact non-partitioned behavior.
		logging.Debug().Err(err).Msg("could not enumerate commits; falling back to single git log process")
		return s.runSingleWorker(ctx, yield)
	}

	count := len(commits)
	workers := s.workers()
	if count == 0 {
		return nil
	}
	if workers > count {
		workers = count
	}

	// For very small repos, just run a single worker (no overhead)
	if workers <= 1 {
		return s.runSingleWorker(ctx, yield)
	}

	// Batch shape balances two competing effects.
	//
	// LOAD BALANCE: commit diff sizes follow a power-law and heavy commits
	// cluster in contiguous stretches of history, so coarse contiguous
	// partitions straggle on wall clock. Spreading each batch across the
	// whole history samples heavy regions uniformly, and the shared queue
	// lets workers self-correct residual imbalance.
	//
	// CACHE LOCALITY: consecutive commits share blob versions (commit N's
	// post-image is commit N+1's pre-image), so a git process walking a
	// contiguous run hits its delta-base cache instead of re-inflating
	// bases; fully contiguous batches cost ~8-18% less git CPU than
	// commit-by-commit striding, but lose far more wall to stragglers.
	//
	// Striding RUNS of 16 consecutive commits (rather than single commits)
	// captures most of the locality win — measured ~8% less git CPU on a
	// 117k-commit monorepo at ~1s of wall — while preserving the uniform
	// heavy-region sampling that keeps workers balanced.
	const runLen = 16
	batchSize := max(count/(workers*8), 64)
	numBatches := (count + batchSize - 1) / batchSize
	logging.Info().Int("commits", count).Int("workers", workers).Int("batch_size", batchSize).Int("batches", numBatches).Msg("parallel git scan")

	batchBufs := make([][]string, numBatches)
	for b := range batchBufs {
		batchBufs[b] = make([]string, 0, batchSize+runLen)
	}
	numRuns := (count + runLen - 1) / runLen
	for j := range numRuns {
		lo := j * runLen
		hi := min(lo+runLen, count)
		b := j % numBatches
		batchBufs[b] = append(batchBufs[b], commits[lo:hi]...)
	}

	batches := make(chan []string, numBatches)
	for _, batch := range batchBufs {
		if len(batch) > 0 {
			batches <- batch
		}
	}
	close(batches)

	g, gctx := errgroup.WithContext(ctx)
	for range workers {
		g.Go(func() error {
			for batch := range batches {
				if err := gctx.Err(); err != nil {
					return err
				}
				if err := s.runWorkerCommits(gctx, yield, batch); err != nil {
					return err
				}
			}
			return nil
		})
	}

	return g.Wait()
}

// runSingleWorker runs a full git log (no partitioning) for small repos or
// single-worker mode.
func (s *ParallelGit) runSingleWorker(ctx context.Context, yield FragmentsFunc) error {
	gitCmd, err := newGitLogCmd(ctx, s.RepoPath, s.LogOpts)
	if err != nil {
		return err
	}

	src := &Git{
		Cmd:             gitCmd,
		ShouldSkip:      s.ShouldSkip,
		Platform:        s.Platform,
		RemoteURL:       s.RemoteURL,
		Sema:            s.Sema,
		MaxArchiveDepth: s.MaxArchiveDepth,
	}

	return src.Fragments(ctx, yield)
}

// runWorkerCommits runs a git log process for a specific set of commit SHAs,
// piped via stdin with --no-walk.
func (s *ParallelGit) runWorkerCommits(ctx context.Context, yield FragmentsFunc, commits []string) error {
	gitCmd, err := newGitLogCommitsCmd(ctx, s.RepoPath, commits)
	if err != nil {
		return err
	}

	src := &Git{
		Cmd:             gitCmd,
		ShouldSkip:      s.ShouldSkip,
		Platform:        s.Platform,
		RemoteURL:       s.RemoteURL,
		Sema:            s.Sema,
		MaxArchiveDepth: s.MaxArchiveDepth,
	}

	return src.Fragments(ctx, yield)
}

// newGitLogCmd constructs a full git log -p command (no partitioning).
func newGitLogCmd(ctx context.Context, source string, logOpts string) (*GitCmd, error) {
	sourceClean := filepath.Clean(source)
	args := []string{"-C", sourceClean, "log", "-p", "-U0"}

	if logOpts != "" {
		userArgs, err := splitGitLogOpts(logOpts)
		if err != nil {
			return nil, fmt.Errorf("invalid --log-opts: %w", err)
		}
		args = append(args, userArgs...)
		return startGitLogCmd(ctx, sourceClean, args, true)
	}
	args = append(args, "--full-history", "--all", "--diff-filter=tuxdb")
	return startGitLogCmd(ctx, sourceClean, args, false)
}

// newGitLogCommitsCmd constructs a git log -p command that processes a specific
// set of commits via --no-walk --stdin. This avoids non-deterministic ordering
// issues with --skip/--max-count on repos with timestamp ties.
func newGitLogCommitsCmd(ctx context.Context, source string, commits []string) (*GitCmd, error) {
	sourceClean := filepath.Clean(source)
	args := []string{"-C", sourceClean, "log", "-p", "-U0", "--no-walk", "--stdin", "--diff-filter=tuxdb"}

	cmd := exec.CommandContext(ctx, gitBinary(), args...)
	cmd.Env = gitConfigIsolationEnv()
	logging.Debug().Msgf("executing: %s (%d commits via stdin)", cmd.String(), len(commits))

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	go func() {
		defer stdin.Close()
		for _, sha := range commits {
			if _, err := fmt.Fprintln(stdin, sha); err != nil {
				return
			}
		}
	}()

	errCh := make(chan error)
	go listenForStdErr(stderr, errCh)

	gitdiffFiles, err := fastParseGitLog(stdout)
	if err != nil {
		return nil, err
	}

	return &GitCmd{
		cmd:         cmd,
		diffFilesCh: gitdiffFiles,
		errCh:       errCh,
		repoPath:    sourceClean,
	}, nil
}

// startGitLogCmd is the shared tail for starting a git log process, wiring up
// stdout/stderr pipes, and returning a GitCmd. hasUserOpts selects the
// general gitdiff parser: user-provided --log-opts can change the output
// format (--pretty, --src-prefix, -U3, ...) beyond what the fast parser
// understands, while betterleaks-constructed argument lists always produce
// the default `log -p -U0` stream shape.
func startGitLogCmd(ctx context.Context, repoPath string, args []string, hasUserOpts bool) (*GitCmd, error) {
	cmd := exec.CommandContext(ctx, gitBinary(), args...)
	cmd.Env = gitConfigIsolationEnv()
	logging.Debug().Msgf("executing: %s", cmd.String())

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	errCh := make(chan error)
	go listenForStdErr(stderr, errCh)

	var gitdiffFiles <-chan *gitdiff.File
	if hasUserOpts {
		gitdiffFiles, err = gitdiff.Parse(stdout)
	} else {
		gitdiffFiles, err = fastParseGitLog(stdout)
	}
	if err != nil {
		return nil, err
	}

	return &GitCmd{
		cmd:         cmd,
		diffFilesCh: gitdiffFiles,
		errCh:       errCh,
		repoPath:    repoPath,
	}, nil
}

// listCommits returns all commit SHAs matching the given log options.
// The order is deterministic for a given repo state (reverse chronological
// from a single rev-list invocation), which is critical for correct
// partitioning across workers.
func listCommits(ctx context.Context, source string, logOpts string) ([]string, error) {
	sourceClean := filepath.Clean(source)
	args := []string{"-C", sourceClean, "rev-list"}

	if logOpts != "" {
		userArgs, err := splitGitLogOpts(logOpts)
		if err != nil {
			return nil, fmt.Errorf("invalid --log-opts: %w", err)
		}
		args = append(args, userArgs...)
	} else {
		args = append(args, "--all")
	}

	cmd := exec.CommandContext(ctx, gitBinary(), args...)
	cmd.Env = gitConfigIsolationEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git rev-list: %w", err)
	}

	text := strings.TrimSpace(string(out))
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

// commitCount returns the number of commits matching the given log options.
func commitCount(ctx context.Context, source string, logOpts string) (int, error) {
	sourceClean := filepath.Clean(source)
	args := []string{"-C", sourceClean, "rev-list", "--count"}

	if logOpts != "" {
		userArgs, err := splitGitLogOpts(logOpts)
		if err != nil {
			return 0, fmt.Errorf("invalid --log-opts: %w", err)
		}
		args = append(args, userArgs...)
	} else {
		args = append(args, "--all")
	}

	cmd := exec.CommandContext(ctx, gitBinary(), args...)
	cmd.Env = gitConfigIsolationEnv()
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("git rev-list --count: %w", err)
	}

	return strconv.Atoi(strings.TrimSpace(string(out)))
}
