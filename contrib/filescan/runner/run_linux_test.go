//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/betterleaks/betterleaks/contrib/filescan/internal/corpus"
	"github.com/betterleaks/betterleaks/contrib/filescan/internal/protocol"
)

func TestExecutePlanEndToEnd(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "corpus")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("tiny corpus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(workspace, "manifest", "corpus.json")
	if _, err := corpus.Create(context.Background(), corpus.CreateOptions{
		Root: root, Output: manifestPath, ID: "tiny-e2e", WorkloadClass: "tiny",
		Role: "confirmation", Source: corpus.Source{Origin: "test-fixture", Revision: "v1"},
	}); err != nil {
		t.Fatal(err)
	}

	identity := protocol.OutputIdentity{
		Count: 1, CanonicalBytes: 12,
		Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	oracleMetrics := protocol.ScannerMetrics{
		SchemaVersion: protocol.SchemaVersion, Candidate: confirmationOracleCandidate,
		Targets: identity, Fragments: identity, Findings: identity,
		Errors: identity, Fallbacks: identity,
	}
	if _, err := corpus.SetOracle(context.Background(), manifestPath, oracleMetrics); err != nil {
		t.Fatal(err)
	}
	metrics := oracleMetrics
	metrics.Candidate = "fake/parallel"
	metricsJSON, err := json.Marshal(metrics)
	if err != nil {
		t.Fatal(err)
	}
	scannerPath := filepath.Join(workspace, "fake-scanner")
	script := fmt.Sprintf("#!/bin/sh\n/bin/sleep 0.08\nprintf '%%s\\n' '%s' >&3\n", metricsJSON)
	if err := os.WriteFile(scannerPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	workDirectory := filepath.Join(workspace, "work")
	if err := os.Mkdir(workDirectory, 0o755); err != nil {
		t.Fatal(err)
	}

	loaded := validTestPlan(workDirectory)
	loaded.Phase = "confirmation"
	loaded.Analysis.MinimumConfirmationBlocks = 2
	loaded.Binary = scannerPath
	loaded.RequireScannerMetrics = true
	for i := range loaded.Candidates {
		loaded.Candidates[i].MetricsCandidate = "fake/parallel"
		loaded.Candidates[i].Args = []string{"--filescan-backend=baseline"}
	}
	loaded.MaxInfrastructureReplacements = 0
	loaded.Environment = map[string]string{"HOME": filepath.Join(workspace, "home"), "LC_ALL": "C"}
	loaded.Strata = []stratum{{
		ID: "tiny", WorkloadClass: "tiny", EvidenceRole: "performance", CorpusManifest: manifestPath,
		Root: root, CacheState: "warm-test", ScannerArgs: []string{"ignored"},
	}}
	loaded.BinarySHA256, err = corpus.FileSHA256(scannerPath)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Artifacts = []artifact{}
	loaded.Strata[0].ManifestSHA256, err = corpus.FileSHA256(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	schedule, err := buildSchedule(loaded.Candidates, loaded.FixedHorizonBlocks, loaded.Seed, loaded.Strata[0].ID, loaded.Phase)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Strata[0].ScheduleSHA256 = scheduleSHA256(schedule)
	if err := loaded.validate(); err != nil {
		t.Fatalf("validate test plan: %v", err)
	}
	mismatched := loaded
	mismatched.Strata = append([]stratum(nil), loaded.Strata...)
	mismatched.Strata[0].ManifestSHA256 = strings.Repeat("f", 64)
	if _, err := loadStrata(mismatched); err == nil {
		t.Fatal("manifest digest mismatch was accepted")
	}
	resultsPath := filepath.Join(workspace, "results", "raw.jsonl")
	summary, err := executePlan(context.Background(), loaded, sha256Hex([]byte("test-plan")), resultsPath)
	if err != nil {
		t.Fatalf("execute plan: %v", err)
	}
	if summary.Rows != 4 || summary.InfrastructureReplacementBlocks != 0 || len(summary.ResultsSHA256) != 64 {
		t.Fatalf("unexpected summary: %#v", summary)
	}

	file, err := os.Open(resultsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	positions := map[string]map[int]int{
		"baseline":  {},
		"candidate": {},
	}
	rows := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var row runResult
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			t.Fatalf("decode result row: %v", err)
		}
		rows++
		if row.Validity != validityValid || row.BlockValidity != validityValid || row.Exit.Code != 0 {
			t.Fatalf("invalid row: %#v", row)
		}
		if row.ScannerMetrics == nil || row.ScannerMetrics.Targets != identity {
			t.Fatalf("missing scanner metrics: %#v", row.ScannerMetrics)
		}
		if row.Procfs.Samples == 0 || row.Command[0] != scannerPath || row.Environment[metricsFDEnvironment] != "3" {
			t.Fatalf("missing provenance or procfs sample: %#v", row)
		}
		if row.BinarySHA256 != loaded.BinarySHA256 || row.ManifestSHA256 != loaded.Strata[0].ManifestSHA256 || len(row.Artifacts) != 0 {
			t.Fatalf("incorrect immutable input provenance: %#v", row)
		}
		if row.Host.Scope != hostTelemetryScope || row.Host.SamplingRounds < 2 ||
			!row.Host.VMStat.Status.DeltaAvailable || !row.Host.MemInfo.Status.DeltaAvailable {
			t.Fatalf("missing host telemetry: %#v", row.Host)
		}
		if len(row.AllowedExitCodes) != 1 || row.AllowedExitCodes[0] != 0 {
			t.Fatalf("default allowed exit contract was not recorded: %#v", row.AllowedExitCodes)
		}
		positions[row.CandidateID][row.SequencePosition]++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if rows != 4 {
		t.Fatalf("read %d rows, want 4", rows)
	}
	for candidate, counts := range positions {
		if counts[0] != 1 || counts[1] != 1 {
			t.Fatalf("candidate %s position counts: %#v", candidate, counts)
		}
	}
}

func TestConfirmationManifestRequiresBaselineParallelOracle(t *testing.T) {
	identity := protocol.OutputIdentity{
		Count: 1, CanonicalBytes: 1,
		Digest: strings.Repeat("a", 64),
	}

	for _, test := range []struct {
		name      string
		candidate string
		wantError bool
	}{
		{name: "baseline oracle", candidate: confirmationOracleCandidate},
		{name: "challenger oracle", candidate: "buffered/parallel", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			root := filepath.Join(workspace, "corpus")
			if err := os.Mkdir(root, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(workspace, "manifest.json")
			if _, err := corpus.Create(context.Background(), corpus.CreateOptions{
				Root: root, Output: manifestPath, ID: "oracle-provenance", WorkloadClass: "test",
				Role: "confirmation", Source: corpus.Source{Origin: "test", Revision: "v1"},
			}); err != nil {
				t.Fatal(err)
			}
			metrics := protocol.ScannerMetrics{
				SchemaVersion: protocol.SchemaVersion, Candidate: test.candidate,
				Targets: identity, Fragments: identity, Findings: identity,
				Errors: identity, Fallbacks: identity,
			}
			if _, err := corpus.SetOracle(context.Background(), manifestPath, metrics); err != nil {
				t.Fatal(err)
			}

			loaded := validTestPlan(workspace)
			loaded.Phase = "confirmation"
			loaded.RequireScannerMetrics = true
			loaded.Strata = []stratum{{
				ID: "oracle-provenance", WorkloadClass: "test", EvidenceRole: "performance",
				CorpusManifest: manifestPath, Root: root, CacheState: "verified",
				ScannerArgs: []string{"ignored"},
			}}
			var err error
			loaded.Strata[0].ManifestSHA256, err = corpus.FileSHA256(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			_, err = loadStrata(loaded)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "oracle candidate") {
					t.Fatalf("challenger-frozen oracle error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("baseline-frozen oracle was rejected: %v", err)
			}
		})
	}
}

func TestLiveGuardRunsBeforeCacheConditioningHook(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "corpus")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("stable\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(workspace, "manifest.json")
	if _, err := corpus.Create(context.Background(), corpus.CreateOptions{
		Root: root, Output: manifestPath, ID: "hook-order", WorkloadClass: "test",
		Role: "calibration", Source: corpus.Source{Origin: "test", Revision: "v1"},
	}); err != nil {
		t.Fatal(err)
	}
	frozen, err := corpus.Load(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	// An invalid corpus must stop the run before cache conditioning.
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(workspace, "conditioning-ran")

	loadedPlan := validTestPlan(workspace)
	loadedPlan.Strata[0] = stratum{
		ID: "hook-order", WorkloadClass: "test", EvidenceRole: "performance",
		CorpusManifest: manifestPath, Root: root, CacheState: "conditioned",
		ScannerArgs: []string{"ignored"},
		PrepareHooks: []hook{{
			ID: "condition-cache", Command: []string{"/usr/bin/touch", marker}, TimeoutSeconds: 5,
		}},
	}
	loaded := loadedStratum{
		plan: loadedPlan.Strata[0], manifest: frozen, manifestSHA256: "manifest-digest",
	}
	rows, infrastructureInvalid := runBlock(
		context.Background(), loadedPlan, "plan-digest", "binary-digest", loaded,
		scheduledBlock{Index: 0, SequenceID: 0, Candidates: []string{"baseline"}},
		0, map[string]candidate{"baseline": {ID: "baseline"}},
	)
	if !infrastructureInvalid || len(rows) != 1 || rows[0].Validity != validityInfrastructure {
		t.Fatalf("invalid corpus was not rejected: infrastructure=%t rows=%#v", infrastructureInvalid, rows)
	}
	if len(rows[0].Hooks) != 0 {
		t.Fatalf("conditioning ran before the live guard: %#v", rows[0].Hooks)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("conditioning marker exists or stat failed: %v", err)
	}
}

func TestImmediateCandidateFailureIsNotReplaceable(t *testing.T) {
	workspace := t.TempDir()
	loadedPlan := validTestPlan(workspace)
	loadedPlan.Binary = "/bin/sh"
	loadedPlan.CommonArgs = []string{"-c", "exit 7"}
	loadedPlan.SampleIntervalMS = 5000
	loadedPlan.RequireScannerMetrics = true
	fastStratum := stratum{
		ID: "fast-exit", WorkloadClass: "test", EvidenceRole: "correctness",
		CorpusManifest: filepath.Join(workspace, "unused-manifest.json"),
		Root:           workspace, CacheState: "uncontrolled", ScannerArgs: []string{"ignored"},
		SkipLiveGuard: true,
	}
	loaded := loadedStratum{
		plan: fastStratum, manifest: &corpus.Manifest{}, manifestSHA256: "manifest-digest",
	}
	rows, infrastructureInvalid := runBlock(
		context.Background(), loadedPlan, "plan-digest", "binary-digest", loaded,
		scheduledBlock{Index: 0, SequenceID: 0, Candidates: []string{"baseline"}},
		0, map[string]candidate{"baseline": {ID: "baseline", MetricsCandidate: "fake/parallel"}},
	)
	if infrastructureInvalid || len(rows) != 1 {
		t.Fatalf("candidate failure was replaceable: infrastructure=%t rows=%#v", infrastructureInvalid, rows)
	}
	row := rows[0]
	if row.Validity != validityCandidate {
		t.Fatalf("candidate failure validity = %q, want %q: %#v", row.Validity, validityCandidate, row.ValidityLedger)
	}
	if row.Exit.Code != 7 || row.Rusage.Scope != "wait4_root_process_including_kernel_accounted_waited_children" {
		t.Fatalf("fast process exit or wait4 usage missing: exit=%#v rusage=%#v", row.Exit, row.Rusage)
	}
	codes := make(map[string]protocol.LedgerEntry)
	for _, entry := range row.ValidityLedger {
		codes[entry.Code] = entry
		if entry.Severity == "error" && entry.Owner == "infrastructure" {
			t.Fatalf("candidate failure gained an infrastructure error: %#v", row.ValidityLedger)
		}
	}
	for _, code := range []string{"unexpected_exit_code", "scanner_metrics_missing"} {
		if entry, exists := codes[code]; !exists || entry.Owner != "candidate" || entry.Severity != "error" {
			t.Fatalf("missing candidate-owned %s: %#v", code, row.ValidityLedger)
		}
	}
}

func TestRetainedMetricsDescriptorIsCandidateOwnedAndKilled(t *testing.T) {
	workspace := t.TempDir()
	pidFile := filepath.Join(workspace, "descendant.pid")
	script := fmt.Sprintf("sleep 60 </dev/null >/dev/null 2>/dev/null & echo $! > %q", pidFile)
	loadedPlan := validTestPlan(workspace)
	loadedPlan.Binary = "/bin/sh"
	loadedPlan.TimeoutSeconds = 5
	loadedPlan.RequireScannerMetrics = true
	declared := candidate{ID: "candidate", MetricsCandidate: "fake/parallel"}
	loaded := loadedStratum{
		plan:     stratum{Root: workspace},
		manifest: &corpus.Manifest{},
	}
	result := runResult{
		Command:          []string{"/bin/sh", "-c", script},
		CandidateID:      declared.ID,
		MetricsCandidate: declared.MetricsCandidate,
		AllowedExitCodes: []int{0},
		Exit:             exitResult{Code: -1},
	}
	runScanner(context.Background(), loadedPlan, declared, loaded, &result)

	var retained protocol.LedgerEntry
	for _, entry := range result.ValidityLedger {
		if entry.Code == "metrics_fd_close_timeout" {
			retained = entry
		}
	}
	if retained.Owner != "candidate" || retained.Severity != "error" {
		t.Fatalf("retained metrics descriptor = %#v, ledger=%#v", retained, result.ValidityLedger)
	}

	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("candidate descendant %d survived process-group cleanup: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRecordProcfsValidity(t *testing.T) {
	t.Run("short process keeps wait4 result valid", func(t *testing.T) {
		result := runResult{
			Procfs: procResourceSummary{},
			Rusage: waitResourceUsage{
				Scope: "wait4_root_process_including_kernel_accounted_waited_children",
			},
		}
		recordProcfsValidity(&result)
		if got := classifyValidity(result.ValidityLedger); got != validityValid {
			t.Fatalf("validity = %q, want %q: %#v", got, validityValid, result.ValidityLedger)
		}
		if len(result.ValidityLedger) != 1 {
			t.Fatalf("ledger = %#v, want one observation", result.ValidityLedger)
		}
		entry := result.ValidityLedger[0]
		if entry.Code != "procfs_no_samples" || entry.Severity != "warning" || entry.Owner != "harness" {
			t.Fatalf("short-process observation = %#v", entry)
		}
	})

	t.Run("sampler failure stays infrastructure owned", func(t *testing.T) {
		result := runResult{Procfs: procResourceSummary{SampleErrors: 2}}
		recordProcfsValidity(&result)
		if got := classifyValidity(result.ValidityLedger); got != validityInfrastructure {
			t.Fatalf("validity = %q, want %q: %#v", got, validityInfrastructure, result.ValidityLedger)
		}
		if len(result.ValidityLedger) != 1 {
			t.Fatalf("ledger = %#v, want one failure", result.ValidityLedger)
		}
		entry := result.ValidityLedger[0]
		if entry.Code != "procfs_sampling_failed" || entry.Severity != "error" || entry.Owner != "infrastructure" {
			t.Fatalf("sampler failure = %#v", entry)
		}
	})

	t.Run("observed process needs no ledger entry", func(t *testing.T) {
		result := runResult{Procfs: procResourceSummary{Samples: 1, SampleErrors: 1}}
		recordProcfsValidity(&result)
		if len(result.ValidityLedger) != 0 {
			t.Fatalf("observed process ledger = %#v, want none", result.ValidityLedger)
		}
	})
}

func TestScannerMetricsCandidateMustMatchDeclaredSignature(t *testing.T) {
	identity := protocol.OutputIdentity{
		Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	metrics := protocol.ScannerMetrics{
		SchemaVersion: protocol.SchemaVersion, Candidate: "buffered/parallel",
		Targets: identity, Fragments: identity, Findings: identity,
		Errors: identity, Fallbacks: identity,
	}
	data, err := json.Marshal(metrics)
	if err != nil {
		t.Fatal(err)
	}
	result := runResult{CandidateID: "candidate", MetricsCandidate: "baseline/parallel"}
	manifest := &corpus.Manifest{ExpectedOutputs: metrics.Outputs()}
	consumeScannerMetrics(plan{RequireScannerMetrics: true}, manifest, metricsRead{data: data}, &result)
	if len(result.ValidityLedger) != 1 || result.ValidityLedger[0].Code != "scanner_candidate_mismatch" {
		t.Fatalf("candidate signature mismatch was not rejected: %#v", result.ValidityLedger)
	}
}

func TestAllowedNonzeroExitIsInformational(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 7")
	waitErr := cmd.Run()
	result := runResult{AllowedExitCodes: []int{0, 7}}
	result.Exit = processExit(cmd, context.Background(), waitErr)
	recordExitValidity(waitErr, &result)
	if len(result.ValidityLedger) != 1 || result.ValidityLedger[0].Code != "allowed_nonzero_exit" ||
		result.ValidityLedger[0].Severity != "info" {
		t.Fatalf("allowed correctness exit was not informational: %#v", result.ValidityLedger)
	}
}

func TestSignalCannotBeAllowed(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "kill -TERM $$")
	waitErr := cmd.Run()
	result := runResult{AllowedExitCodes: []int{0, 143}}
	result.Exit = processExit(cmd, context.Background(), waitErr)
	recordExitValidity(waitErr, &result)
	if len(result.ValidityLedger) != 1 || result.ValidityLedger[0].Code != "process_signaled" ||
		result.ValidityLedger[0].Severity != "error" {
		t.Fatalf("signal was relaxed by exit contract: %#v", result.ValidityLedger)
	}
}
