//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type procResourceSummary struct {
	Scope                        string `json:"scope"`
	SampleIntervalMS             int64  `json:"sample_interval_ms"`
	Samples                      uint64 `json:"samples"`
	SampleErrors                 uint64 `json:"sample_errors"`
	PeakProcesses                uint64 `json:"peak_processes"`
	PeakAggregateRSSBytes        uint64 `json:"peak_aggregate_rss_bytes"`
	PeakAggregateVSZBytes        uint64 `json:"peak_aggregate_vsz_bytes"`
	ProcessTreeMinorFaults       uint64 `json:"process_tree_minor_faults"`
	ProcessTreeMajorFaults       uint64 `json:"process_tree_major_faults"`
	ProcessTreeUserTicks         uint64 `json:"process_tree_user_ticks"`
	ProcessTreeSystemTicks       uint64 `json:"process_tree_system_ticks"`
	ProcessTreeReadSyscalls      uint64 `json:"process_tree_read_syscalls"`
	ProcessTreeWriteSyscalls     uint64 `json:"process_tree_write_syscalls"`
	ProcessTreeReadChars         uint64 `json:"process_tree_read_chars"`
	ProcessTreeWriteChars        uint64 `json:"process_tree_write_chars"`
	ProcessTreeStorageReadBytes  uint64 `json:"process_tree_storage_read_bytes"`
	ProcessTreeStorageWriteBytes uint64 `json:"process_tree_storage_write_bytes"`
}

type procIdentity struct {
	PID       int
	StartTime uint64
}

type procStat struct {
	PID         int
	PPID        int
	StartTime   uint64
	RSSBytes    uint64
	VSZBytes    uint64
	MinorFaults uint64
	MajorFaults uint64
	UserTicks   uint64
	SystemTicks uint64
	IO          procIO
}

type procIO struct {
	ReadSyscalls      uint64
	WriteSyscalls     uint64
	ReadChars         uint64
	WriteChars        uint64
	StorageReadBytes  uint64
	StorageWriteBytes uint64
}

func sampleProcessTree(ctx context.Context, rootPID int, interval time.Duration) procResourceSummary {
	result := procResourceSummary{
		Scope:            "sampled_procfs_descendants_by_pid_and_starttime",
		SampleIntervalMS: interval.Milliseconds(),
	}
	observed := make(map[procIdentity]procStat)
	var rootStartTime uint64
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	sample := func() {
		table, err := readProcessTable()
		if err != nil {
			result.SampleErrors++
			return
		}
		root, exists := table[rootPID]
		if !exists {
			return
		}
		if rootStartTime == 0 {
			rootStartTime = root.StartTime
		} else if root.StartTime != rootStartTime {
			return
		}
		descendants := descendantPIDs(table, rootPID)
		if len(descendants) == 0 {
			return
		}
		result.Samples++
		var aggregateRSS, aggregateVSZ uint64
		for pid := range descendants {
			stat := table[pid]
			aggregateRSS += stat.RSSBytes
			aggregateVSZ += stat.VSZBytes
			identity := procIdentity{PID: stat.PID, StartTime: stat.StartTime}
			previous := observed[identity]
			observed[identity] = maxProcCounters(previous, stat)
		}
		result.PeakProcesses = max(result.PeakProcesses, uint64(len(descendants)))
		result.PeakAggregateRSSBytes = max(result.PeakAggregateRSSBytes, aggregateRSS)
		result.PeakAggregateVSZBytes = max(result.PeakAggregateVSZBytes, aggregateVSZ)
	}

	sample()
	for {
		select {
		case <-ctx.Done():
			accumulateObserved(&result, observed)
			return result
		case <-ticker.C:
			sample()
		}
	}
}

func readProcessTable() (map[int]procStat, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	table := make(map[int]procStat)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !entry.IsDir() {
			continue
		}
		stat, err := readProcStat(pid)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				continue
			}
			continue
		}
		stat.IO, _ = readProcIO(pid)
		table[pid] = stat
	}
	return table, nil
}

func readProcStat(pid int) (procStat, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return procStat{}, err
	}
	text := string(data)
	closeParen := strings.LastIndexByte(text, ')')
	if closeParen < 0 || closeParen+2 >= len(text) {
		return procStat{}, errors.New("malformed proc stat")
	}
	fields := strings.Fields(text[closeParen+2:])
	if len(fields) < 22 {
		return procStat{}, errors.New("short proc stat")
	}
	parse := func(index int) (uint64, error) {
		return strconv.ParseUint(fields[index], 10, 64)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return procStat{}, err
	}
	minor, err := parse(7)
	if err != nil {
		return procStat{}, err
	}
	major, err := parse(9)
	if err != nil {
		return procStat{}, err
	}
	user, err := parse(11)
	if err != nil {
		return procStat{}, err
	}
	system, err := parse(12)
	if err != nil {
		return procStat{}, err
	}
	start, err := parse(19)
	if err != nil {
		return procStat{}, err
	}
	vsz, err := parse(20)
	if err != nil {
		return procStat{}, err
	}
	rssPages, err := strconv.ParseInt(fields[21], 10, 64)
	if err != nil {
		return procStat{}, err
	}
	if rssPages < 0 {
		rssPages = 0
	}
	return procStat{
		PID: pid, PPID: ppid, StartTime: start,
		RSSBytes: uint64(rssPages) * uint64(os.Getpagesize()), VSZBytes: vsz,
		MinorFaults: minor, MajorFaults: major, UserTicks: user, SystemTicks: system,
	}, nil
}

func readProcIO(pid int) (procIO, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "io"))
	if err != nil {
		return procIO{}, err
	}
	values := make(map[string]uint64)
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if err == nil {
			values[key] = parsed
		}
	}
	return procIO{
		ReadSyscalls: values["syscr"], WriteSyscalls: values["syscw"],
		ReadChars: values["rchar"], WriteChars: values["wchar"],
		StorageReadBytes: values["read_bytes"], StorageWriteBytes: values["write_bytes"],
	}, nil
}

func descendantPIDs(table map[int]procStat, rootPID int) map[int]struct{} {
	descendants := map[int]struct{}{rootPID: {}}
	changed := true
	for changed {
		changed = false
		for pid, stat := range table {
			if _, parent := descendants[stat.PPID]; !parent {
				continue
			}
			if _, exists := descendants[pid]; !exists {
				descendants[pid] = struct{}{}
				changed = true
			}
		}
	}
	for pid := range descendants {
		if _, exists := table[pid]; !exists {
			delete(descendants, pid)
		}
	}
	return descendants
}

func maxProcCounters(previous, current procStat) procStat {
	current.MinorFaults = max(previous.MinorFaults, current.MinorFaults)
	current.MajorFaults = max(previous.MajorFaults, current.MajorFaults)
	current.UserTicks = max(previous.UserTicks, current.UserTicks)
	current.SystemTicks = max(previous.SystemTicks, current.SystemTicks)
	current.IO.ReadSyscalls = max(previous.IO.ReadSyscalls, current.IO.ReadSyscalls)
	current.IO.WriteSyscalls = max(previous.IO.WriteSyscalls, current.IO.WriteSyscalls)
	current.IO.ReadChars = max(previous.IO.ReadChars, current.IO.ReadChars)
	current.IO.WriteChars = max(previous.IO.WriteChars, current.IO.WriteChars)
	current.IO.StorageReadBytes = max(previous.IO.StorageReadBytes, current.IO.StorageReadBytes)
	current.IO.StorageWriteBytes = max(previous.IO.StorageWriteBytes, current.IO.StorageWriteBytes)
	return current
}

func accumulateObserved(result *procResourceSummary, observed map[procIdentity]procStat) {
	for _, stat := range observed {
		result.ProcessTreeMinorFaults += stat.MinorFaults
		result.ProcessTreeMajorFaults += stat.MajorFaults
		result.ProcessTreeUserTicks += stat.UserTicks
		result.ProcessTreeSystemTicks += stat.SystemTicks
		result.ProcessTreeReadSyscalls += stat.IO.ReadSyscalls
		result.ProcessTreeWriteSyscalls += stat.IO.WriteSyscalls
		result.ProcessTreeReadChars += stat.IO.ReadChars
		result.ProcessTreeWriteChars += stat.IO.WriteChars
		result.ProcessTreeStorageReadBytes += stat.IO.StorageReadBytes
		result.ProcessTreeStorageWriteBytes += stat.IO.StorageWriteBytes
	}
}

func (p procStat) String() string {
	return fmt.Sprintf("pid=%d ppid=%d start=%d", p.PID, p.PPID, p.StartTime)
}
