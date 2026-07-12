//go:build linux

package main

import (
	"strings"
	"testing"
)

func TestParseDiskstatsAndDelta(t *testing.T) {
	fixture := []byte(strings.Join([]string{
		"  8 0 sda 1 0 2 3 4 0 5 6 0 7 8",
		"259 1 nvme0n1p1 10 2 100 20 30 4 200 40 3 50 60 0 0 0 0 0 0",
	}, "\n"))
	name, counters, err := parseDiskstats(fixture, 259, 1)
	if err != nil {
		t.Fatal(err)
	}
	if name != "nvme0n1p1" || counters.ReadSectors != 100 || counters.WriteSectors != 200 ||
		counters.IOMilliseconds != 50 || counters.WeightedIOMilliseconds != 60 || counters.InFlight != 3 {
		t.Fatalf("unexpected diskstats: name=%q counters=%#v", name, counters)
	}
	end := counters
	end.ReadSectors += 25
	end.WriteSectors += 50
	end.IOMilliseconds += 7
	end.WeightedIOMilliseconds += 9
	delta := deltaDiskstats(counters, end)
	if delta.ReadSectors != 25 || delta.WriteSectors != 50 || delta.IOMilliseconds != 7 ||
		delta.WeightedIOMilliseconds != 9 || delta.CounterReset {
		t.Fatalf("unexpected diskstats delta: %#v", delta)
	}
	if _, _, err := parseDiskstats(fixture, 259, 2); err == nil {
		t.Fatal("missing corpus device was accepted")
	}
}

func TestParseVMStatAndDelta(t *testing.T) {
	start, err := parseVMStat([]byte(strings.Join([]string{
		"nr_file_pages 1000",
		"workingset_refault_file 200",
		"pgpgin 300",
		"pgpgout 400",
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	end := vmstatSnapshot{
		FilePages: 980, WorkingsetRefaultFile: 215, PageInKiB: 325, PageOutKiB: 440,
	}
	delta := deltaVMStat(start, end)
	if delta.FilePages != -20 || delta.WorkingsetRefaultFile != 15 || delta.PageInKiB != 25 ||
		delta.PageOutKiB != 40 || delta.CounterReset {
		t.Fatalf("unexpected vmstat delta: %#v", delta)
	}
	end.PageInKiB = 1
	if delta := deltaVMStat(start, end); !delta.CounterReset || delta.PageInKiB != 0 {
		t.Fatalf("counter reset was not retained: %#v", delta)
	}
	if _, err := parseVMStat([]byte("nr_file_pages 1\n")); err == nil {
		t.Fatal("incomplete vmstat fixture was accepted")
	}
}

func TestParseMemInfoAndDelta(t *testing.T) {
	start, err := parseMemInfo([]byte(strings.Join([]string{
		"MemAvailable: 1000 kB",
		"Cached: 200 kB",
		"SReclaimable: 30 kB",
		"Dirty: 4 kB",
		"Writeback: 5 kB",
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if start.CachedBytes != 200*1024 || start.MemAvailableBytes != 1000*1024 {
		t.Fatalf("meminfo was not converted to bytes: %#v", start)
	}
	end := start
	end.CachedBytes += 8 * 1024
	end.DirtyBytes -= 2 * 1024
	delta := deltaMemInfo(start, end)
	if delta.CachedBytes != 8*1024 || delta.DirtyBytes != -2*1024 {
		t.Fatalf("unexpected meminfo delta: %#v", delta)
	}
	if _, err := parseMemInfo([]byte("Cached: 1 kB\n")); err == nil {
		t.Fatal("incomplete meminfo fixture was accepted")
	}
}

func TestParsePressureIOAndDelta(t *testing.T) {
	start, err := parsePressureIO([]byte(strings.Join([]string{
		"some avg10=0.10 avg60=0.20 avg300=0.30 total=1234",
		"full avg10=0.01 avg60=0.02 avg300=0.03 total=234",
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	full := uint64(250)
	end := pressureSnapshot{SomeTotalMicroseconds: 1300, FullTotalMicroseconds: &full}
	delta := deltaPressure(start, end)
	if delta.SomeTotalMicroseconds != 66 || delta.FullTotalMicroseconds == nil ||
		*delta.FullTotalMicroseconds != 16 || delta.CounterReset {
		t.Fatalf("unexpected pressure delta: %#v", delta)
	}
	if _, err := parsePressureIO([]byte("full total=1\n")); err == nil {
		t.Fatal("pressure fixture without some total was accepted")
	}
}

func TestLinuxDeviceNumbers(t *testing.T) {
	for _, pair := range [][2]uint32{{8, 0}, {259, 1}, {4097, 65537}} {
		device := uint64(pair[1]&0xff) |
			uint64(pair[0]&0xfff)<<8 |
			uint64(pair[1]&^0xff)<<12 |
			uint64(pair[0]&^0xfff)<<32
		major, minor := linuxDeviceNumbers(device)
		if major != pair[0] || minor != pair[1] {
			t.Fatalf("device %d decoded as %d:%d, want %d:%d", device, major, minor, pair[0], pair[1])
		}
	}
}
