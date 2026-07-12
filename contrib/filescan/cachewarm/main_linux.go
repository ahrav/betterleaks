//go:build linux

// Command cachewarm prepares and verifies warm or corpus-local cold page-cache
// benchmark state.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

type cacheSummary struct {
	SchemaVersion        int      `json:"schema_version"`
	State                string   `json:"state"`
	Root                 string   `json:"root"`
	ExcludedDirs         []string `json:"excluded_dirs,omitempty"`
	Files                uint64   `json:"files"`
	LogicalBytes         uint64   `json:"logical_bytes"`
	Pages                uint64   `json:"pages"`
	ResidentPages        uint64   `json:"resident_pages"`
	CachedPages          uint64   `json:"cached_pages"`
	ResidentRatio        float64  `json:"resident_ratio"`
	MinimumRatio         *float64 `json:"minimum_ratio,omitempty"`
	MaximumRatio         *float64 `json:"maximum_ratio,omitempty"`
	DirtyPages           uint64   `json:"dirty_pages"`
	WritebackPages       uint64   `json:"writeback_pages"`
	EvictedPages         uint64   `json:"evicted_pages"`
	RecentlyEvictedPages uint64   `json:"recently_evicted_pages"`
	PrepareDurationNS    int64    `json:"prepare_duration_ns"`
	ProbeDurationNS      int64    `json:"probe_duration_ns"`
}

type cacheProbe struct {
	Pages                uint64
	ResidentPages        uint64
	CachedPages          uint64
	DirtyPages           uint64
	WritebackPages       uint64
	EvictedPages         uint64
	RecentlyEvictedPages uint64
}

type fileAction func(string) error

const maxInt64 = int64(^uint64(0) >> 1)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	flags := flag.NewFlagSet("filescan-cachewarm", flag.ExitOnError)
	root := flags.String("root", "", "corpus root")
	state := flags.String("state", "warm", "required cache state: warm, cold, or probe")
	exclude := flags.String("exclude-dir", "", "comma-separated directory base names to skip")
	workers := flags.Int("workers", min(8, runtime.GOMAXPROCS(0)), "parallel file workers")
	minimum := flags.Float64("minimum-resident-ratio", 0.99, "required mincore resident-page ratio")
	maximum := flags.Float64("maximum-resident-ratio", 0.01, "maximum resident-page ratio for cold state")
	windowMiB := flags.Int64("probe-window-mib", 128, "maximum bytes mapped by each mincore probe")
	flags.Parse(os.Args[1:])
	if flags.NArg() != 0 || *root == "" {
		fmt.Fprintln(os.Stderr, "usage: filescan-cachewarm -root CORPUS [flags]")
		os.Exit(2)
	}
	probeWindow, err := mebibytes(*windowMiB)
	if err != nil {
		fmt.Fprintln(os.Stderr, "filescan-cachewarm:", err)
		os.Exit(2)
	}
	summary, err := prepareCache(
		ctx,
		*root,
		parseExcludedDirs(*exclude),
		*workers,
		*state,
		*minimum,
		*maximum,
		probeWindow,
	)
	if summary.Root != "" {
		_ = json.NewEncoder(os.Stdout).Encode(summary)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "filescan-cachewarm:", err)
		os.Exit(1)
	}
}

func prepareWarmCache(
	ctx context.Context,
	root string,
	excluded []string,
	workers int,
	minimum float64,
	probeWindow int64,
) (cacheSummary, error) {
	return prepareCache(ctx, root, excluded, workers, "warm", minimum, 0.01, probeWindow)
}

func prepareCache(
	ctx context.Context,
	root string,
	excluded []string,
	workers int,
	state string,
	minimum float64,
	maximum float64,
	probeWindow int64,
) (cacheSummary, error) {
	if workers <= 0 {
		return cacheSummary{}, errors.New("workers must be positive")
	}
	if state != "warm" && state != "cold" && state != "probe" {
		return cacheSummary{}, errors.New("state must be warm, cold, or probe")
	}
	if minimum <= 0 || minimum > 1 {
		return cacheSummary{}, errors.New("minimum resident ratio must be in (0,1]")
	}
	if maximum < 0 || maximum >= 1 {
		return cacheSummary{}, errors.New("maximum resident ratio must be in [0,1)")
	}
	pageSize := int64(os.Getpagesize())
	if probeWindow <= 0 || probeWindow%pageSize != 0 {
		return cacheSummary{}, errors.New("probe window must be a positive page-size multiple")
	}
	if uint64(probeWindow) > uint64(^uint(0)>>1) {
		return cacheSummary{}, errors.New("probe window exceeds the platform int range")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return cacheSummary{}, err
	}
	paths, logicalBytes, err := collectRegularFiles(ctx, root, excluded)
	if err != nil {
		return cacheSummary{}, err
	}
	summary := cacheSummary{
		SchemaVersion: 1,
		State:         state,
		Root:          root,
		ExcludedDirs:  append([]string(nil), excluded...),
		Files:         uint64(len(paths)),
		LogicalBytes:  logicalBytes,
	}
	if state == "warm" {
		summary.MinimumRatio = &minimum
	}
	if state == "cold" {
		summary.MaximumRatio = &maximum
	}

	prepareStarted := time.Now()
	var prepareErr error
	switch state {
	case "warm":
		prepareErr = warmFiles(ctx, paths, workers)
	case "cold":
		prepareErr = evictFiles(ctx, paths, workers)
	}
	summary.PrepareDurationNS = time.Since(prepareStarted).Nanoseconds()
	if prepareErr != nil {
		return summary, prepareErr
	}

	probeStarted := time.Now()
	probe, err := probeFiles(ctx, paths, workers, probeWindow)
	summary.ProbeDurationNS = time.Since(probeStarted).Nanoseconds()
	summary.Pages = probe.Pages
	summary.ResidentPages = probe.ResidentPages
	summary.CachedPages = probe.CachedPages
	summary.DirtyPages = probe.DirtyPages
	summary.WritebackPages = probe.WritebackPages
	summary.EvictedPages = probe.EvictedPages
	summary.RecentlyEvictedPages = probe.RecentlyEvictedPages
	if err != nil {
		return summary, err
	}
	if summary.Pages == 0 {
		if state == "warm" {
			summary.ResidentRatio = 1
		}
	} else {
		summary.ResidentRatio = float64(summary.ResidentPages) / float64(summary.Pages)
	}
	if state == "warm" && summary.ResidentRatio < minimum {
		return summary, fmt.Errorf(
			"resident page ratio %.6f is below required %.6f",
			summary.ResidentRatio,
			minimum,
		)
	}
	if state == "cold" && summary.ResidentRatio > maximum {
		return summary, fmt.Errorf(
			"resident page ratio %.6f is above required %.6f",
			summary.ResidentRatio,
			maximum,
		)
	}
	if state == "cold" && (summary.DirtyPages != 0 || summary.WritebackPages != 0) {
		return summary, fmt.Errorf(
			"cold corpus still has dirty=%d or writeback=%d pages",
			summary.DirtyPages,
			summary.WritebackPages,
		)
	}
	return summary, nil
}

func mebibytes(value int64) (int64, error) {
	if value <= 0 {
		return 0, errors.New("probe window MiB must be positive")
	}
	if value > maxInt64/(1<<20) {
		return 0, errors.New("probe window MiB overflows bytes")
	}
	return value << 20, nil
}

func parseExcludedDirs(value string) []string {
	seen := make(map[string]struct{})
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" && item != "." && item != string(filepath.Separator) {
			seen[item] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for item := range seen {
		result = append(result, item)
	}
	slices.Sort(result)
	return result
}

func collectRegularFiles(
	ctx context.Context,
	root string,
	excluded []string,
) ([]string, uint64, error) {
	excludedSet := make(map[string]struct{}, len(excluded))
	for _, name := range excluded {
		excludedSet[name] = struct{}{}
	}
	var paths []string
	var logicalBytes uint64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root {
				if _, skip := excludedSet[entry.Name()]; skip {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return nil
		}
		paths = append(paths, path)
		logicalBytes += uint64(info.Size())
		return nil
	})
	return paths, logicalBytes, err
}

func warmFiles(ctx context.Context, paths []string, workers int) error {
	return processFiles(ctx, paths, workers, "warm", func() fileAction {
		buffer := make([]byte, 1<<20)
		return func(path string) error {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			readErr := readAll(ctx, file, buffer)
			return errors.Join(readErr, file.Close())
		}
	})
}

func evictFiles(ctx context.Context, paths []string, workers int) error {
	return processFiles(ctx, paths, workers, "evict", func() fileAction {
		return func(path string) error {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			info, infoErr := file.Stat()
			var status unix.Cachestat_t
			var statErr error
			if infoErr == nil {
				status, statErr = cacheStat(file, info.Size())
			}
			if statErr == nil && (status.Dirty != 0 || status.Writeback != 0) {
				statErr = fmt.Errorf("dirty=%d writeback=%d pages", status.Dirty, status.Writeback)
			}
			adviseErr := error(nil)
			if infoErr == nil && statErr == nil {
				adviseErr = unix.Fadvise(int(file.Fd()), 0, 0, unix.FADV_DONTNEED)
			}
			return errors.Join(infoErr, statErr, adviseErr, file.Close())
		}
	})
}

func readAll(ctx context.Context, file *os.File, buffer []byte) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := file.Read(buffer)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func processFiles(
	ctx context.Context,
	paths []string,
	workers int,
	operation string,
	newAction func() fileAction,
) error {
	if workers <= 0 {
		return errors.New("workers must be positive")
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan string)
	var firstErr error
	var errOnce sync.Once
	var group sync.WaitGroup
	for range workers {
		action := newAction()
		group.Go(func() {
			for {
				select {
				case <-workerCtx.Done():
					return
				case path, ok := <-jobs:
					if !ok {
						return
					}
					if err := action(path); err != nil {
						wrapped := fmt.Errorf("%s %s: %w", operation, path, err)
						errOnce.Do(func() {
							firstErr = wrapped
							cancel()
						})
					}
				}
			}
		})
	}
sendLoop:
	for _, path := range paths {
		select {
		case <-workerCtx.Done():
			break sendLoop
		case jobs <- path:
		}
	}
	close(jobs)
	group.Wait()
	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func probeFiles(ctx context.Context, paths []string, workers int, windowBytes int64) (cacheProbe, error) {
	var pages atomic.Uint64
	var resident atomic.Uint64
	var cached atomic.Uint64
	var dirty atomic.Uint64
	var writeback atomic.Uint64
	var evicted atomic.Uint64
	var recentlyEvicted atomic.Uint64
	err := processFiles(ctx, paths, workers, "probe", func() fileAction {
		return func(path string) error {
			result, err := probeFile(path, windowBytes)
			if err != nil {
				return err
			}
			pages.Add(result.Pages)
			resident.Add(result.ResidentPages)
			cached.Add(result.CachedPages)
			dirty.Add(result.DirtyPages)
			writeback.Add(result.WritebackPages)
			evicted.Add(result.EvictedPages)
			recentlyEvicted.Add(result.RecentlyEvictedPages)
			return nil
		}
	})
	return cacheProbe{
		Pages: pages.Load(), ResidentPages: resident.Load(), CachedPages: cached.Load(),
		DirtyPages: dirty.Load(), WritebackPages: writeback.Load(), EvictedPages: evicted.Load(),
		RecentlyEvictedPages: recentlyEvicted.Load(),
	}, err
}

func probeFile(path string, windowBytes int64) (cacheProbe, error) {
	file, err := os.Open(path)
	if err != nil {
		return cacheProbe{}, err
	}
	info, infoErr := file.Stat()
	if infoErr != nil {
		return cacheProbe{}, errors.Join(infoErr, file.Close())
	}
	status, statErr := cacheStat(file, info.Size())
	pages, resident, residencyErr := fileResidency(file, info.Size(), windowBytes)
	closeErr := file.Close()
	if err := errors.Join(statErr, residencyErr, closeErr); err != nil {
		return cacheProbe{}, err
	}
	return cacheProbe{
		Pages: pages, ResidentPages: resident, CachedPages: status.Cache,
		DirtyPages: status.Dirty, WritebackPages: status.Writeback, EvictedPages: status.Evicted,
		RecentlyEvictedPages: status.Recently_evicted,
	}, nil
}

func cacheStat(file *os.File, size int64) (unix.Cachestat_t, error) {
	query := unix.CachestatRange{Len: uint64(size)}
	var status unix.Cachestat_t
	if err := unix.Cachestat(uint(file.Fd()), &query, &status, 0); err != nil {
		return unix.Cachestat_t{}, err
	}
	return status, nil
}

func fileResidency(file *os.File, size int64, windowBytes int64) (uint64, uint64, error) {
	pageSize := int64(os.Getpagesize())
	var pages, resident uint64
	for offset := int64(0); offset < size; offset += windowBytes {
		length := min(windowBytes, size-offset)
		mapping, err := unix.Mmap(
			int(file.Fd()),
			offset,
			int(length),
			unix.PROT_NONE,
			unix.MAP_SHARED,
		)
		if err != nil {
			return pages, resident, err
		}
		windowPages := uint64((length + pageSize - 1) / pageSize)
		vector := make([]byte, windowPages)
		_, _, errno := unix.Syscall(
			unix.SYS_MINCORE,
			uintptr(unsafe.Pointer(unsafe.SliceData(mapping))),
			uintptr(length),
			uintptr(unsafe.Pointer(unsafe.SliceData(vector))),
		)
		runtime.KeepAlive(mapping)
		runtime.KeepAlive(vector)
		unmapErr := unix.Munmap(mapping)
		if errno != 0 {
			return pages, resident, errno
		}
		if unmapErr != nil {
			return pages, resident, unmapErr
		}
		pages += windowPages
		for _, value := range vector {
			if value&1 != 0 {
				resident++
			}
		}
	}
	return pages, resident, nil
}
