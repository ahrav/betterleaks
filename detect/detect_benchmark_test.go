package detect

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/betterleaks/betterleaks/config"
	"github.com/betterleaks/betterleaks/sources"
)

func BenchmarkDetectFragmentSemiGenericKeywordNoOperator(b *testing.B) {
	cfg, err := config.Default()
	if err != nil {
		b.Fatal(err)
	}

	benchmarkDetectFragmentGateModes(b, cfg, benchmarkKeywordMissFragment(cfg), 0)
}

func BenchmarkDetectFragmentSemiGenericKeywordPunctuationMiss(b *testing.B) {
	cfg, err := config.Default()
	if err != nil {
		b.Fatal(err)
	}

	benchmarkDetectFragmentGateModes(b, cfg, benchmarkKeywordPunctuationMissFragment(cfg), 0)
}

func BenchmarkDetectFragmentSemiGenericPositive(b *testing.B) {
	cfg, err := config.Default()
	if err != nil {
		b.Fatal(err)
	}

	benchmarkDetectFragmentGateModes(b, cfg, benchmarkPositiveFragment(), 1)
}

func BenchmarkDetectFragmentSemiGenericColdNoOperator(b *testing.B) {
	cfg, err := config.Default()
	if err != nil {
		b.Fatal(err)
	}
	fragment := benchmarkKeywordMissFragment(cfg)
	b.SetBytes(int64(len(fragment.Raw)))

	for _, tt := range []struct {
		name                      string
		operatorByteGate          bool
		requiredAnyCacheDisabled  bool
		mandatoryAtomGateDisabled bool
	}{
		{name: "operator_byte_gate", operatorByteGate: true},
		{name: "operator_byte_gate_no_atom", operatorByteGate: true, mandatoryAtomGateDisabled: true},
		{name: "operator_byte_gate_per_rule_scan", operatorByteGate: true, requiredAnyCacheDisabled: true},
		{name: "prefix_regex_gate_only", operatorByteGate: false, mandatoryAtomGateDisabled: true},
	} {
		b.Run(tt.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(fragment.Raw)))
			for b.Loop() {
				d := newBenchmarkDetector(cfg, tt.operatorByteGate, tt.requiredAnyCacheDisabled, tt.mandatoryAtomGateDisabled)
				if findings := d.detectFragment(context.Background(), fragment); len(findings) != 0 {
					b.Fatalf("detected %d findings, want 0", len(findings))
				}
			}
		})
	}
}

func benchmarkDetectFragmentGateModes(b *testing.B, cfg *config.Config, fragment sources.Fragment, wantFindings int) {
	b.SetBytes(int64(len(fragment.Raw)))

	for _, tt := range []struct {
		name                      string
		operatorByteGate          bool
		requiredAnyCacheDisabled  bool
		mandatoryAtomGateDisabled bool
	}{
		{name: "operator_byte_gate", operatorByteGate: true},
		{name: "operator_byte_gate_no_atom", operatorByteGate: true, mandatoryAtomGateDisabled: true},
		{name: "operator_byte_gate_per_rule_scan", operatorByteGate: true, requiredAnyCacheDisabled: true},
		{name: "prefix_regex_gate_only", operatorByteGate: false, mandatoryAtomGateDisabled: true},
	} {
		b.Run(tt.name, func(b *testing.B) {
			d := newBenchmarkDetector(cfg, tt.operatorByteGate, tt.requiredAnyCacheDisabled, tt.mandatoryAtomGateDisabled)
			if findings := d.detectFragment(context.Background(), fragment); len(findings) != wantFindings {
				b.Fatalf("warmup produced %d findings, want %d", len(findings), wantFindings)
			}

			b.ReportAllocs()
			b.SetBytes(int64(len(fragment.Raw)))
			b.ResetTimer()
			for b.Loop() {
				if findings := d.detectFragment(context.Background(), fragment); len(findings) != wantFindings {
					b.Fatalf("detected %d findings, want %d", len(findings), wantFindings)
				}
			}
		})
	}
}

func newBenchmarkDetector(cfg *config.Config, operatorByteGate bool, requiredAnyCacheDisabled bool, mandatoryAtomGateDisabled bool) *Detector {
	d := NewDetector(cfg)
	d.MaxDecodeDepth = 0
	d.disableRuleGateRequiredAnyCache = requiredAnyCacheDisabled
	if mandatoryAtomGateDisabled {
		d.mandatoryAtomGatesByRank = nil
	}
	if !operatorByteGate {
		for ruleID, gate := range d.ruleGates {
			gate.requiredAny = ""
			d.ruleGates[ruleID] = gate
		}
	}
	return d
}

func benchmarkKeywordMissFragment(cfg *config.Config) sources.Fragment {
	keywords := benchmarkKeywords(cfg)

	var raw strings.Builder
	for range 8 {
		for _, keyword := range keywords {
			raw.WriteString("local ")
			raw.WriteString(keyword)
			raw.WriteString(" token placeholder text without assignment\n")
		}
	}

	return sources.Fragment{
		Raw:        raw.String(),
		Attributes: map[string]string{},
	}
}

func benchmarkKeywordPunctuationMissFragment(cfg *config.Config) sources.Fragment {
	keywords := benchmarkKeywords(cfg)

	var raw strings.Builder
	for range 8 {
		for _, keyword := range keywords {
			raw.WriteString("local ")
			raw.WriteString(keyword)
			raw.WriteString(" = \"placeholder text without enough secret structure\"\n")
		}
	}

	return sources.Fragment{
		Raw:        raw.String(),
		Attributes: map[string]string{},
	}
}

func benchmarkKeywords(cfg *config.Config) []string {
	keywords := make([]string, 0, len(cfg.Keywords))
	for keyword := range cfg.Keywords {
		if strings.ContainsAny(keyword, semiGenericOperatorChars) {
			continue
		}
		keywords = append(keywords, keyword)
	}
	sort.Strings(keywords)
	return keywords
}

func benchmarkPositiveFragment() sources.Fragment {
	return sources.Fragment{
		Raw:        `const Discord_Public_Key = "e7322523fb86ed64c836a979cf8465fbd436378c653c1db38f9ae87bc62a6fd5"`,
		Attributes: map[string]string{sources.AttrPath: "tmp.py"},
	}
}
