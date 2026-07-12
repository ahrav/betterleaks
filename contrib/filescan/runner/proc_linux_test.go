//go:build linux

package main

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestReadProcStatForCurrentProcess(t *testing.T) {
	stat, err := readProcStat(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if stat.PID != os.Getpid() || stat.StartTime == 0 || stat.VSZBytes == 0 {
		t.Fatalf("unexpected proc stat: %#v", stat)
	}
	if _, err := readProcIO(os.Getpid()); err != nil {
		t.Fatal(err)
	}
}

func TestDescendantPIDs(t *testing.T) {
	table := map[int]procStat{
		10: {PID: 10, PPID: 1},
		11: {PID: 11, PPID: 10},
		12: {PID: 12, PPID: 11},
		20: {PID: 20, PPID: 1},
	}
	got := descendantPIDs(table, 10)
	for _, pid := range []int{10, 11, 12} {
		if _, exists := got[pid]; !exists {
			t.Fatalf("missing descendant %d: %#v", pid, got)
		}
	}
	if _, exists := got[20]; exists {
		t.Fatalf("included unrelated pid: %#v", got)
	}
}

func TestSampleProcessTree(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	summary := sampleProcessTree(ctx, os.Getpid(), 10*time.Millisecond)
	if summary.Samples == 0 || summary.PeakProcesses == 0 || summary.PeakAggregateRSSBytes == 0 {
		t.Fatalf("sampler did not observe current process: %#v", summary)
	}
}

func TestSampleProcessTreeProcessAlreadyGone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	summary := sampleProcessTree(ctx, -1, time.Second)
	if summary.Samples != 0 || summary.SampleErrors != 0 {
		t.Fatalf("gone process was reported as a sampler failure: %#v", summary)
	}
}
