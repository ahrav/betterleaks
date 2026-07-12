package sources

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestFileScanConfigNormalizeAndValidate(t *testing.T) {
	metrics := NewFileScanMetrics()
	got := (FileScanConfig{Metrics: metrics}).Normalize()
	if got.Backend != FileScanBackendBaseline {
		t.Fatalf("backend = %q, want %q", got.Backend, FileScanBackendBaseline)
	}
	if got.Namespace != FileScanNamespaceParallel {
		t.Fatalf("namespace = %q, want %q", got.Namespace, FileScanNamespaceParallel)
	}
	if got.ReadMode != FileScanReadModeRead {
		t.Fatalf("read mode = %q, want %q", got.ReadMode, FileScanReadModeRead)
	}
	if got.ActiveFiles <= 0 || got.DetectorWorkers <= 0 || got.Walkers <= 0 {
		t.Fatalf("non-positive normalized concurrency: %+v", got)
	}
	if got.MaxInFlightBytes <= 0 {
		t.Fatalf("max in-flight bytes = %d, want positive", got.MaxInFlightBytes)
	}
	if got.Metrics != metrics {
		t.Fatal("Normalize did not retain Metrics")
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("default config did not validate: %v", err)
	}
	if got.MemoryCost() < int64(got.ActiveFiles*defaultBufferSize) {
		t.Fatalf("memory cost %d excludes framing buffers", got.MemoryCost())
	}

	backends := []FileScanBackend{
		FileScanBackendBaseline,
		FileScanBackendBuffered,
		FileScanBackendMmap,
		FileScanBackendDirect,
		FileScanBackendUring,
		FileScanBackendUringDirect,
	}
	for _, backend := range backends {
		t.Run(string(backend), func(t *testing.T) {
			config := FileScanConfig{Backend: backend}.Normalize()
			if err := config.Validate(); err != nil {
				t.Fatalf("normalized config did not validate: %+v: %v", config, err)
			}
			if config.MemoryCost() <= 0 {
				t.Fatalf("memory cost = %d, want positive", config.MemoryCost())
			}
		})
	}

	mmap := (FileScanConfig{
		Backend:         FileScanBackendMmap,
		MmapWindowBytes: 128 * 1024 * 1024,
		Advice:          []string{"SEQUENTIAL", "madv_sequential", "sequential"},
	}).Normalize()
	if mmap.MaxInFlightBytes < mmap.MmapWindowBytes {
		t.Fatalf("default max in-flight %d is below mmap window %d", mmap.MaxInFlightBytes, mmap.MmapWindowBytes)
	}
	wantAdvice := []string{string(FileScanAdviceMadvSequential)}
	if !reflect.DeepEqual(mmap.Advice, wantAdvice) {
		t.Fatalf("advice = %#v, want %#v", mmap.Advice, wantAdvice)
	}
	if err := mmap.Validate(); err != nil {
		t.Fatalf("mmap config did not validate: %v", err)
	}

	buffered := (FileScanConfig{
		Backend: FileScanBackendBuffered,
		Advice:  []string{"noreuse", "FADV_SEQUENTIAL", "noreuse", "will-need", "willneed"},
	}).Normalize()
	wantAdvice = []string{
		string(FileScanAdviceFadvNoReuse),
		string(FileScanAdviceFadvSequential),
		string(FileScanAdviceFadvWillNeed),
	}
	if !reflect.DeepEqual(buffered.Advice, wantAdvice) {
		t.Fatalf("advice = %#v, want %#v", buffered.Advice, wantAdvice)
	}
}

func TestFileScanConfigValidationFailures(t *testing.T) {
	tests := map[string]FileScanConfig{
		"backend": {
			Backend: "mystery",
		},
		"namespace": {
			Namespace: "mystery",
		},
		"read-mode": {
			ReadMode: "mystery",
		},
		"pread-on-baseline": {
			ReadMode: FileScanReadModePread,
		},
		"negative-prefetch": {
			PrefetchBytes: -1,
		},
		"non-positive-active-files": {
			ActiveFiles: -1,
		},
		"registered-baseline": {
			RegisteredBuffers: true,
		},
		"mmap-advice-on-buffered": {
			Backend: FileScanBackendBuffered,
			Advice:  []string{string(FileScanAdviceMadvSequential)},
		},
		"fadv-on-baseline": {
			Advice: []string{string(FileScanAdviceFadvSequential)},
		},
		"unknown-advice": {
			Advice: []string{"surprise"},
		},
		"mmap-over-budget": {
			Backend:          FileScanBackendMmap,
			MmapWindowBytes:  128,
			MaxInFlightBytes: 64,
		},
		"mmap-unaligned": {
			Backend:         FileScanBackendMmap,
			MmapWindowBytes: int64(os.Getpagesize()) + 1,
		},
		"baseline-framing-over-budget": {
			Backend:          FileScanBackendBaseline,
			MaxInFlightBytes: 64,
		},
		"queue-on-buffered": {
			Backend:    FileScanBackendBuffered,
			QueueDepth: 1,
		},
		"uring-queue-too-deep": {
			Backend:    FileScanBackendUring,
			QueueDepth: maxFileScanUringQueueDepth + 1,
		},
		"uring-arena-over-budget": {
			Backend:          FileScanBackendUring,
			QueueDepth:       32,
			PrefetchBytes:    512 * 1024,
			MaxInFlightBytes: 1024 * 1024,
		},
		"registered-over-budget": {
			Backend:           FileScanBackendUring,
			QueueDepth:        32,
			PrefetchBytes:     512 * 1024,
			RegisteredBuffers: true,
			MaxInFlightBytes:  1024 * 1024,
		},
		"buffered-uring-namespace": {
			Backend:   FileScanBackendBuffered,
			Namespace: FileScanNamespaceUringOpenat,
		},
		"mmap-uring-namespace": {
			Backend:   FileScanBackendMmap,
			Namespace: FileScanNamespaceUringOpenat,
		},
		"direct-uring-namespace": {
			Backend:   FileScanBackendDirect,
			Namespace: FileScanNamespaceUringOpenat,
		},
	}
	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			if err := config.Validate(); err == nil {
				t.Fatalf("config unexpectedly validated: %+v", config.Normalize())
			}
		})
	}
}

func TestBaselineUringNamespaceUsesQueueOnly(t *testing.T) {
	cfg := (FileScanConfig{
		Backend:   FileScanBackendBaseline,
		Namespace: FileScanNamespaceUringOpenat,
	}).Normalize()
	if cfg.QueueDepth != defaultFileScanQueueDepth {
		t.Fatalf("normalized queue depth = %d, want %d", cfg.QueueDepth, defaultFileScanQueueDepth)
	}
	if cfg.PrefetchBytes != 0 || cfg.MmapWindowBytes != 0 || cfg.RegisteredBuffers {
		t.Fatalf("namespace-only config acquired content options: %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("baseline uring-openat config did not validate: %v", err)
	}

	invalid := []struct {
		name string
		cfg  FileScanConfig
	}{
		{
			name: "queue without uring namespace",
			cfg: FileScanConfig{
				Backend: FileScanBackendBaseline, QueueDepth: 4,
			},
		},
		{
			name: "prefetch",
			cfg: FileScanConfig{
				Backend: FileScanBackendBaseline, Namespace: FileScanNamespaceUringOpenat,
				QueueDepth: 4, PrefetchBytes: 4096,
			},
		},
		{
			name: "mmap window",
			cfg: FileScanConfig{
				Backend: FileScanBackendBaseline, Namespace: FileScanNamespaceUringOpenat,
				QueueDepth: 4, MmapWindowBytes: 4096,
			},
		},
		{
			name: "registered buffers",
			cfg: FileScanConfig{
				Backend: FileScanBackendBaseline, Namespace: FileScanNamespaceUringOpenat,
				QueueDepth: 4, RegisteredBuffers: true,
			},
		},
		{
			name: "queue over Linux maximum",
			cfg: FileScanConfig{
				Backend: FileScanBackendBaseline, Namespace: FileScanNamespaceUringOpenat,
				QueueDepth: maxFileScanUringQueueDepth + 1,
			},
		},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if err := test.cfg.Validate(); err == nil {
				t.Fatalf("config unexpectedly validated: %+v", test.cfg.Normalize())
			}
		})
	}
}

func TestRegisteredUringBudgetUsesPageRoundedArena(t *testing.T) {
	page := os.Getpagesize()
	const depth = 2
	prefetch := page + 1
	logicalBytes := int64(depth * prefetch)
	config := FileScanConfig{
		Backend:           FileScanBackendUring,
		QueueDepth:        depth,
		PrefetchBytes:     prefetch,
		RegisteredBuffers: true,
		MaxInFlightBytes:  logicalBytes + int64(defaultBufferSize+4096),
		ActiveFiles:       1,
	}
	if err := config.Validate(); err == nil {
		t.Fatalf("logical-sized budget accepted page-rounded registered arena: %+v", config)
	}
	wantRounded := int64(depth * 2 * page)
	if got := saturatingFileScanMul(depth, fileScanRoundedBufferBytes(prefetch)); got != wantRounded {
		t.Fatalf("rounded arena = %d, want %d", got, wantRounded)
	}
}

func TestFileScanMetricsOrderIndependentMultisets(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "corpus")
	targets := []ScanTarget{
		{Path: filepath.Join(root, "a.txt")},
		{Path: filepath.Join(root, "b.txt"), Symlink: filepath.Join(root, "link-b")},
		{Path: filepath.Join(root, "c.txt")},
	}
	fragments := []Fragment{
		{Raw: "alpha", StartLine: 1, Attributes: map[string]string{AttrPath: targets[0].Path}},
		{Raw: "beta", StartLine: 8, Attributes: map[string]string{AttrPath: targets[1].Path, AttrFSSymlink: targets[1].Symlink}},
		{Raw: "gamma", StartLine: 3, Attributes: map[string]string{AttrPath: targets[2].Path}},
	}
	findings := [][]byte{[]byte("finding-a"), []byte("finding-b"), []byte("finding-c")}
	ledger := [][3]string{
		{"error", targets[0].Path, "permission denied"},
		{"skip", targets[1].Path, "too large"},
		{"fallback", targets[2].Path, "unsupported"},
	}

	record := func(reverse bool) FileScanDigestsSnapshot {
		metrics := NewFileScanMetrics()
		for step := range len(targets) {
			i := step
			if reverse {
				i = len(targets) - 1 - step
			}
			metrics.RecordTarget(root, targets[i])
			metrics.RecordFragment(root, fragments[i])
			metrics.RecordFinding(findings[i])
			metrics.RecordLedger(ledger[i][0], ledger[i][1], ledger[i][2])
		}
		return metrics.Snapshot().Digests
	}

	forward := record(false)
	reverse := record(true)
	if forward != reverse {
		t.Fatalf("multiset digests depend on record order:\nforward: %+v\nreverse: %+v", forward, reverse)
	}
}

func TestFileScanMetricsDuplicateSensitivity(t *testing.T) {
	root := t.TempDir()
	target := ScanTarget{Path: filepath.Join(root, "file.txt")}
	fragment := Fragment{
		Raw:        "raw",
		StartLine:  2,
		Attributes: map[string]string{AttrPath: target.Path},
	}

	record := func(copies int) FileScanDigestsSnapshot {
		metrics := NewFileScanMetrics()
		for range copies {
			metrics.RecordTarget(root, target)
			metrics.RecordFragment(root, fragment)
			metrics.RecordFinding([]byte("finding"))
			metrics.RecordLedger("error", target.Path, "boom")
		}
		return metrics.Snapshot().Digests
	}

	one := record(1)
	two := record(2)
	checks := []struct {
		name string
		one  FileScanDigestSnapshot
		two  FileScanDigestSnapshot
	}{
		{"targets", one.Targets, two.Targets},
		{"fragments", one.Fragments, two.Fragments},
		{"findings", one.Findings, two.Findings},
		{"ledger", one.Ledger, two.Ledger},
	}
	for _, check := range checks {
		if check.one.Count != 1 || check.two.Count != 2 {
			t.Fatalf("%s counts = %d and %d, want 1 and 2", check.name, check.one.Count, check.two.Count)
		}
		if check.one.SHA256 == check.two.SHA256 {
			t.Fatalf("%s digest did not change when a duplicate was added", check.name)
		}
	}
}

func TestFileScanMetricsCanonicalRelativePaths(t *testing.T) {
	root := t.TempDir()
	canonicalTarget := ScanTarget{
		Path:    filepath.Join(root, "dir", "file.txt"),
		Symlink: filepath.Join(root, "file-link"),
	}
	uncleanTarget := ScanTarget{
		Path:    filepath.Join(root, "discard", "..", "dir", ".", "file.txt"),
		Symlink: filepath.Join(root, "discard", "..", "file-link"),
	}
	canonicalFragment := Fragment{
		Raw:       "same",
		StartLine: 4,
		Attributes: map[string]string{
			AttrPath:      canonicalTarget.Path,
			AttrFSSymlink: canonicalTarget.Symlink,
		},
	}
	uncleanFragment := canonicalFragment
	uncleanFragment.Attributes = map[string]string{
		AttrPath:      uncleanTarget.Path,
		AttrFSSymlink: uncleanTarget.Symlink,
	}

	left := NewFileScanMetrics()
	left.RecordTarget(root+string(filepath.Separator), canonicalTarget)
	left.RecordFragment(root+string(filepath.Separator), canonicalFragment)
	right := NewFileScanMetrics()
	right.RecordTarget(filepath.Join(root, "."), uncleanTarget)
	right.RecordFragment(filepath.Join(root, "."), uncleanFragment)

	leftDigests := left.Snapshot().Digests
	rightDigests := right.Snapshot().Digests
	if leftDigests.Targets != rightDigests.Targets {
		t.Fatalf("equivalent target paths changed digest: %+v != %+v", leftDigests.Targets, rightDigests.Targets)
	}
	if leftDigests.Fragments != rightDigests.Fragments {
		t.Fatalf("equivalent fragment paths changed digest: %+v != %+v", leftDigests.Fragments, rightDigests.Fragments)
	}

	otherRoot := t.TempDir()
	translated := NewFileScanMetrics()
	translated.RecordTarget(otherRoot, ScanTarget{
		Path:    filepath.Join(otherRoot, "dir", "file.txt"),
		Symlink: filepath.Join(otherRoot, "file-link"),
	})
	if translated.Snapshot().Digests.Targets != leftDigests.Targets {
		t.Fatal("target digest retained the absolute corpus root")
	}
}

func TestFileScanLocalMetricsHistogramAndMerge(t *testing.T) {
	local := NewFileScanLocal()
	local.RecordPhase(FileScanPhaseReadWait, 1, nil, 0)
	local.RecordPhase(FileScanPhaseReadWait, 2, nil, time.Nanosecond)
	local.RecordPhase(FileScanPhaseReadWait, 3, errors.New("read"), 2*time.Nanosecond)
	local.RecordPhase(FileScanPhaseReadWait, 4, nil, 3*time.Nanosecond)
	local.StartPhase(FileScanPhaseOpen).Done(7, nil)
	local.StartPhase(FileScanPhaseClose).Done(0, nil)

	metrics := NewFileScanMetrics()
	metrics.MergeLocal(local)
	// MergeLocal consumes the accumulated values, preventing an accidental
	// second merge from double-counting the same file.
	metrics.MergeLocal(local)
	snapshot := metrics.Snapshot()
	read := snapshot.Phases[FileScanPhaseReadWait]
	if read.Count != 4 || read.Bytes != 10 || read.Errors != 1 || read.Nanoseconds != 6 {
		t.Fatalf("read phase = %+v", read)
	}
	if read.LatencyLog2NS[0] != 1 || read.LatencyLog2NS[1] != 1 || read.LatencyLog2NS[2] != 2 {
		t.Fatalf("unexpected histogram buckets: %#v", read.LatencyLog2NS[:4])
	}
	if open := snapshot.Phases[FileScanPhaseOpen]; open.Count != 1 || open.Bytes != 7 {
		t.Fatalf("open phase = %+v", open)
	}
	if closePhase := snapshot.Phases[FileScanPhaseClose]; closePhase.Count != 1 || closePhase.Bytes != 0 {
		t.Fatalf("close phase = %+v", closePhase)
	}
}

func TestFileScanLocalMetricsConcurrentRecord(t *testing.T) {
	const recordsPerPhase = 2_000
	phases := []string{
		FileScanPhaseReadWait,
		FileScanPhaseFraming,
		FileScanPhaseDecompression,
		FileScanPhaseDetector,
	}
	local := NewFileScanLocal()
	var wg sync.WaitGroup
	for _, phase := range phases {
		wg.Go(func() {
			for range recordsPerPhase {
				local.RecordPhase(phase, 1, nil, time.Nanosecond)
			}
		})
	}
	wg.Wait()

	metrics := NewFileScanMetrics()
	metrics.MergeLocal(local)
	snapshot := metrics.Snapshot()
	for _, phase := range phases {
		got := snapshot.Phases[phase]
		if got.Count != recordsPerPhase || got.Bytes != recordsPerPhase || got.Nanoseconds != recordsPerPhase {
			t.Errorf("phase %q = %+v, want %d one-byte one-nanosecond records", phase, got, recordsPerPhase)
		}
	}
}

func TestFileScanMetricsMergeLocalConcurrentWithRecord(t *testing.T) {
	const (
		workers          = 8
		recordsPerWorker = 2_000
	)
	local := NewFileScanLocal()
	metrics := NewFileScanMetrics()
	start := make(chan struct{})
	writersDone := make(chan struct{})
	mergerDone := make(chan struct{})

	go func() {
		defer close(mergerDone)
		<-start
		for {
			select {
			case <-writersDone:
				return
			default:
				metrics.MergeLocal(local)
			}
		}
	}()

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			<-start
			for range recordsPerWorker {
				local.RecordPhase(FileScanPhaseReadWait, 1, nil, time.Nanosecond)
			}
		})
	}
	close(start)
	wg.Wait()
	close(writersDone)
	<-mergerDone
	metrics.MergeLocal(local)

	got := metrics.Snapshot().Phases[FileScanPhaseReadWait]
	want := uint64(workers * recordsPerWorker)
	if got.Count != want || got.Bytes != want || got.Nanoseconds != want {
		t.Fatalf("merged phase = %+v, want %d one-byte one-nanosecond records", got, want)
	}
}

func TestFileScanMetricsConcurrent(t *testing.T) {
	const (
		workers = 24
		per     = 100
	)
	root := t.TempDir()
	metrics := NewFileScanMetrics()
	var wg sync.WaitGroup
	wg.Add(workers)
	for worker := range workers {
		go func() {
			defer wg.Done()
			local := metrics.NewLocal()
			for item := range per {
				path := filepath.Join(root, fmt.Sprintf("worker-%d", worker), fmt.Sprintf("file-%d", item))
				var phaseErr error
				if item%10 == 0 {
					phaseErr = errors.New("expected")
				}
				local.RecordPhase(FileScanPhaseReadWait, uint64(item+1), phaseErr, time.Duration(item)*time.Nanosecond)
				metrics.RecordTarget(root, ScanTarget{Path: path})
				metrics.RecordFragment(root, Fragment{
					Raw:        fmt.Sprintf("raw-%d-%d", worker, item),
					StartLine:  item + 1,
					Attributes: map[string]string{AttrPath: path},
				})
				metrics.RecordFinding([]byte(fmt.Sprintf("finding-%d-%d", worker, item)))
				metrics.RecordBackend(string(FileScanBackendBaseline), uint64(item+1))
				metrics.AddGauge(FileScanGaugeActiveFiles, 1)
				metrics.AddGauge(FileScanGaugeActiveFiles, -1)
				if item%10 == 0 {
					metrics.RecordFallback("unsupported")
				}
			}
			metrics.MergeLocal(local)
		}()
	}
	wg.Wait()

	snapshot := metrics.Snapshot()
	want := uint64(workers * per)
	phase := snapshot.Phases[FileScanPhaseReadWait]
	if phase.Count != want || phase.Errors != workers*(per/10) {
		t.Fatalf("phase count/errors = %d/%d, want %d/%d", phase.Count, phase.Errors, want, workers*(per/10))
	}
	backend := snapshot.Backends[string(FileScanBackendBaseline)]
	if backend.Files != want {
		t.Fatalf("backend files = %d, want %d", backend.Files, want)
	}
	if snapshot.Digests.Targets.Count != want || snapshot.Digests.Fragments.Count != want || snapshot.Digests.Findings.Count != want {
		t.Fatalf("digest counts = %+v, want %d each", snapshot.Digests, want)
	}
	wantFallbacks := uint64(workers * (per / 10))
	if snapshot.FallbackReasons["unsupported"] != wantFallbacks || snapshot.Digests.Ledger.Count != wantFallbacks {
		t.Fatalf("fallbacks/ledger = %d/%d, want %d", snapshot.FallbackReasons["unsupported"], snapshot.Digests.Ledger.Count, wantFallbacks)
	}
	gauge := snapshot.Resources[FileScanGaugeActiveFiles]
	if gauge.Current != 0 || gauge.Peak < 1 || gauge.Peak > workers {
		t.Fatalf("active-files gauge = %+v", gauge)
	}
}

func TestFileScanMetricsSnapshotSchemaAndNilNoop(t *testing.T) {
	var disabled *FileScanMetrics
	if local := disabled.NewLocal(); local != nil {
		t.Fatal("nil metrics allocated local timing")
	}
	disabled.RecordPhase("phase", 1, errors.New("ignored"), time.Second)
	disabled.RecordTarget("", ScanTarget{Path: "ignored"})
	disabled.RecordFragment("", Fragment{Raw: "ignored"})
	disabled.RecordFinding([]byte("ignored"))
	disabled.RecordLedger("error", "ignored", "ignored")
	disabled.RecordFallback("ignored")
	disabled.RecordBackend("ignored", 1)
	disabled.SetGauge("ignored", 1)
	disabled.AddGauge("ignored", 1)
	disabled.Gauge("ignored")

	snapshot := disabled.Snapshot()
	if snapshot.SchemaVersion != FileScanSnapshotSchemaVersion {
		t.Fatalf("schema version = %d", snapshot.SchemaVersion)
	}
	if snapshot.Digests.Targets.Count != 0 || snapshot.Digests.Fragments.Count != 0 ||
		snapshot.Digests.Findings.Count != 0 || snapshot.Digests.Ledger.Count != 0 {
		t.Fatalf("nil metrics recorded data: %+v", snapshot.Digests)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	for _, field := range []string{
		"schema_version", "phases", "backends", "resources", "fallback_reasons", "ledger_kinds", "digests",
	} {
		if _, ok := decoded[field]; !ok {
			t.Fatalf("snapshot JSON omitted %q: %s", field, encoded)
		}
	}
}

type fileScanFailingReader struct{}

func (fileScanFailingReader) Read([]byte) (int, error) {
	return 0, errors.New("injected read failure")
}

func TestFileScanMetricsRecordsErrorOnlyFragment(t *testing.T) {
	root := t.TempDir()
	metrics := NewFileScanMetrics()
	local := metrics.NewLocal()
	source := File{
		Content:         fileScanFailingReader{},
		Path:            filepath.Join(root, "failed.txt"),
		fileScanRoot:    root,
		fileScanMetrics: metrics,
		fileScanLocal:   local,
	}
	yielded := 0
	if err := source.Fragments(t.Context(), func(fragment Fragment, err error) error {
		yielded++
		if err == nil || fragment.Attr(AttrPath) != source.Path {
			t.Fatalf("error-only fragment = %+v, err=%v", fragment, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("fragments: %v", err)
	}
	metrics.MergeLocal(local)
	snapshot := metrics.Snapshot()
	if yielded != 1 || snapshot.Digests.Fragments.Count != 1 ||
		snapshot.Digests.Fragments.CanonicalBytes == 0 {
		t.Fatalf("yielded=%d fragment identity=%+v", yielded, snapshot.Digests.Fragments)
	}
}

func TestJoinFileScanTaskErrorsIsOrderIndependent(t *testing.T) {
	errA := errors.New("error-a")
	errB := errors.New("error-b")
	forward := joinFileScanTaskErrors([]fileScanTaskError{
		{path: "b", err: errB},
		{path: "a", err: errA},
	})
	reverse := joinFileScanTaskErrors([]fileScanTaskError{
		{path: "a", err: errA},
		{path: "b", err: errB},
	})
	if forward.Error() != reverse.Error() || !errors.Is(forward, errA) || !errors.Is(forward, errB) {
		t.Fatalf("unstable joined errors: forward=%q reverse=%q", forward, reverse)
	}
}
