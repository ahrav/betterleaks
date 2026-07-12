//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validTestPlan(workspace string) plan {
	return plan{
		SchemaVersion:                 planSchemaVersion,
		StudyID:                       "filescan-test-v1",
		Phase:                         "calibration",
		Seed:                          17,
		Binary:                        "/bin/true",
		WorkingDirectory:              workspace,
		Environment:                   map[string]string{"HOME": filepath.Join(workspace, "home")},
		FixedHorizonBlocks:            2,
		SampleIntervalMS:              10,
		TimeoutSeconds:                2,
		MaxInfrastructureReplacements: 1,
		BaselineCandidate:             "baseline",
		Candidates: []candidate{
			{ID: "baseline"},
			{ID: "candidate"},
		},
		Strata: []stratum{{
			ID: "tiny", WorkloadClass: "test", EvidenceRole: "performance", CorpusManifest: "/tmp/corpus.json",
			Root: "/tmp/corpus", CacheState: "warm", ScannerArgs: []string{"dir", "/tmp/corpus"},
		}},
		Analysis: analysisConfig{
			MinimumWorthwhileEffectPct: 5, AlphaFamilywise: 0.05, Power: 0.9,
			WinnerClaims: 4, VarianceInflation: 1.5, MinimumConfirmationBlocks: 12,
			EquivalenceMarginPct: 2, CPUMaterialImprovementPct: 10,
			RSSMaterialImprovementPct: 20, PageCacheMaterialImprovementPct: 20,
			BootstrapResamples: 1000,
		},
	}
}

func addTestProvenancePins(t *testing.T, loaded *plan) {
	t.Helper()
	loaded.BinarySHA256 = strings.Repeat("a", 64)
	loaded.Artifacts = []artifact{}
	for index := range loaded.Strata {
		loaded.Strata[index].ManifestSHA256 = strings.Repeat("b", 64)
		schedule, err := buildSchedule(
			loaded.Candidates, loaded.FixedHorizonBlocks, loaded.Seed,
			loaded.Strata[index].ID, loaded.Phase,
		)
		if err != nil {
			t.Fatalf("build pinned test schedule: %v", err)
		}
		loaded.Strata[index].ScheduleSHA256 = scheduleSHA256(schedule)
	}
}

func TestLoadPlanStrict(t *testing.T) {
	workspace := t.TempDir()
	loaded := validTestPlan(workspace)
	data, err := json.Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace, "plan.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, digest, err := loadPlan(path)
	if err != nil {
		t.Fatalf("load valid plan: %v", err)
	}
	if got.StudyID != loaded.StudyID || len(digest) != 64 {
		t.Fatalf("unexpected loaded plan or digest: %#v %q", got, digest)
	}

	unknown := strings.TrimSuffix(string(data), "}") + `,"unexpected":true}`
	if err := os.WriteFile(path, []byte(unknown), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadPlan(path); err == nil {
		t.Fatal("unknown plan field was accepted")
	}
	if err := os.WriteFile(path, append(data, []byte(" {}")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadPlan(path); err == nil {
		t.Fatal("trailing JSON value was accepted")
	}
}

func TestHostLocalExamplePlanValid(t *testing.T) {
	loaded, digest, err := loadPlan(filepath.Join("..", "examples", "host-local-plan.json"))
	if err != nil {
		t.Fatalf("load host-local example: %v", err)
	}
	if loaded.FixedHorizonBlocks != 2 || loaded.Phase != "calibration" || len(digest) != 64 {
		t.Fatalf("unexpected host-local example: %#v", loaded)
	}
}

func TestPlanValidationRejectsConfounds(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*plan)
	}{
		{"partial-cycle", func(plan *plan) {
			plan.Phase = "confirmation"
			plan.Analysis.MinimumConfirmationBlocks = 2
			plan.FixedHorizonBlocks = 3
		}},
		{"missing-baseline", func(plan *plan) { plan.BaselineCandidate = "missing" }},
		{"relative-binary", func(plan *plan) { plan.Binary = "betterleaks" }},
		{"reserved-environment", func(plan *plan) { plan.Environment[metricsFDEnvironment] = "9" }},
		{"nul-environment", func(plan *plan) { plan.Environment["BAD"] = "a\x00b" }},
		{"duplicate-candidate", func(plan *plan) { plan.Candidates[1].ID = "baseline" }},
		{"common-candidate-option", func(plan *plan) {
			plan.CommonArgs = []string{"--filescan-backend=buffered"}
		}},
		{"stratum-candidate-option", func(plan *plan) {
			plan.Strata[0].ScannerArgs = append(plan.Strata[0].ScannerArgs, "--filescan-active-files=1")
		}},
		{"duplicate-candidate-option", func(plan *plan) {
			plan.Candidates[0].Args = []string{"--filescan-walkers=8", "--filescan-walkers=16"}
		}},
		{"candidate-workload-option", func(plan *plan) {
			plan.Candidates[0].Args = []string{"--max-archive-depth=0"}
		}},
		{"unknown-candidate-option", func(plan *plan) {
			plan.Candidates[0].Args = []string{"--filescan-invented=true"}
		}},
		{"noncanonical-candidate-option", func(plan *plan) {
			plan.Candidates[0].Args = []string{"--filescan-backend", "baseline"}
		}},
		{"low-bootstrap", func(plan *plan) { plan.Analysis.BootstrapResamples = 999 }},
		{"invalid-evidence-role", func(plan *plan) { plan.Strata[0].EvidenceRole = "timing" }},
		{"performance-nonzero-exit", func(plan *plan) { plan.Strata[0].AllowedExitCodes = []int{0, 7} }},
		{"empty-exit-set", func(plan *plan) { plan.Strata[0].AllowedExitCodes = []int{} }},
		{"duplicate-exit-code", func(plan *plan) {
			plan.Strata[0].EvidenceRole = "correctness"
			plan.Strata[0].AllowedExitCodes = []int{7, 7}
		}},
		{"exit-code-out-of-range", func(plan *plan) {
			plan.Strata[0].EvidenceRole = "correctness"
			plan.Strata[0].AllowedExitCodes = []int{256}
		}},
		{"short-pilot", func(plan *plan) { plan.Phase = "pilot" }},
		{"short-confirmation", func(plan *plan) { plan.Phase = "confirmation" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			loaded := validTestPlan(t.TempDir())
			test.mutate(&loaded)
			if err := loaded.validate(); err == nil {
				t.Fatal("invalid plan was accepted")
			}
		})
	}
}

func TestCorrectnessPlanMayPredeclareNonzeroExit(t *testing.T) {
	loaded := validTestPlan(t.TempDir())
	loaded.Strata[0].EvidenceRole = "correctness"
	loaded.Strata[0].AllowedExitCodes = []int{0, 7}
	if err := loaded.validate(); err != nil {
		t.Fatalf("correctness-only allowed exit was rejected: %v", err)
	}
}

func TestConfirmationRequiresCorrectnessAndLiveGuards(t *testing.T) {
	validConfirmation := func() plan {
		loaded := validTestPlan(t.TempDir())
		loaded.Phase = "confirmation"
		loaded.FixedHorizonBlocks = 12
		loaded.RequireScannerMetrics = true
		loaded.Candidates[0].MetricsCandidate = "baseline/parallel"
		loaded.Candidates[1].MetricsCandidate = "buffered/parallel"
		loaded.Candidates[0].Args = []string{"--filescan-backend=baseline"}
		loaded.Candidates[1].Args = []string{"--filescan-backend=buffered"}
		addTestProvenancePins(t, &loaded)
		return loaded
	}

	loaded := validConfirmation()
	if err := loaded.validate(); err != nil {
		t.Fatalf("guarded confirmation was rejected: %v", err)
	}
	loaded = validConfirmation()
	loaded.RequireScannerMetrics = false
	if err := loaded.validate(); err == nil {
		t.Fatal("confirmation without scanner correctness metrics was accepted")
	}
	loaded = validConfirmation()
	loaded.Strata[0].SkipLiveGuard = true
	if err := loaded.validate(); err == nil {
		t.Fatal("confirmation with a disabled live guard was accepted")
	}
	loaded = validConfirmation()
	loaded.Candidates[1].MetricsCandidate = ""
	if err := loaded.validate(); err == nil {
		t.Fatal("confirmation with an unbound candidate signature was accepted")
	}
}

func TestPilotRequiresCompleteProvenancePins(t *testing.T) {
	validPilot := func() plan {
		loaded := validTestPlan(t.TempDir())
		loaded.Phase = "pilot"
		loaded.FixedHorizonBlocks = 6
		addTestProvenancePins(t, &loaded)
		return loaded
	}

	loaded := validPilot()
	if err := loaded.validate(); err != nil {
		t.Fatalf("fully pinned pilot was rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*plan)
	}{
		{"missing-binary", func(plan *plan) { plan.BinarySHA256 = "" }},
		{"missing-artifact-array", func(plan *plan) { plan.Artifacts = nil }},
		{"missing-manifest", func(plan *plan) { plan.Strata[0].ManifestSHA256 = "" }},
		{"missing-schedule", func(plan *plan) { plan.Strata[0].ScheduleSHA256 = "" }},
		{"wrong-schedule", func(plan *plan) { plan.Strata[0].ScheduleSHA256 = strings.Repeat("c", 64) }},
		{"inherited-environment", func(plan *plan) { plan.InheritEnvironment = true }},
		{"duplicate-artifact", func(plan *plan) {
			plan.Artifacts = []artifact{
				{Path: "/tmp/config", SHA256: strings.Repeat("c", 64)},
				{Path: "/tmp/config", SHA256: strings.Repeat("d", 64)},
			}
		}},
		{"unpinned-helper", func(plan *plan) {
			plan.PreRunHooks = []hook{{ID: "prepare", Command: []string{"/bin/true"}, TimeoutSeconds: 1}}
		}},
		{"unpinned-long-config", func(plan *plan) { plan.CommonArgs = []string{"--config=/tmp/config.toml"} }},
		{"unpinned-short-config", func(plan *plan) { plan.CommonArgs = []string{"-c", "/tmp/config.toml"} }},
		{"unpinned-env-config", func(plan *plan) { plan.Environment["BETTERLEAKS_CONFIG"] = "/tmp/config.toml" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			loaded := validPilot()
			test.mutate(&loaded)
			if err := loaded.validate(); err == nil {
				t.Fatal("incompletely pinned pilot was accepted")
			}
		})
	}

	loaded = validPilot()
	loaded.PreRunHooks = []hook{{ID: "prepare", Command: []string{"/bin/true"}, TimeoutSeconds: 1}}
	loaded.CommonArgs = []string{"-c=/tmp/config.toml"}
	loaded.Artifacts = []artifact{
		{Path: "/bin/true", SHA256: strings.Repeat("c", 64)},
		{Path: "/tmp/config.toml", SHA256: strings.Repeat("d", 64)},
	}
	if err := loaded.validate(); err != nil {
		t.Fatalf("pinned helper and config were rejected: %v", err)
	}
}

func TestScheduleDigestsForDraft(t *testing.T) {
	draft := validTestPlan(t.TempDir())
	draft.Phase = "pilot"
	draft.FixedHorizonBlocks = 6
	draft.BinarySHA256 = strings.Repeat("a", 64)
	draft.Artifacts = []artifact{}
	draft.Strata[0].ManifestSHA256 = strings.Repeat("b", 64)

	digests, err := scheduleDigestsForDraft(draft)
	if err != nil {
		t.Fatalf("derive draft schedule: %v", err)
	}
	schedule, err := buildSchedule(draft.Candidates, draft.FixedHorizonBlocks, draft.Seed, draft.Strata[0].ID, draft.Phase)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := digests[draft.Strata[0].ID], scheduleSHA256(schedule); got != want {
		t.Fatalf("schedule digest %q, want %q", got, want)
	}
}

func TestCalibrationPlanAllowsTruncatedCycle(t *testing.T) {
	loaded := validTestPlan(t.TempDir())
	loaded.Candidates = append(loaded.Candidates, candidate{ID: "third"})
	loaded.FixedHorizonBlocks = 1
	if err := loaded.validate(); err != nil {
		t.Fatalf("one-block calibration plan was rejected: %v", err)
	}
}
