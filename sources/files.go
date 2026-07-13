package sources

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/betterleaks/betterleaks/logging"
	"github.com/fatih/semgroup"
)

// TODO: remove this in v9 and have scanTargets yield file sources
type ScanTarget struct {
	Path    string
	Symlink string
	Size    int64
}

// Files is a source for yielding fragments from a collection of files
type Files struct {
	ShouldSkip      SkipFunc
	FollowSymlinks  bool
	MaxFileSize     int
	Path            string
	Sema            *semgroup.Group
	MaxArchiveDepth int
	// FileScan enables the explicitly experimental filesystem scanner
	// tournament. Nil preserves the production scanner path.
	FileScan *FileScanConfig
}

// scanTargets yields scan targets to a callback func
func (s *Files) scanTargets(ctx context.Context, yield func(ScanTarget, error) error) error {
	// Per-entry metadata (lstat via d.Info) is needed only when something
	// consumes the size: the filescan experiment's metrics and backends, or
	// an explicit max-size gate. The default path skips the stat entirely;
	// empty files are discovered at open time and yield no fragments.
	needStat := s.FileScan != nil || s.MaxFileSize > 0
	return filepath.WalkDir(s.Path, func(path string, d fs.DirEntry, err error) error {
		scanTarget := ScanTarget{Path: path}

		if err != nil {
			if os.IsPermission(err) {
				// This seems to only fail on directories at this stage.
				logging.Warn().Str("path", path).Err(errors.New("permission denied")).Msg("skipping directory")
				return filepath.SkipDir
			}
			logging.Warn().Str("path", path).Err(err).Msg("skipping")
			return nil
		}
		if needStat {
			metadataStart := time.Time{}
			if s.fileScanMetrics() != nil {
				metadataStart = time.Now()
			}
			info, err := d.Info()
			if !metadataStart.IsZero() {
				s.fileScanMetrics().RecordPhase(FileScanPhaseMetadata, 0, err, time.Since(metadataStart))
			}
			if err != nil {
				if d.IsDir() {
					logging.Error().Str("path", path).Err(err).Msg("skipping directory: could not get info")
					return filepath.SkipDir
				}
				logging.Error().Str("path", path).Err(err).Msg("skipping file: could not get info")
				return nil
			}
			scanTarget.Size = info.Size()

			if !d.IsDir() {
				// Empty; nothing to do here.
				if info.Size() == 0 {
					logging.Debug().Str("path", path).Msg("skipping empty file")
					return nil
				}

				// Too large; nothing to do here.
				if s.MaxFileSize > 0 && info.Size() > int64(s.MaxFileSize) {
					logging.Warn().Str("path", path).Msgf(
						"skipping file: too large max_size=%dMB, size=%dMB",
						s.MaxFileSize/1_000_000, info.Size()/1_000_000,
					)
					return nil
				}
			}
		}

		// set the initial scan target values
		if d.Type() == fs.ModeSymlink {
			if !s.FollowSymlinks {
				logging.Debug().Str("path", path).Msg("skipping symlink: follow symlinks disabled")
				return nil
			}
			realPath, err := filepath.EvalSymlinks(path)
			if err != nil {
				logging.Error().Str("path", path).Err(err).Msg("skipping symlink: could not evaluate")
				return nil
			}
			realPathFileInfo, statErr := os.Stat(realPath)
			if statErr != nil {
				logging.Error().Str("path", path).Err(statErr).Msg("skipping symlink: could not stat target")
				s.fileScanMetrics().RecordLedger("symlink_stat_error", path, statErr.Error())
				return nil
			}
			if realPathFileInfo.IsDir() {
				logging.Debug().Str("path", path).Str("target", realPath).Msgf("skipping symlink: target is directory")
				return nil
			}
			scanTarget = ScanTarget{
				Path:    realPath,
				Symlink: path,
				Size:    realPathFileInfo.Size(),
			}
		}

		// handle dir cases (mainly just see if it should be skipped
		if d.IsDir() {
			if shouldSkipPath(s.ShouldSkip, path) {
				logging.Debug().Str("path", path).Msg("skipping directory: global allowlist")
				return filepath.SkipDir
			}
			return nil
		}

		if shouldSkipPath(s.ShouldSkip, path) {
			logging.Debug().Str("path", path).Msg("skipping file: global allowlist")
			return nil
		}

		s.fileScanMetrics().RecordTarget(s.Path, scanTarget)
		return yield(scanTarget, nil)
	})
}

// Fragments yields fragments from files discovered under the path
func (s *Files) Fragments(ctx context.Context, yield FragmentsFunc) error {
	var (
		cfg        *FileScanConfig
		metrics    *FileScanMetrics
		backend    *contentBackend
		dispatcher *fileScanDispatcher
	)
	if s.FileScan != nil {
		normalized := s.FileScan.Normalize()
		cfg = &normalized
		metrics = normalized.Metrics
		var err error
		backend, err = newContentBackend(ctx, s.Path, normalized)
		if err != nil {
			return err
		}
		defer backend.Close()
		if !normalized.InlineDetector {
			dispatcher = newFileScanDispatcher(ctx, normalized.DetectorWorkers, metrics, yield)
		}
	}

	group := s.Sema
	if cfg != nil || group == nil {
		workers := max(40, runtime.NumCPU()*2)
		if cfg != nil {
			workers = cfg.ActiveFiles
		}
		group = semgroup.NewGroup(ctx, int64(workers))
	}

	scanFile := func(scanTarget ScanTarget) error {
		logging.Trace().Str("path", scanTarget.Path).Msg("scanning path")
		local := metrics.NewLocal()
		if metrics != nil {
			metrics.AddGauge(FileScanGaugeActiveFiles, 1)
			defer metrics.AddGauge(FileScanGaugeActiveFiles, -1)
			defer metrics.MergeLocal(local)
		}

		openTimer := local.StartPhase(FileScanPhaseOpen)
		var (
			opened *openedContent
			err    error
		)
		if backend == nil {
			var file *os.File
			file, err = os.Open(scanTarget.Path)
			if err == nil {
				opened = &openedContent{
					Reader:  file,
					Backend: FileScanBackendBaseline,
					close:   file.Close,
				}
			}
		} else {
			opened, err = backend.Open(ctx, scanTarget)
		}
		openTimer.Done(0, err)
		if err != nil {
			metrics.RecordLedger("open_error", scanTarget.Path, err.Error())
			if os.IsPermission(err) {
				logging.Warn().Str("path", scanTarget.Path).Msg("skipping file: permission denied")
			}
			if cfg != nil {
				return err
			}
			return nil
		}
		if opened.Fallback != "" {
			metrics.RecordFallback(opened.Fallback)
		}
		tracker := &fileScanReadTracker{}
		content := instrumentFileScanReader(opened.Reader, local, tracker)

		// Convert this to a file source
		file := File{
			Content:         content,
			Path:            scanTarget.Path,
			Symlink:         scanTarget.Symlink,
			ShouldSkip:      s.ShouldSkip,
			MaxArchiveDepth: s.MaxArchiveDepth,
			fileScanRoot:    s.Path,
			fileScanMetrics: metrics,
			fileScanLocal:   local,
		}

		fileYield := yield
		if dispatcher != nil {
			fileYield = dispatcher.callback(local)
		} else if local != nil {
			fileYield = measuredFileScanYield(local, yield)
		}
		scanErr := file.Fragments(ctx, fileYield)
		if scanErr != nil {
			metrics.RecordLedger("read_error", scanTarget.Path, scanErr.Error())
		}
		closeTimer := local.StartPhase(FileScanPhaseClose)
		closeErr := opened.Close()
		closeTimer.Done(0, closeErr)
		if closeErr != nil {
			metrics.RecordLedger("close_error", scanTarget.Path, closeErr.Error())
		}
		metrics.RecordBackend(string(opened.Backend), tracker.bytes.Load())
		if cfg != nil {
			return errors.Join(scanErr, closeErr)
		}
		return nil
	}

	var (
		scanErrorsMu sync.Mutex
		scanErrors   []fileScanTaskError
		legacyWG     sync.WaitGroup
	)
	schedule := func(scanTarget ScanTarget) {
		if cfg == nil {
			legacyWG.Add(1)
			group.Go(func() error {
				defer legacyWG.Done()
				return scanFile(scanTarget)
			})
			return
		}
		group.Go(func() error {
			err := scanFile(scanTarget)
			if cfg == nil || err == nil {
				return err
			}
			scanErrorsMu.Lock()
			scanErrors = append(scanErrors, fileScanTaskError{
				path: canonicalFileScanPath(s.Path, scanTarget.Path),
				err:  err,
			})
			scanErrorsMu.Unlock()
			// semgroup formats accumulated errors in completion order. Keep
			// experimental errors out of that nondeterministic aggregate and
			// join them by canonical target below.
			return nil
		})
	}
	walkStart := time.Time{}
	if metrics != nil {
		walkStart = time.Now()
	}
	var walkErr error
	if cfg != nil && cfg.Namespace == FileScanNamespaceSerial {
		walkErr = s.scanTargets(ctx, func(scanTarget ScanTarget, err error) error {
			if err != nil {
				return err
			}
			schedule(scanTarget)
			return nil
		})
	} else {
		walkErr = s.scanTargetsParallel(ctx, schedule)
	}
	if !walkStart.IsZero() {
		metrics.RecordPhase(FileScanPhaseEnumeration, 0, walkErr, time.Since(walkStart))
	}
	if cfg == nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			legacyWG.Wait()
			return walkErr
		}
	}
	groupErr := group.Wait()
	scanErr := joinFileScanTaskErrors(scanErrors)
	var dispatchErr error
	if dispatcher != nil {
		dispatchErr = dispatcher.Close()
	}
	backendErr := error(nil)
	if backend != nil {
		backendErr = backend.Close()
	}
	if ctx.Err() != nil {
		return errors.Join(walkErr, scanErr, groupErr, dispatchErr, backendErr, ctx.Err())
	}
	return errors.Join(walkErr, scanErr, groupErr, dispatchErr, backendErr)
}

type fileScanTaskError struct {
	path string
	err  error
}

func joinFileScanTaskErrors(items []fileScanTaskError) error {
	if len(items) == 0 {
		return nil
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].path != items[j].path {
			return items[i].path < items[j].path
		}
		return items[i].err.Error() < items[j].err.Error()
	})
	joined := make([]error, 0, len(items))
	for _, item := range items {
		joined = append(joined, item.err)
	}
	return errors.Join(joined...)
}

func (s *Files) fileScanMetrics() *FileScanMetrics {
	if s == nil || s.FileScan == nil {
		return nil
	}
	return s.FileScan.Metrics
}

type fileScanReadTracker struct {
	mu    sync.Mutex
	bytes atomic.Uint64
}

func (t *fileScanReadTracker) record(local *FileScanLocalMetrics, n int, err error, start time.Time) {
	if n > 0 {
		t.bytes.Add(uint64(n))
	}
	if local == nil {
		return
	}
	t.mu.Lock()
	local.RecordPhase(FileScanPhaseReadWait, uint64(max(n, 0)), err, time.Since(start))
	t.mu.Unlock()
}

type fileScanMeasuredReader struct {
	reader  io.Reader
	local   *FileScanLocalMetrics
	tracker *fileScanReadTracker
}

func (r *fileScanMeasuredReader) Read(p []byte) (int, error) {
	start := time.Time{}
	if r.local != nil {
		start = time.Now()
	}
	n, err := r.reader.Read(p)
	r.tracker.record(r.local, n, err, start)
	return n, err
}

type fileScanMeasuredFile struct {
	*os.File
	local   *FileScanLocalMetrics
	tracker *fileScanReadTracker
}

func (f *fileScanMeasuredFile) Read(p []byte) (int, error) {
	start := time.Time{}
	if f.local != nil {
		start = time.Now()
	}
	n, err := f.File.Read(p)
	f.tracker.record(f.local, n, err, start)
	return n, err
}

func (f *fileScanMeasuredFile) ReadAt(p []byte, off int64) (int, error) {
	start := time.Time{}
	if f.local != nil {
		start = time.Now()
	}
	n, err := f.File.ReadAt(p, off)
	f.tracker.record(f.local, n, err, start)
	return n, err
}

func instrumentFileScanReader(
	reader io.Reader,
	local *FileScanLocalMetrics,
	tracker *fileScanReadTracker,
) io.Reader {
	if local == nil {
		return reader
	}
	if file, ok := reader.(*os.File); ok {
		return &fileScanMeasuredFile{File: file, local: local, tracker: tracker}
	}
	return &fileScanMeasuredReader{reader: reader, local: local, tracker: tracker}
}

func measuredFileScanYield(local *FileScanLocalMetrics, yield FragmentsFunc) FragmentsFunc {
	return func(fragment Fragment, err error) error {
		timer := local.StartPhase(FileScanPhaseDetector)
		yieldErr := yield(fragment, err)
		timer.Done(uint64(len(fragment.Raw)), yieldErr)
		return yieldErr
	}
}

type fileScanDispatchEvent struct {
	fragment Fragment
	err      error
	local    *FileScanLocalMetrics
	ack      chan error
}

type fileScanDispatcher struct {
	ctx     context.Context
	metrics *FileScanMetrics
	yield   FragmentsFunc
	events  chan fileScanDispatchEvent
	wg      sync.WaitGroup
	once    sync.Once
}

func newFileScanDispatcher(
	ctx context.Context,
	workers int,
	metrics *FileScanMetrics,
	yield FragmentsFunc,
) *fileScanDispatcher {
	d := &fileScanDispatcher{
		ctx:     ctx,
		metrics: metrics,
		yield:   yield,
		events:  make(chan fileScanDispatchEvent, workers),
	}
	d.wg.Add(workers)
	for range workers {
		go func() {
			defer d.wg.Done()
			for event := range d.events {
				d.metrics.AddGauge("fragment_queue_depth", -1)
				timer := event.local.StartPhase(FileScanPhaseDetector)
				err := d.yield(event.fragment, event.err)
				timer.Done(uint64(len(event.fragment.Raw)), err)
				event.ack <- err
			}
		}()
	}
	return d
}

func (d *fileScanDispatcher) callback(local *FileScanLocalMetrics) FragmentsFunc {
	ack := make(chan error, 1)
	return func(fragment Fragment, err error) error {
		queueStart := time.Time{}
		if d.metrics != nil {
			queueStart = time.Now()
		}
		d.metrics.AddGauge("fragment_queue_depth", 1)
		select {
		case <-d.ctx.Done():
			d.metrics.AddGauge("fragment_queue_depth", -1)
			if !queueStart.IsZero() {
				d.metrics.RecordPhase(
					"detector_queue",
					uint64(len(fragment.Raw)),
					d.ctx.Err(),
					time.Since(queueStart),
				)
			}
			return d.ctx.Err()
		case d.events <- fileScanDispatchEvent{fragment: fragment, err: err, local: local, ack: ack}:
			if !queueStart.IsZero() {
				d.metrics.RecordPhase(
					"detector_queue",
					uint64(len(fragment.Raw)),
					nil,
					time.Since(queueStart),
				)
			}
		}
		// Once ownership is transferred, wait for the worker even after
		// cancellation. The fragment and per-file metrics remain owned by the
		// producer until this acknowledgement arrives.
		return <-ack
	}
}

func (d *fileScanDispatcher) Close() error {
	d.once.Do(func() {
		close(d.events)
		d.wg.Wait()
	})
	return nil
}

// scanTargetsParallel walks the directory tree with a bounded pool of walker
// goroutines so file discovery (readdir + stat) is not serialized on a single
// goroutine. Subdirectories are pushed to a shared work queue; each walker pops
// a directory, reads its entries, emits scan targets for files, and pushes
// child directories back. Emission order is nondeterministic, but findings are
// compared as order-independent multisets by the tournament correctness gate.
func (s *Files) scanTargetsParallel(ctx context.Context, emit func(ScanTarget)) error {
	rootInfo, err := os.Lstat(s.Path)
	if err != nil {
		// filepath.WalkDir invokes its callback with the root error and the
		// original scanTargets logged and swallowed it (returning nil), so a
		// missing or unreadable root is a no-op scan, not a hard error.
		if os.IsPermission(err) {
			logging.Warn().Str("path", s.Path).Err(errors.New("permission denied")).Msg("skipping directory")
		} else {
			logging.Warn().Str("path", s.Path).Err(err).Msg("skipping")
		}
		return nil
	}
	if !rootInfo.IsDir() {
		// Single-file source: handle directly (mirrors WalkDir visiting one file).
		s.emitTarget(ctx, s.Path, fs.FileInfoToDirEntry(rootInfo), emit)
		return nil
	}

	walkers := runtime.NumCPU()
	if s.FileScan != nil {
		walkers = s.FileScan.Normalize().Walkers
	}
	var (
		mu       sync.Mutex
		pending  []string
		inFlight int
		cond     = sync.NewCond(&mu)
		wg       sync.WaitGroup
	)
	pending = append(pending, s.Path)
	inFlight = 1 // the root, claimed below
	s.fileScanMetrics().SetGauge("pending_dirs", int64(len(pending)))

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
			s.fileScanMetrics().SetGauge("pending_dirs", int64(len(pending)))
			mu.Unlock()

			children := s.walkDir(ctx, dir, emit)

			mu.Lock()
			pending = append(pending, children...)
			s.fileScanMetrics().SetGauge("pending_dirs", int64(len(pending)))
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
		logging.Warn().Str("path", dir).Err(err).Msg("skipping directory")
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
	scanTarget := ScanTarget{Path: path}
	// Mirror scanTargets: stat only when the size is consumed (filescan
	// experiment metrics/backends, or an explicit max-size gate). On the
	// default path this saves one lstat per file; empty files are discovered
	// at open time and yield no fragments.
	if s.FileScan != nil || s.MaxFileSize > 0 {
		metadataStart := time.Time{}
		if s.fileScanMetrics() != nil {
			metadataStart = time.Now()
		}
		info, err := d.Info()
		if !metadataStart.IsZero() {
			s.fileScanMetrics().RecordPhase(FileScanPhaseMetadata, 0, err, time.Since(metadataStart))
		}
		if err != nil {
			logging.Error().Str("path", path).Err(err).Msg("skipping file: could not get info")
			s.fileScanMetrics().RecordLedger("metadata_error", path, err.Error())
			return
		}

		if info.Size() == 0 {
			return
		}
		if s.MaxFileSize > 0 && info.Size() > int64(s.MaxFileSize) {
			logging.Warn().Str("path", path).Msgf("skipping file: too large max_size=%dMB, size=%dMB",
				s.MaxFileSize/1_000_000, info.Size()/1_000_000)
			return
		}
		scanTarget.Size = info.Size()
	}

	if d.Type() == fs.ModeSymlink {
		if !s.FollowSymlinks {
			return
		}
		realPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			logging.Error().Str("path", path).Err(err).Msg("skipping symlink: could not evaluate")
			return
		}
		realPathFileInfo, statErr := os.Stat(realPath)
		if statErr != nil {
			logging.Error().Str("path", path).Err(statErr).Msg("skipping symlink: could not stat target")
			s.fileScanMetrics().RecordLedger("symlink_stat_error", path, statErr.Error())
			return
		}
		if realPathFileInfo.IsDir() {
			return
		}
		scanTarget = ScanTarget{Path: realPath, Symlink: path, Size: realPathFileInfo.Size()}
	}

	if shouldSkipPath(s.ShouldSkip, path) {
		return
	}
	s.fileScanMetrics().RecordTarget(s.Path, scanTarget)
	emit(scanTarget)
}
