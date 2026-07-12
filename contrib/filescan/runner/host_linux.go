//go:build linux

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const hostTelemetryScope = "corpus_device_and_host_global_observations_not_process_attributed"

var errDiskDeviceNotFound = errors.New("corpus device is absent from /proc/diskstats")

type hostTelemetry struct {
	Scope            string                 `json:"scope"`
	SampleIntervalMS int64                  `json:"sample_interval_ms"`
	SamplingRounds   uint64                 `json:"sampling_rounds"`
	SampleErrors     uint64                 `json:"sample_errors"`
	CorpusDevice     hostCorpusDevice       `json:"corpus_device"`
	Diskstats        hostDiskstatsTelemetry `json:"diskstats"`
	VMStat           hostVMStatTelemetry    `json:"vmstat"`
	MemInfo          hostMemInfoTelemetry   `json:"meminfo"`
	PressureIO       hostPressureTelemetry  `json:"pressure_io"`
}

type hostSourceStatus struct {
	Available         bool     `json:"available"`
	DeltaAvailable    bool     `json:"delta_available"`
	Samples           uint64   `json:"samples"`
	SampleErrors      uint64   `json:"sample_errors"`
	UnavailableReason string   `json:"unavailable_reason,omitempty"`
	ErrorExamples     []string `json:"error_examples,omitempty"`
}

type hostCorpusDevice struct {
	Scope        string `json:"scope"`
	Root         string `json:"root"`
	Available    bool   `json:"available"`
	Major        uint32 `json:"major"`
	Minor        uint32 `json:"minor"`
	MajorMinor   string `json:"major_minor,omitempty"`
	ResolveError string `json:"resolve_error,omitempty"`
}

type hostDiskstatsTelemetry struct {
	Scope        string             `json:"scope"`
	Status       hostSourceStatus   `json:"status"`
	DeviceName   string             `json:"device_name,omitempty"`
	Start        *diskstatsCounters `json:"start,omitempty"`
	End          *diskstatsCounters `json:"end,omitempty"`
	Delta        *diskstatsDelta    `json:"delta,omitempty"`
	PeakInFlight uint64             `json:"peak_in_flight"`
}

type diskstatsCounters struct {
	ReadSectors            uint64 `json:"read_sectors"`
	WriteSectors           uint64 `json:"write_sectors"`
	IOMilliseconds         uint64 `json:"io_milliseconds"`
	WeightedIOMilliseconds uint64 `json:"weighted_io_milliseconds"`
	InFlight               uint64 `json:"in_flight"`
}

type diskstatsDelta struct {
	ReadSectors            uint64 `json:"read_sectors"`
	WriteSectors           uint64 `json:"write_sectors"`
	IOMilliseconds         uint64 `json:"io_milliseconds"`
	WeightedIOMilliseconds uint64 `json:"weighted_io_milliseconds"`
	CounterReset           bool   `json:"counter_reset"`
}

type hostVMStatTelemetry struct {
	Scope  string           `json:"scope"`
	Status hostSourceStatus `json:"status"`
	Start  *vmstatSnapshot  `json:"start,omitempty"`
	End    *vmstatSnapshot  `json:"end,omitempty"`
	Delta  *vmstatDelta     `json:"delta,omitempty"`
}

type vmstatSnapshot struct {
	FilePages             uint64 `json:"file_pages"`
	WorkingsetRefaultFile uint64 `json:"workingset_refault_file"`
	PageInKiB             uint64 `json:"page_in_kib"`
	PageOutKiB            uint64 `json:"page_out_kib"`
}

type vmstatDelta struct {
	FilePages             int64  `json:"file_pages"`
	WorkingsetRefaultFile uint64 `json:"workingset_refault_file"`
	PageInKiB             uint64 `json:"page_in_kib"`
	PageOutKiB            uint64 `json:"page_out_kib"`
	CounterReset          bool   `json:"counter_reset"`
}

type hostMemInfoTelemetry struct {
	Scope  string           `json:"scope"`
	Status hostSourceStatus `json:"status"`
	Start  *meminfoSnapshot `json:"start,omitempty"`
	End    *meminfoSnapshot `json:"end,omitempty"`
	Delta  *meminfoDelta    `json:"delta,omitempty"`
}

type meminfoSnapshot struct {
	CachedBytes       uint64 `json:"cached_bytes"`
	SReclaimableBytes uint64 `json:"sreclaimable_bytes"`
	DirtyBytes        uint64 `json:"dirty_bytes"`
	WritebackBytes    uint64 `json:"writeback_bytes"`
	MemAvailableBytes uint64 `json:"mem_available_bytes"`
}

type meminfoDelta struct {
	CachedBytes       int64 `json:"cached_bytes"`
	SReclaimableBytes int64 `json:"sreclaimable_bytes"`
	DirtyBytes        int64 `json:"dirty_bytes"`
	WritebackBytes    int64 `json:"writeback_bytes"`
	MemAvailableBytes int64 `json:"mem_available_bytes"`
}

type hostPressureTelemetry struct {
	Scope  string            `json:"scope"`
	Status hostSourceStatus  `json:"status"`
	Start  *pressureSnapshot `json:"start,omitempty"`
	End    *pressureSnapshot `json:"end,omitempty"`
	Delta  *pressureDelta    `json:"delta,omitempty"`
}

type pressureSnapshot struct {
	SomeTotalMicroseconds uint64  `json:"some_total_microseconds"`
	FullTotalMicroseconds *uint64 `json:"full_total_microseconds,omitempty"`
}

type pressureDelta struct {
	SomeTotalMicroseconds uint64  `json:"some_total_microseconds"`
	FullTotalMicroseconds *uint64 `json:"full_total_microseconds,omitempty"`
	CounterReset          bool    `json:"counter_reset"`
}

type hostSampler struct {
	result           hostTelemetry
	diskFirst        *diskstatsCounters
	diskLast         *diskstatsCounters
	vmstatFirst      *vmstatSnapshot
	vmstatLast       *vmstatSnapshot
	meminfoFirst     *meminfoSnapshot
	meminfoLast      *meminfoSnapshot
	pressureFirst    *pressureSnapshot
	pressureLast     *pressureSnapshot
	pressureDisabled bool
}

func newHostTelemetrySkeleton(root string, interval time.Duration) hostTelemetry {
	notSampled := func() hostSourceStatus {
		return hostSourceStatus{UnavailableReason: "scanner_not_started"}
	}
	return hostTelemetry{
		Scope: hostTelemetryScope, SampleIntervalMS: interval.Milliseconds(),
		CorpusDevice: hostCorpusDevice{Scope: "filesystem_root_st_dev", Root: root},
		Diskstats: hostDiskstatsTelemetry{
			Scope: "corpus_device_only", Status: notSampled(),
		},
		VMStat: hostVMStatTelemetry{
			Scope: "host_global_not_process_attributed", Status: notSampled(),
		},
		MemInfo: hostMemInfoTelemetry{
			Scope: "host_global_not_process_attributed", Status: notSampled(),
		},
		PressureIO: hostPressureTelemetry{
			Scope: "host_global_not_process_attributed", Status: notSampled(),
		},
	}
}

func newHostSampler(root string, interval time.Duration) *hostSampler {
	result := newHostTelemetrySkeleton(root, interval)
	result.Diskstats.Status.UnavailableReason = ""
	result.VMStat.Status.UnavailableReason = ""
	result.MemInfo.Status.UnavailableReason = ""
	result.PressureIO.Status.UnavailableReason = ""
	result.CorpusDevice = resolveCorpusDevice(root)
	if !result.CorpusDevice.Available {
		result.Diskstats.Status.UnavailableReason = result.CorpusDevice.ResolveError
	}
	return &hostSampler{result: result}
}

func (sampler *hostSampler) run(ctx context.Context, interval time.Duration) hostTelemetry {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			sampler.sample()
			return sampler.finish()
		case <-ticker.C:
			sampler.sample()
		}
	}
}

func (sampler *hostSampler) sample() {
	sampler.result.SamplingRounds++
	if sampler.result.CorpusDevice.Available {
		data, err := os.ReadFile("/proc/diskstats")
		if err == nil {
			var name string
			var counters diskstatsCounters
			name, counters, err = parseDiskstats(
				data, sampler.result.CorpusDevice.Major, sampler.result.CorpusDevice.Minor,
			)
			if err == nil {
				sampler.result.Diskstats.DeviceName = name
				sampler.result.Diskstats.PeakInFlight = max(sampler.result.Diskstats.PeakInFlight, counters.InFlight)
				observe(&sampler.result.Diskstats.Status, &sampler.diskFirst, &sampler.diskLast, counters)
			}
		}
		if err != nil {
			sampler.recordError(&sampler.result.Diskstats.Status, err)
		}
	}

	data, err := os.ReadFile("/proc/vmstat")
	if err == nil {
		var snapshot vmstatSnapshot
		snapshot, err = parseVMStat(data)
		if err == nil {
			observe(&sampler.result.VMStat.Status, &sampler.vmstatFirst, &sampler.vmstatLast, snapshot)
		}
	}
	if err != nil {
		sampler.recordError(&sampler.result.VMStat.Status, err)
	}

	data, err = os.ReadFile("/proc/meminfo")
	if err == nil {
		var snapshot meminfoSnapshot
		snapshot, err = parseMemInfo(data)
		if err == nil {
			observe(&sampler.result.MemInfo.Status, &sampler.meminfoFirst, &sampler.meminfoLast, snapshot)
		}
	}
	if err != nil {
		sampler.recordError(&sampler.result.MemInfo.Status, err)
	}

	if !sampler.pressureDisabled {
		data, err = os.ReadFile("/proc/pressure/io")
		if err == nil {
			var snapshot pressureSnapshot
			snapshot, err = parsePressureIO(data)
			if err == nil {
				observe(&sampler.result.PressureIO.Status, &sampler.pressureFirst, &sampler.pressureLast, snapshot)
			}
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				sampler.pressureDisabled = true
				sampler.result.PressureIO.Status.UnavailableReason = err.Error()
			} else {
				sampler.recordError(&sampler.result.PressureIO.Status, err)
			}
		}
	}
}

func observe[T any](status *hostSourceStatus, first, last **T, value T) {
	copy := value
	if *first == nil {
		*first = &copy
	}
	*last = &copy
	status.Available = true
	status.Samples++
	status.UnavailableReason = ""
}

func (sampler *hostSampler) recordError(status *hostSourceStatus, err error) {
	status.SampleErrors++
	sampler.result.SampleErrors++
	if !status.Available {
		status.UnavailableReason = err.Error()
	}
	if len(status.ErrorExamples) < 5 {
		status.ErrorExamples = append(status.ErrorExamples, err.Error())
	}
}

func (sampler *hostSampler) finish() hostTelemetry {
	if sampler.diskFirst != nil && sampler.diskLast != nil {
		sampler.result.Diskstats.Start = sampler.diskFirst
		sampler.result.Diskstats.End = sampler.diskLast
		if sampler.result.Diskstats.Status.Samples >= 2 {
			delta := deltaDiskstats(*sampler.diskFirst, *sampler.diskLast)
			sampler.result.Diskstats.Delta = &delta
			sampler.result.Diskstats.Status.DeltaAvailable = true
		}
	}
	if sampler.vmstatFirst != nil && sampler.vmstatLast != nil {
		sampler.result.VMStat.Start = sampler.vmstatFirst
		sampler.result.VMStat.End = sampler.vmstatLast
		if sampler.result.VMStat.Status.Samples >= 2 {
			delta := deltaVMStat(*sampler.vmstatFirst, *sampler.vmstatLast)
			sampler.result.VMStat.Delta = &delta
			sampler.result.VMStat.Status.DeltaAvailable = true
		}
	}
	if sampler.meminfoFirst != nil && sampler.meminfoLast != nil {
		sampler.result.MemInfo.Start = sampler.meminfoFirst
		sampler.result.MemInfo.End = sampler.meminfoLast
		if sampler.result.MemInfo.Status.Samples >= 2 {
			delta := deltaMemInfo(*sampler.meminfoFirst, *sampler.meminfoLast)
			sampler.result.MemInfo.Delta = &delta
			sampler.result.MemInfo.Status.DeltaAvailable = true
		}
	}
	if sampler.pressureFirst != nil && sampler.pressureLast != nil {
		sampler.result.PressureIO.Start = sampler.pressureFirst
		sampler.result.PressureIO.End = sampler.pressureLast
		if sampler.result.PressureIO.Status.Samples >= 2 {
			delta := deltaPressure(*sampler.pressureFirst, *sampler.pressureLast)
			sampler.result.PressureIO.Delta = &delta
			sampler.result.PressureIO.Status.DeltaAvailable = true
		}
	}
	return sampler.result
}

func resolveCorpusDevice(root string) hostCorpusDevice {
	result := hostCorpusDevice{Scope: "filesystem_root_st_dev", Root: root}
	info, err := os.Stat(root)
	if err != nil {
		result.ResolveError = err.Error()
		return result
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		result.ResolveError = "filesystem stat has no Linux Stat_t"
		return result
	}
	result.Major, result.Minor = linuxDeviceNumbers(uint64(stat.Dev))
	result.MajorMinor = fmt.Sprintf("%d:%d", result.Major, result.Minor)
	result.Available = true
	return result
}

func linuxDeviceNumbers(device uint64) (uint32, uint32) {
	major := uint32((device&0x00000000000fff00)>>8 | (device&0xfffff00000000000)>>32)
	minor := uint32(device&0x00000000000000ff | (device&0x00000ffffff00000)>>12)
	return major, minor
}

func parseDiskstats(data []byte, major, minor uint32) (string, diskstatsCounters, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 14 {
			continue
		}
		observedMajor, majorErr := strconv.ParseUint(fields[0], 10, 32)
		observedMinor, minorErr := strconv.ParseUint(fields[1], 10, 32)
		if majorErr != nil || minorErr != nil || uint32(observedMajor) != major || uint32(observedMinor) != minor {
			continue
		}
		values := make([]uint64, 5)
		for i, index := range []int{5, 9, 12, 13, 11} {
			value, err := strconv.ParseUint(fields[index], 10, 64)
			if err != nil {
				return "", diskstatsCounters{}, fmt.Errorf("parse /proc/diskstats field %d: %w", index, err)
			}
			values[i] = value
		}
		return fields[2], diskstatsCounters{
			ReadSectors: values[0], WriteSectors: values[1],
			IOMilliseconds: values[2], WeightedIOMilliseconds: values[3], InFlight: values[4],
		}, nil
	}
	if err := scanner.Err(); err != nil {
		return "", diskstatsCounters{}, err
	}
	return "", diskstatsCounters{}, errDiskDeviceNotFound
}

func parseVMStat(data []byte) (vmstatSnapshot, error) {
	values, err := parseUintKeyValues(data)
	if err != nil {
		return vmstatSnapshot{}, fmt.Errorf("parse /proc/vmstat: %w", err)
	}
	required := []string{"nr_file_pages", "workingset_refault_file", "pgpgin", "pgpgout"}
	for _, key := range required {
		if _, ok := values[key]; !ok {
			return vmstatSnapshot{}, fmt.Errorf("parse /proc/vmstat: missing %s", key)
		}
	}
	return vmstatSnapshot{
		FilePages: values["nr_file_pages"], WorkingsetRefaultFile: values["workingset_refault_file"],
		PageInKiB: values["pgpgin"], PageOutKiB: values["pgpgout"],
	}, nil
}

func parseMemInfo(data []byte) (meminfoSnapshot, error) {
	values := make(map[string]uint64)
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return meminfoSnapshot{}, fmt.Errorf("parse /proc/meminfo %s: %w", key, err)
		}
		if len(fields) >= 3 && fields[2] != "kB" {
			continue
		}
		if value > math.MaxUint64/1024 {
			return meminfoSnapshot{}, fmt.Errorf("parse /proc/meminfo %s: value overflows bytes", key)
		}
		values[key] = value * 1024
	}
	if err := scanner.Err(); err != nil {
		return meminfoSnapshot{}, err
	}
	for _, key := range []string{"Cached", "SReclaimable", "Dirty", "Writeback", "MemAvailable"} {
		if _, ok := values[key]; !ok {
			return meminfoSnapshot{}, fmt.Errorf("parse /proc/meminfo: missing %s", key)
		}
	}
	return meminfoSnapshot{
		CachedBytes: values["Cached"], SReclaimableBytes: values["SReclaimable"],
		DirtyBytes: values["Dirty"], WritebackBytes: values["Writeback"],
		MemAvailableBytes: values["MemAvailable"],
	}, nil
}

func parsePressureIO(data []byte) (pressureSnapshot, error) {
	var result pressureSnapshot
	foundSome := false
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		var total *uint64
		for _, field := range fields[1:] {
			text, ok := strings.CutPrefix(field, "total=")
			if !ok {
				continue
			}
			value, err := strconv.ParseUint(text, 10, 64)
			if err != nil {
				return pressureSnapshot{}, fmt.Errorf("parse /proc/pressure/io total: %w", err)
			}
			total = &value
			break
		}
		if total == nil {
			continue
		}
		switch fields[0] {
		case "some":
			result.SomeTotalMicroseconds = *total
			foundSome = true
		case "full":
			result.FullTotalMicroseconds = total
		}
	}
	if err := scanner.Err(); err != nil {
		return pressureSnapshot{}, err
	}
	if !foundSome {
		return pressureSnapshot{}, errors.New("parse /proc/pressure/io: missing some total")
	}
	return result, nil
}

func parseUintKeyValues(data []byte) (map[string]uint64, error) {
	values := make(map[string]uint64)
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", fields[0], err)
		}
		values[fields[0]] = value
	}
	return values, scanner.Err()
}

func deltaDiskstats(start, end diskstatsCounters) diskstatsDelta {
	read, readReset := monotonicDelta(start.ReadSectors, end.ReadSectors)
	write, writeReset := monotonicDelta(start.WriteSectors, end.WriteSectors)
	ioMilliseconds, ioReset := monotonicDelta(start.IOMilliseconds, end.IOMilliseconds)
	weighted, weightedReset := monotonicDelta(start.WeightedIOMilliseconds, end.WeightedIOMilliseconds)
	return diskstatsDelta{
		ReadSectors: read, WriteSectors: write, IOMilliseconds: ioMilliseconds,
		WeightedIOMilliseconds: weighted,
		CounterReset:           readReset || writeReset || ioReset || weightedReset,
	}
}

func deltaVMStat(start, end vmstatSnapshot) vmstatDelta {
	refault, refaultReset := monotonicDelta(start.WorkingsetRefaultFile, end.WorkingsetRefaultFile)
	pageIn, pageInReset := monotonicDelta(start.PageInKiB, end.PageInKiB)
	pageOut, pageOutReset := monotonicDelta(start.PageOutKiB, end.PageOutKiB)
	return vmstatDelta{
		FilePages:             signedDelta(start.FilePages, end.FilePages),
		WorkingsetRefaultFile: refault, PageInKiB: pageIn, PageOutKiB: pageOut,
		CounterReset: refaultReset || pageInReset || pageOutReset,
	}
}

func deltaMemInfo(start, end meminfoSnapshot) meminfoDelta {
	return meminfoDelta{
		CachedBytes:       signedDelta(start.CachedBytes, end.CachedBytes),
		SReclaimableBytes: signedDelta(start.SReclaimableBytes, end.SReclaimableBytes),
		DirtyBytes:        signedDelta(start.DirtyBytes, end.DirtyBytes),
		WritebackBytes:    signedDelta(start.WritebackBytes, end.WritebackBytes),
		MemAvailableBytes: signedDelta(start.MemAvailableBytes, end.MemAvailableBytes),
	}
}

func deltaPressure(start, end pressureSnapshot) pressureDelta {
	some, someReset := monotonicDelta(start.SomeTotalMicroseconds, end.SomeTotalMicroseconds)
	result := pressureDelta{SomeTotalMicroseconds: some, CounterReset: someReset}
	if start.FullTotalMicroseconds != nil && end.FullTotalMicroseconds != nil {
		full, fullReset := monotonicDelta(*start.FullTotalMicroseconds, *end.FullTotalMicroseconds)
		result.FullTotalMicroseconds = &full
		result.CounterReset = result.CounterReset || fullReset
	}
	return result
}

func monotonicDelta(start, end uint64) (uint64, bool) {
	if end < start {
		return 0, true
	}
	return end - start, false
}

func signedDelta(start, end uint64) int64 {
	if end >= start {
		delta := end - start
		if delta > math.MaxInt64 {
			return math.MaxInt64
		}
		return int64(delta)
	}
	delta := start - end
	if delta > math.MaxInt64 {
		return math.MinInt64
	}
	return -int64(delta)
}
