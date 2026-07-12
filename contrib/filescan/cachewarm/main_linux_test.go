//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPrepareWarmCacheAndMincore(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.txt"), make([]byte, 3*os.Getpagesize()+17), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".skip"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".skip", "ignored.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err := prepareWarmCache(
		context.Background(), root, []string{".skip"}, 2, 1, int64(2*os.Getpagesize()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Files != 1 || summary.Pages != 4 || summary.ResidentPages != 4 || summary.ResidentRatio != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.State != "warm" || summary.MinimumRatio == nil || *summary.MinimumRatio != 1 || summary.MaximumRatio != nil {
		t.Fatalf("summary thresholds = %+v", summary)
	}
}

func TestPrepareColdCacheAndMincore(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "cold.bin")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(make([]byte, 16*os.Getpagesize()+17)); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareWarmCache(
		context.Background(), root, nil, 1, 1, int64(4*os.Getpagesize()),
	); err != nil {
		t.Fatal(err)
	}
	summary, err := prepareCache(
		context.Background(), root, nil, 1, "cold", 0.99, 0.01, int64(4*os.Getpagesize()),
	)
	if err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
			t.Skip(err)
		}
		var stat unix.Statfs_t
		if statErr := unix.Statfs(root, &stat); statErr == nil && stat.Type == unix.TMPFS_MAGIC {
			t.Skipf("tmpfs does not honor corpus-local FADV_DONTNEED: %v", err)
		}
		t.Fatal(err)
	}
	if summary.State != "cold" || summary.Pages != 17 || summary.ResidentRatio > 0.01 || summary.DirtyPages != 0 || summary.WritebackPages != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.MaximumRatio == nil || *summary.MaximumRatio != 0.01 || summary.MinimumRatio != nil {
		t.Fatalf("summary thresholds = %+v", summary)
	}
}

func TestPrepareCacheEmptyCorpusRatiosAndThresholds(t *testing.T) {
	tests := []struct {
		state     string
		maximum   float64
		wantRatio float64
		wantMin   bool
		wantMax   bool
	}{
		{state: "warm", maximum: 0.01, wantRatio: 1, wantMin: true},
		{state: "cold", maximum: 0, wantMax: true},
		{state: "probe", maximum: 0.01},
	}
	for _, test := range tests {
		t.Run(test.state, func(t *testing.T) {
			summary, err := prepareCache(
				context.Background(), t.TempDir(), nil, 2, test.state, 0.99, test.maximum, int64(os.Getpagesize()),
			)
			if err != nil {
				t.Fatal(err)
			}
			if summary.Pages != 0 || summary.ResidentPages != 0 || summary.ResidentRatio != test.wantRatio {
				t.Fatalf("summary = %+v", summary)
			}
			if (summary.MinimumRatio != nil) != test.wantMin || (summary.MaximumRatio != nil) != test.wantMax {
				t.Fatalf("summary thresholds = %+v", summary)
			}
			if test.wantMax && *summary.MaximumRatio != test.maximum {
				t.Fatalf("maximum ratio = %v, want %v", *summary.MaximumRatio, test.maximum)
			}
			encoded, err := json.Marshal(summary)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantMax && !strings.Contains(string(encoded), `"maximum_ratio":0`) {
				t.Fatalf("encoded summary omits zero maximum: %s", encoded)
			}
		})
	}
}

func TestPrepareProbeConcurrent(t *testing.T) {
	root := t.TempDir()
	pageSize := os.Getpagesize()
	wantPages := uint64(0)
	for index := range 32 {
		pages := index%4 + 1
		path := filepath.Join(root, "file-"+strconv.Itoa(index))
		if err := os.WriteFile(path, make([]byte, pages*pageSize), 0o600); err != nil {
			t.Fatal(err)
		}
		wantPages += uint64(pages)
	}
	if _, err := prepareWarmCache(context.Background(), root, nil, 4, 1, int64(2*pageSize)); err != nil {
		t.Fatal(err)
	}
	summary, err := prepareCache(context.Background(), root, nil, 4, "probe", 0.99, 0.01, int64(2*pageSize))
	if err != nil {
		t.Fatal(err)
	}
	if summary.State != "probe" || summary.Files != 32 || summary.Pages != wantPages ||
		summary.ResidentPages != wantPages || summary.CachedPages != wantPages || summary.ResidentRatio != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.MinimumRatio != nil || summary.MaximumRatio != nil {
		t.Fatalf("probe summary has thresholds: %+v", summary)
	}
}

func TestProcessFilesCreatesOneActionPerWorker(t *testing.T) {
	var actions atomic.Int64
	var processed atomic.Int64
	paths := make([]string, 37)
	err := processFiles(context.Background(), paths, 4, "test", func() fileAction {
		actions.Add(1)
		return func(string) error {
			processed.Add(1)
			return nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if actions.Load() != 4 || processed.Load() != int64(len(paths)) {
		t.Fatalf("actions = %d, processed = %d", actions.Load(), processed.Load())
	}
	if err := processFiles(context.Background(), paths, 0, "test", func() fileAction { return nil }); err == nil {
		t.Fatal("processFiles with zero workers succeeded")
	}
}

func TestWarmFilesHonorsCanceledContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, make([]byte, os.Getpagesize()), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := warmFiles(ctx, []string{path}, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestMebibytes(t *testing.T) {
	if got, err := mebibytes(128); err != nil || got != 128<<20 {
		t.Fatalf("mebibytes(128) = %d, %v", got, err)
	}
	for _, value := range []int64{0, -1, maxInt64/(1<<20) + 1} {
		if _, err := mebibytes(value); err == nil {
			t.Fatalf("mebibytes(%d) succeeded", value)
		}
	}
}

func TestPrepareCacheRejectsInvalidState(t *testing.T) {
	_, err := prepareCache(context.Background(), t.TempDir(), nil, 1, "lukewarm", 0.99, 0.01, int64(os.Getpagesize()))
	if err == nil || !strings.Contains(err.Error(), "state must be") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseExcludedDirs(t *testing.T) {
	got := parseExcludedDirs(" .git,cache,.git, ")
	if len(got) != 2 || got[0] != ".git" || got[1] != "cache" {
		t.Fatalf("excluded = %#v", got)
	}
}
