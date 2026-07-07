package sources

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/betterleaks/betterleaks/logging"
	"github.com/fatih/semgroup"
)

// TODO: remove this in v9 and have scanTargets yield file sources
type ScanTarget struct {
	Path    string
	Symlink string
}

// Files is a source for yielding fragments from a collection of files
type Files struct {
	ShouldSkip      SkipFunc
	FollowSymlinks  bool
	MaxFileSize     int
	Path            string
	Sema            *semgroup.Group
	MaxArchiveDepth int
}

// scanTargets yields scan targets to a callback func
func (s *Files) scanTargets(ctx context.Context, yield func(ScanTarget, error) error) error {
	return filepath.WalkDir(s.Path, func(path string, d fs.DirEntry, err error) error {
		scanTarget := ScanTarget{Path: path}
		logger := logging.With().Str("path", path).Logger()

		if err != nil {
			if os.IsPermission(err) {
				// This seems to only fail on directories at this stage.
				logger.Warn().Err(errors.New("permission denied")).Msg("skipping directory")
				return filepath.SkipDir
			}
			logger.Warn().Err(err).Msg("skipping")
			return nil
		}

		info, err := d.Info()
		if err != nil {
			if d.IsDir() {
				logger.Error().Err(err).Msg("skipping directory: could not get info")
				return filepath.SkipDir
			}
			logger.Error().Err(err).Msg("skipping file: could not get info")
			return nil
		}

		if !d.IsDir() {
			// Empty; nothing to do here.
			if info.Size() == 0 {
				logger.Debug().Msg("skipping empty file")
				return nil
			}

			// Too large; nothing to do here.
			if s.MaxFileSize > 0 && info.Size() > int64(s.MaxFileSize) {
				logger.Warn().Msgf(
					"skipping file: too large max_size=%dMB, size=%dMB",
					s.MaxFileSize/1_000_000, info.Size()/1_000_000,
				)
				return nil
			}
		}

		// set the initial scan target values
		if d.Type() == fs.ModeSymlink {
			if !s.FollowSymlinks {
				logger.Debug().Msg("skipping symlink: follow symlinks disabled")
				return nil
			}
			realPath, err := filepath.EvalSymlinks(path)
			if err != nil {
				logger.Error().Err(err).Msg("skipping symlink: could not evaluate")
				return nil
			}
			if realPathFileInfo, _ := os.Stat(realPath); realPathFileInfo.IsDir() {
				logger.Debug().Str("target", realPath).Msgf("skipping symlink: target is directory")
				return nil
			}
			scanTarget = ScanTarget{
				Path:    realPath,
				Symlink: path,
			}
		}

		// handle dir cases (mainly just see if it should be skipped
		if info.IsDir() {
			if shouldSkipPath(s.ShouldSkip, path) {
				logger.Debug().Msg("skipping directory: global allowlist")
				return filepath.SkipDir
			}
			return nil
		}

		if shouldSkipPath(s.ShouldSkip, path) {
			logger.Debug().Msg("skipping file: global allowlist")
			return nil
		}

		return yield(scanTarget, nil)
	})
}

// Fragments yields fragments from files discovered under the path
func (s *Files) Fragments(ctx context.Context, yield FragmentsFunc) error {
	var wg sync.WaitGroup

	scanFile := func(scanTarget ScanTarget) {
		defer wg.Done()
		logger := logging.With().Str("path", scanTarget.Path).Logger()
		logger.Trace().Msg("scanning path")

		f, err := os.Open(scanTarget.Path)
		if err != nil {
			if os.IsPermission(err) {
				logger.Warn().Msg("skipping file: permission denied")
			}
			return
		}

		// Convert this to a file source
		file := File{
			Content:         f,
			Path:            scanTarget.Path,
			Symlink:         scanTarget.Symlink,
			ShouldSkip:      s.ShouldSkip,
			MaxArchiveDepth: s.MaxArchiveDepth,
		}

		_ = file.Fragments(ctx, yield)
		// Avoiding a defer in a hot loop
		_ = f.Close()
	}

	err := s.scanTargetsParallel(ctx, func(scanTarget ScanTarget) {
		wg.Add(1)
		s.Sema.Go(func() error {
			scanFile(scanTarget)
			return nil
		})
	})

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		wg.Wait()
		return err
	}
}

// scanTargetsParallel walks the directory tree with a bounded pool of walker
// goroutines so file discovery (readdir + stat) is not serialized on a single
// goroutine. Subdirectories are pushed to a shared work queue; each walker pops
// a directory, reads its entries, emits scan targets for files, and pushes
// child directories back. Emission order is nondeterministic, but findings are
// order-independent (deduplicated by content), so results are byte-identical to
// the serial filepath.WalkDir walk.
func (s *Files) scanTargetsParallel(ctx context.Context, emit func(ScanTarget)) error {
	rootInfo, err := os.Lstat(s.Path)
	if err != nil {
		// filepath.WalkDir invokes its callback with the root error and the
		// original scanTargets logged and swallowed it (returning nil), so a
		// missing or unreadable root is a no-op scan, not a hard error.
		logger := logging.With().Str("path", s.Path).Logger()
		if os.IsPermission(err) {
			logger.Warn().Err(errors.New("permission denied")).Msg("skipping directory")
		} else {
			logger.Warn().Err(err).Msg("skipping")
		}
		return nil
	}
	if !rootInfo.IsDir() {
		// Single-file source: handle directly (mirrors WalkDir visiting one file).
		s.emitTarget(ctx, s.Path, fs.FileInfoToDirEntry(rootInfo), emit)
		return nil
	}

	walkers := runtime.NumCPU()
	var (
		mu      sync.Mutex
		pending []string
		inFlight int
		cond    = sync.NewCond(&mu)
		wg      sync.WaitGroup
	)
	pending = append(pending, s.Path)
	inFlight = 1 // the root, claimed below

	worker := func() {
		defer wg.Done()
		for {
			mu.Lock()
			for len(pending) == 0 && inFlight > 0 {
				cond.Wait()
			}
			if len(pending) == 0 && inFlight == 0 {
				mu.Unlock()
				cond.Broadcast() // wake peers so they can also exit
				return
			}
			dir := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			mu.Unlock()

			children := s.walkDir(ctx, dir, emit)

			mu.Lock()
			pending = append(pending, children...)
			inFlight += len(children)
			inFlight-- // this dir is done
			mu.Unlock()
			cond.Broadcast()
		}
	}

	wg.Add(walkers)
	for range walkers {
		go worker()
	}
	wg.Wait()
	return nil
}

// walkDir reads one directory's entries, emits file scan targets via emit, and
// returns the child directories to be walked. It applies the same skip/size/
// symlink rules as the serial scanTargets so output is identical.
func (s *Files) walkDir(ctx context.Context, dir string, emit func(ScanTarget)) []string {
	select {
	case <-ctx.Done():
		return nil
	default:
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		logger := logging.With().Str("path", dir).Logger()
		logger.Warn().Err(err).Msg("skipping directory")
		return nil
	}

	// Directory-level allowlist skip (matches scanTargets: SkipDir on match).
	if shouldSkipPath(s.ShouldSkip, dir) {
		return nil
	}

	var children []string
	for _, d := range entries {
		path := filepath.Join(dir, d.Name())
		if d.IsDir() {
			children = append(children, path)
			continue
		}
		s.emitTarget(ctx, path, d, emit)
	}
	return children
}

// emitTarget applies the per-file rules from the original scanTargets (empty,
// too-large, symlink handling, allowlist) and emits a ScanTarget when the file
// should be scanned.
func (s *Files) emitTarget(ctx context.Context, path string, d fs.DirEntry, emit func(ScanTarget)) {
	logger := logging.With().Str("path", path).Logger()
	info, err := d.Info()
	if err != nil {
		logger.Error().Err(err).Msg("skipping file: could not get info")
		return
	}

	if info.Size() == 0 {
		return
	}
	if s.MaxFileSize > 0 && info.Size() > int64(s.MaxFileSize) {
		logger.Warn().Msgf("skipping file: too large max_size=%dMB, size=%dMB",
			s.MaxFileSize/1_000_000, info.Size()/1_000_000)
		return
	}

	scanTarget := ScanTarget{Path: path}
	if d.Type() == fs.ModeSymlink {
		if !s.FollowSymlinks {
			return
		}
		realPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			logger.Error().Err(err).Msg("skipping symlink: could not evaluate")
			return
		}
		if realPathFileInfo, _ := os.Stat(realPath); realPathFileInfo != nil && realPathFileInfo.IsDir() {
			return
		}
		scanTarget = ScanTarget{Path: realPath, Symlink: path}
	}

	if shouldSkipPath(s.ShouldSkip, path) {
		return
	}
	emit(scanTarget)
}
