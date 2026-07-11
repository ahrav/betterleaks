package sources

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/betterleaks/betterleaks/internal/gitengine"
	"github.com/betterleaks/betterleaks/logging"
)

// Engine opt-in knobs. The native-record engine path is an experiment and is
// never selected unless BETTERLEAKS_GIT_ENGINE_BIN points at a patched git
// binary that implements `git betterleaks--diff-engine --protocol=1`.
//
//   - BETTERLEAKS_GIT_ENGINE_BIN: path to the patched git executable. Empty
//     disables the engine path entirely (production default).
//   - BETTERLEAKS_GIT_ENGINE_RECYCLE: restart each helper after this many
//     completed batches (0 or unset keeps one persistent helper per worker).
//   - BETTERLEAKS_GIT_ENGINE_STRICT: "1" turns every preflight failure into a
//     terminal scan error instead of falling back to the text path. Benchmark
//     runs set this so an arm can never silently measure the wrong mechanism.
func engineBinary() string {
	bin := os.Getenv("BETTERLEAKS_GIT_ENGINE_BIN")
	if bin == "" {
		return ""
	}
	if _, err := os.Stat(bin); err != nil {
		logging.Warn().Str("bin", bin).Err(err).Msg("BETTERLEAKS_GIT_ENGINE_BIN is not usable; ignoring")
		return ""
	}
	return bin
}

func engineRecycleBatches() int {
	v := os.Getenv("BETTERLEAKS_GIT_ENGINE_RECYCLE")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		logging.Warn().Str("value", v).Msg("invalid BETTERLEAKS_GIT_ENGINE_RECYCLE; ignoring")
		return 0
	}
	return n
}

func engineStrict() bool { return os.Getenv("BETTERLEAKS_GIT_ENGINE_STRICT") == "1" }

// engineFactory builds the persistent-helper factory for one repository.
func engineFactory(bin, repoPath string) (*gitengine.ProcessFactory, error) {
	command := func(repo string, _ gitengine.ScanProfile) *exec.Cmd {
		cmd := exec.Command(bin, "-C", repo, "betterleaks--diff-engine", "--protocol=1")
		cmd.Env = gitConfigIsolationEnv()
		return cmd
	}
	return gitengine.NewProcessFactory(gitengine.ProcessConfig{Command: command})
}

// enginePreflight validates the helper against the repository before any
// worker opens or emits. A nil error is the only signal that permits the
// engine path; any failure here happened before record emission, so the
// caller may still fall back to the text path.
func (s *ParallelGit) enginePreflight(ctx context.Context, bin string) (*gitengine.ProcessFactory, error) {
	factory, err := engineFactory(bin, s.RepoPath)
	if err != nil {
		return nil, err
	}
	if _, err := factory.Preflight(ctx, s.RepoPath, gitengine.ProductionProfile()); err != nil {
		return nil, fmt.Errorf("git engine preflight: %w", err)
	}
	return factory, nil
}

// runEngineScan drains the shared batch queue with persistent native-record
// engine workers. It preserves ParallelGit's scheduler and batch ownership:
// the batches channel and worker count are identical to the text path; only
// the mechanism that turns a batch of commit SHAs into scanner fragments
// changes. Errors returned from here are terminal: records may already have
// been emitted, so no fallback is permitted.
func (s *ParallelGit) runEngineScan(ctx context.Context, yield FragmentsFunc, batches chan []string, workers int, factory *gitengine.ProcessFactory) error {
	recycle := engineRecycleBatches()
	logging.Info().Int("workers", workers).Int("recycle_batches", recycle).Msg("git engine scan")

	// One Git projection helper shared by all workers: scheduleGitFile only
	// reads immutable fields (Sema, ShouldSkip, repoPath for cat-file blob
	// readers), so sharing is safe and keeps the projection code identical to
	// the production text path.
	projector := &Git{
		Cmd:             &GitCmd{repoPath: s.RepoPath},
		ShouldSkip:      s.ShouldSkip,
		Platform:        s.Platform,
		RemoteURL:       s.RemoteURL,
		Sema:            s.Sema,
		MaxArchiveDepth: s.MaxArchiveDepth,
	}

	var wg sync.WaitGroup
	var helperStarts atomic.Uint64
	g, gctx := errgroup.WithContext(ctx)
	for range workers {
		g.Go(func() error {
			worker, err := factory.Open(gctx, s.RepoPath, gitengine.ProductionProfile())
			if err != nil {
				return fmt.Errorf("git engine open: %w", err)
			}
			helperStarts.Add(1)
			defer func() {
				if worker != nil {
					_ = worker.Close()
				}
			}()
			batchesSinceOpen := 0
			batchID := uint64(0)
			for batch := range batches {
				if err := gctx.Err(); err != nil {
					return err
				}
				if recycle > 0 && batchesSinceOpen == recycle {
					if err := worker.Close(); err != nil {
						worker = nil
						return fmt.Errorf("git engine recycle close: %w", err)
					}
					worker, err = factory.Open(gctx, s.RepoPath, gitengine.ProductionProfile())
					if err != nil {
						return fmt.Errorf("git engine recycle open: %w", err)
					}
					helperStarts.Add(1)
					batchesSinceOpen = 0
				}
				batchID++
				request, err := engineBatchRequest(batchID, batch)
				if err != nil {
					return err
				}
				// Per-batch WaitGroup mirrors the text path: Git.Fragments
				// waits for every scheduled file scan before its batch
				// completes, keeping gctx alive for archive blob reads.
				var batchWG sync.WaitGroup
				runner := engineBatchRunner{git: projector, ctx: gctx, yield: yield, wg: &batchWG}
				result, err := worker.ScanBatch(gctx, request, runner.emit)
				if err != nil {
					return fmt.Errorf("git engine batch: %w", err)
				}
				if err := runner.flushFile(); err != nil {
					return err
				}
				if result.Commits != uint64(len(batch)) {
					return fmt.Errorf("git engine batch covered %d of %d commits", result.Commits, len(batch))
				}
				batchWG.Wait()
				batchesSinceOpen++
			}
			if worker != nil {
				closeErr := worker.Close()
				worker = nil
				if closeErr != nil {
					return fmt.Errorf("git engine close: %w", closeErr)
				}
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		wg.Wait()
		logging.Info().Uint64("helper_starts", helperStarts.Load()).Msg("git engine scan complete")
		return nil
	}
}

func engineBatchRequest(id uint64, batch []string) (gitengine.BatchRequest, error) {
	commits := make([]gitengine.OID, len(batch))
	for i, sha := range batch {
		oid, err := hex.DecodeString(sha)
		if err != nil {
			return gitengine.BatchRequest{}, fmt.Errorf("git engine commit %q: %w", sha, err)
		}
		commits[i] = oid
	}
	return gitengine.BatchRequest{ID: id, Commits: commits}, nil
}

// engineBatchRunner projects typed engine records onto the exact structures
// consumed by the production scheduleGitFile path. A file is scheduled when
// the next file/commit record arrives or the batch completes, mirroring the
// text parser's one-task-per-file behavior.
type engineBatchRunner struct {
	git   *Git
	ctx   context.Context
	yield FragmentsFunc
	wg    *sync.WaitGroup

	header *fastGitHeader
	file   *fastGitFile
}

func (r *engineBatchRunner) emit(record gitengine.Record) error {
	switch record.Kind {
	case gitengine.RecordCommit:
		if err := r.flushFile(); err != nil {
			return err
		}
		r.header = engineHeader(record.Commit)
	case gitengine.RecordFile:
		if err := r.flushFile(); err != nil {
			return err
		}
		if r.header == nil {
			return errors.New("git engine file record before commit record")
		}
		r.file = &fastGitFile{
			header:   r.header,
			newName:  string(record.File.NewPath),
			isBinary: record.File.Binary,
		}
	case gitengine.RecordHunk:
		if r.file == nil {
			return errors.New("git engine hunk record before file record")
		}
		r.file.fragments = append(r.file.fragments, fastGitFragment{
			newPosition:         int64(record.Hunk.NewPosition),
			raw:                 string(record.Hunk.Added),
			missingFinalNewline: record.Hunk.MissingFinalNewline,
		})
	default:
		return fmt.Errorf("git engine record kind %d", record.Kind)
	}
	return nil
}

// flushFile schedules the pending file through the production projection.
func (r *engineBatchRunner) flushFile() error {
	if r.file == nil {
		return nil
	}
	file := r.file
	r.file = nil
	if err := r.ctx.Err(); err != nil {
		return err
	}
	r.git.scheduleGitFile(r.ctx, r.yield, r.wg, gitScanFile{
		fastHeader:    file.header,
		newName:       file.newName,
		isBinary:      file.isBinary,
		native:        true,
		fastFragments: file.fragments,
	})
	return nil
}

func engineHeader(commit gitengine.CommitRecord) *fastGitHeader {
	header := &fastGitHeader{
		sha:     hex.EncodeToString(commit.OID),
		message: string(commit.Message),
	}
	if commit.HasAuthor {
		header.author = fastGitIdentity{
			name:  string(commit.AuthorName),
			email: string(commit.AuthorEmail),
			valid: true,
		}
	}
	if commit.HasAuthorTime {
		zone := time.FixedZone("", int(commit.AuthorUTCOffsetMinutes)*60)
		header.authorDate = time.Unix(commit.AuthorUnixSeconds, 0).In(zone)
	}
	return header
}
