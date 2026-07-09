package detect

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"testing"

	"github.com/fatih/semgroup"

	"github.com/betterleaks/betterleaks/config"
	"github.com/betterleaks/betterleaks/sources"
)

func TestRuleGateStatsForPath(t *testing.T) {
	path := os.Getenv("BETTERLEAKS_RULE_GATE_STATS_PATH")
	if path == "" {
		t.Skip("set BETTERLEAKS_RULE_GATE_STATS_PATH to measure gate stats for a file tree")
	}

	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	d := NewDetector(cfg)
	d.MaxDecodeDepth = 0
	d.SkipFindingAppend = true
	d.ruleGateStats = &ruleGateStats{}

	src := &sources.Files{
		Path: path,
		Sema: semgroup.NewGroup(ctx, int64(max(1, runtime.NumCPU()))),
	}

	findings := 0
	for result := range d.Run(ctx, src) {
		if result.Err != nil {
			t.Fatal(result.Err)
		}
		findings++
	}

	gatesInstalled := 0
	requiredAnyGates := 0
	for _, gate := range d.ruleGates {
		gatesInstalled++
		if gate.requiredAny != "" {
			requiredAnyGates++
		}
	}
	mandatoryAtomGates := 0
	for _, atoms := range d.mandatoryAtomGatesByRank {
		if len(atoms) > 0 {
			mandatoryAtomGates++
		}
	}

	stats := d.ruleGateStats.snapshot()
	gateRejects := stats.RequiredAnyRejects + stats.RegexGateRejects
	fmt.Printf("METRIC gates_installed=%d\n", gatesInstalled)
	fmt.Printf("METRIC required_any_gates=%d\n", requiredAnyGates)
	fmt.Printf("METRIC mandatory_atom_gates=%d\n", mandatoryAtomGates)
	fmt.Printf("METRIC fragments=%d\n", stats.Fragments)
	fmt.Printf("METRIC decode_passes=%d\n", stats.DecodePasses)
	fmt.Printf("METRIC decode_pass_bytes=%d\n", stats.DecodePassBytes)
	fmt.Printf("METRIC rule_checks=%d\n", stats.RuleChecks)
	fmt.Printf("METRIC gated_rule_checks=%d\n", stats.GatedRuleChecks)
	fmt.Printf("METRIC gate_rejects=%d\n", gateRejects)
	fmt.Printf("METRIC required_any_checks=%d\n", stats.RequiredAnyChecks)
	fmt.Printf("METRIC required_any_rejects=%d\n", stats.RequiredAnyRejects)
	fmt.Printf("METRIC required_any_bytes=%d\n", stats.RequiredAnyBytes)
	fmt.Printf("METRIC required_any_cache_hits=%d\n", stats.RequiredAnyCacheHits)
	fmt.Printf("METRIC required_any_cache_misses=%d\n", stats.RequiredAnyCacheMisses)
	fmt.Printf("METRIC regex_gate_checks=%d\n", stats.RegexGateChecks)
	fmt.Printf("METRIC regex_gate_rejects=%d\n", stats.RegexGateRejects)
	fmt.Printf("METRIC regex_gate_accepts=%d\n", stats.RegexGateAccepts)
	fmt.Printf("METRIC regex_gate_bytes=%d\n", stats.RegexGateBytes)
	fmt.Printf("METRIC mandatory_atom_checks=%d\n", stats.MandatoryAtomChecks)
	fmt.Printf("METRIC mandatory_atom_rejects=%d\n", stats.MandatoryAtomRejects)
	fmt.Printf("METRIC mandatory_atom_accepts=%d\n", stats.MandatoryAtomAccepts)
	fmt.Printf("METRIC mandatory_atom_bytes=%d\n", stats.MandatoryAtomBytes)
	fmt.Printf("METRIC gate_compile_fail_opens=%d\n", stats.GateCompileFailOpens)
	fmt.Printf("METRIC full_regex_calls=%d\n", stats.FullRegexCalls)
	fmt.Printf("METRIC full_regex_no_match_calls=%d\n", stats.FullRegexNoMatchCalls)
	fmt.Printf("METRIC full_regex_match_calls=%d\n", stats.FullRegexMatchCalls)
	fmt.Printf("METRIC full_regex_match_spans=%d\n", stats.FullRegexMatchSpans)
	fmt.Printf("METRIC full_regex_bytes=%d\n", stats.FullRegexBytes)
	fmt.Printf("METRIC filtered_findings=%d\n", stats.FilteredFindings)
	fmt.Printf("METRIC emitted_findings=%d\n", findings)
}
