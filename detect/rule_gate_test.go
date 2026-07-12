package detect

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/lucasjones/reggen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/betterleaks/betterleaks/config"
	blregexp "github.com/betterleaks/betterleaks/regexp"
	"github.com/betterleaks/betterleaks/report"
	"github.com/betterleaks/betterleaks/sources"
)

// TestGateForPattern covers the recognized prefix shapes, the shapes that
// must yield no gate, and shapes that superficially resemble the prefix.
func TestGateForPattern(t *testing.T) {
	tests := map[string]struct {
		pattern string
		want    string
	}{
		"case-insensitive prefix": {
			pattern: `(?i)[\w.-]{0,50}?(?:secret|token)(?:\s*=\s*)(\w{16})`,
			want:    `(?i)(?:secret|token)(?:\s*=\s*)(\w{16})`,
		},
		"grouped prefix": {
			pattern: `[\w.-]{0,50}?(?i:api[_-]?key)(?:.{0,20})(\w{32})`,
			want:    `(?i:api[_-]?key)(?:.{0,20})(\w{32})`,
		},
		"no prefix shape": {
			pattern: `AKIA[0-9A-Z]{16}`,
			want:    "",
		},
		"anchored pattern": {
			pattern: `^ghp_[0-9A-Za-z]{36}$`,
			want:    "",
		},
		"required prefix is not optional-prefix shape": {
			// {1,50} (required) is NOT the optional {0,50}? shape; stripping
			// it would be unsound, so no gate may be produced.
			pattern: `(?i)[\w.-]{1,50}?(?:secret)`,
			want:    "",
		},
		"prefix shape not at start": {
			pattern: `token(?i)[\w.-]{0,50}?(?:secret)`,
			want:    "",
		},
		"empty pattern": {
			pattern: "",
			want:    "",
		},
		"prefix only, nothing after": {
			// Degenerate but shape-valid: gate is the stripped remainder.
			pattern: `(?i)[\w.-]{0,50}?(?:x)`,
			want:    `(?i)(?:x)`,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, gateForPattern(tt.pattern))
		})
	}
}

func TestGateForPatternMatchesGeneratorShapes(t *testing.T) {
	tests := map[string]struct {
		pattern string
		want    string
	}{
		"case insensitive prefix": {
			pattern: generatedSemiGenericPattern(`(?i)[\w.-]{0,50}?(?:`, `foo|bar`),
			want:    generatedSemiGenericPattern(`(?i)(?:`, `foo|bar`),
		},
		"grouped prefix": {
			pattern: generatedSemiGenericPattern(`[\w.-]{0,50}?(?i:`, `foo|bar`),
			want:    generatedSemiGenericPattern(`(?i:`, `foo|bar`),
		},
		"other pattern": {
			pattern: `(?i)foo[\w.-]{0,50}?bar`,
			want:    "",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := gateForPattern(tt.pattern); got != tt.want {
				t.Fatalf("gateForPattern() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCompileRuleGates covers gate-map construction, including the
// compile-failure fallback (gate skipped, no panic, rule runs ungated).
func TestCompileRuleGates(t *testing.T) {
	gates := compileRuleGates(map[string]string{
		"semi-generic": `(?i)[\w.-]{0,50}?(?:secret)(\w{8})`,
		"specific":     `AKIA[0-9A-Z]{16}`,
		// Shape-valid prefix whose remainder is an invalid regex: the
		// stripped gate `(?i)(?:secret)(` must fail to compile and be
		// skipped silently.
		"broken-remainder": `(?i)[\w.-]{0,50}?(?:secret)(`,
	})

	assert.Contains(t, gates, "semi-generic")
	assert.NotContains(t, gates, "specific")
	assert.NotContains(t, gates, "broken-remainder")
}

func TestRuleGateOperatorRejectsNoOperatorFragment(t *testing.T) {
	pattern := generatedSemiGenericPattern(`(?i)[\w.-]{0,50}?(?:`, `foo|bar`)
	gates := compileRuleGates(map[string]string{"test-rule": pattern})

	gate, ok := gates["test-rule"]
	if !ok {
		t.Fatal("compileRuleGates did not build a gate")
	}
	if gate.requiredAny != semiGenericOperatorChars {
		t.Fatalf("requiredAny = %q, want %q", gate.requiredAny, semiGenericOperatorChars)
	}
	if gate.MatchString("foo token text without assignment") {
		t.Fatal("gate matched a fragment with no assignment operator byte")
	}
	if !gate.MatchString(`foo = "ABCDEF1234"`) {
		t.Fatal("gate rejected a generated semi-generic match")
	}
}

func TestRuleGateStatsCountOperatorByteRejects(t *testing.T) {
	pattern := generatedSemiGenericPattern(`(?i)[\w.-]{0,50}?(?:`, `foo|bar`)
	gates := compileRuleGates(map[string]string{"test-rule": pattern})
	gate := gates["test-rule"]
	stats := &ruleGateStats{}

	if gate.matchString("foo token text without assignment", nil, stats) {
		t.Fatal("gate matched a fragment with no assignment operator byte")
	}
	snapshot := stats.snapshot()
	if snapshot.RequiredAnyChecks != 1 {
		t.Fatalf("RequiredAnyChecks = %d, want 1", snapshot.RequiredAnyChecks)
	}
	if snapshot.RequiredAnyRejects != 1 {
		t.Fatalf("RequiredAnyRejects = %d, want 1", snapshot.RequiredAnyRejects)
	}
	if snapshot.RegexGateChecks != 0 {
		t.Fatalf("RegexGateChecks = %d, want 0 after byte rejection", snapshot.RegexGateChecks)
	}
}

func TestRuleGateRequiredAnyCacheScansOncePerPass(t *testing.T) {
	pattern := generatedSemiGenericPattern(`(?i)[\w.-]{0,50}?(?:`, `foo|bar`)
	gates := compileRuleGates(map[string]string{"test-rule": pattern})
	gate := gates["test-rule"]
	stats := &ruleGateStats{}
	facts := &ruleGateScanFacts{}
	fragment := "foo token text without assignment"

	for range 3 {
		if gate.matchString(fragment, facts, stats) {
			t.Fatal("gate matched a fragment with no assignment operator byte")
		}
	}

	snapshot := stats.snapshot()
	if snapshot.RequiredAnyChecks != 3 {
		t.Fatalf("RequiredAnyChecks = %d, want 3", snapshot.RequiredAnyChecks)
	}
	if snapshot.RequiredAnyRejects != 3 {
		t.Fatalf("RequiredAnyRejects = %d, want 3", snapshot.RequiredAnyRejects)
	}
	if snapshot.RequiredAnyCacheMisses != 1 {
		t.Fatalf("RequiredAnyCacheMisses = %d, want 1", snapshot.RequiredAnyCacheMisses)
	}
	if snapshot.RequiredAnyCacheHits != 2 {
		t.Fatalf("RequiredAnyCacheHits = %d, want 2", snapshot.RequiredAnyCacheHits)
	}
	if snapshot.RequiredAnyBytes != uint64(len(fragment)) {
		t.Fatalf("RequiredAnyBytes = %d, want one scan of %d bytes", snapshot.RequiredAnyBytes, len(fragment))
	}
}

func TestRuleGateOperatorRequirementIsGeneratorSpecific(t *testing.T) {
	tests := map[string]string{
		"no generated operator": `(?i)[\w.-]{0,50}?(?:foo)[A-Z]{10}`,
		"optional operator": `(?i)[\w.-]{0,50}?(?:foo)` +
			gateSemiGenericOperatorPattern + `?` +
			gateSemiGenericSecretPrefix + `[A-Z0-9]{10}` + gateSemiGenericSecretSuffix,
		"merged top-level alternative": generatedSemiGenericPattern(`(?i)[\w.-]{0,50}?(?:`, `foo`) +
			`|\b([A-Z0-9]{10}` + gateSemiGenericSecretSuffix,
	}

	for name, pattern := range tests {
		t.Run(name, func(t *testing.T) {
			gates := compileRuleGates(map[string]string{"test-rule": pattern})

			gate, ok := gates["test-rule"]
			if !ok {
				return
			}
			if gate.requiredAny != "" {
				t.Fatalf("requiredAny = %q, want empty", gate.requiredAny)
			}
		})
	}
}

func TestTopLevelAlternationDetection(t *testing.T) {
	tests := map[string]struct {
		pattern string
		want    bool
	}{
		"none": {
			pattern: generatedSemiGenericPattern(`(?i)[\w.-]{0,50}?(?:`, `foo|bar`),
		},
		"in character class": {
			pattern: `[A|B]{10}`,
		},
		"escaped": {
			pattern: `foo\|bar`,
		},
		"nested": {
			pattern: `foo(?:bar|baz)qux`,
		},
		"top level": {
			pattern: generatedSemiGenericPattern(`(?i)[\w.-]{0,50}?(?:`, `foo`) + `|\b([A-Z0-9]{10}` + gateSemiGenericSecretSuffix,
			want:    true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := hasTopLevelAlternation(tt.pattern); got != tt.want {
				t.Fatalf("hasTopLevelAlternation() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGeneratedSemiGenericGateAcceptsFullMatches(t *testing.T) {
	pattern := generatedSemiGenericPattern(`(?i)[\w.-]{0,50}?(?:`, `foo|bar`)
	full := blregexp.MustCompile(pattern)
	gates := compileRuleGates(map[string]string{"test-rule": pattern})
	gate := gates["test-rule"]

	samples := []string{
		`foo = "ABCDEF1234"`,
		`prefix-bar_token := 'ABCDEF1234'`,
		`bar=>ABCDEF1234`,
		`no match here`,
	}
	for _, sample := range samples {
		if full.MatchString(sample) && !gate.MatchString(sample) {
			t.Fatalf("gate rejected full match %q", sample)
		}
	}
}

// TestRuleGateSoundness is the money test: for every default-config rule
// that gets a gate, strings generated FROM THE FULL PATTERN must also match
// the gate. A single violation means the gate can reject a fragment the
// full pattern would have matched — i.e. silently missed secrets.
func TestRuleGateSoundness(t *testing.T) {
	cfg, err := config.Default()
	require.NoError(t, err)

	const perRule = 50

	gated := 0
	for ruleID, rule := range cfg.Rules {
		if rule.Regex == nil {
			continue
		}
		pattern := rule.Regex.String()
		gatePattern := gateForPattern(pattern)
		if gatePattern == "" {
			continue
		}
		gate, err := blregexp.Compile(gatePattern)
		if err != nil {
			continue // compileRuleGates skips these too
		}
		gated++

		gen, err := reggen.NewGenerator(pattern)
		if err != nil {
			// reggen can't parse some RE2 constructs; the invariant is
			// still exercised for every rule it can generate for.
			t.Logf("rule %s: reggen cannot generate (%v); skipped", ruleID, err)
			continue
		}
		gen.SetSeed(1) // deterministic

		for i := 0; i < perRule; i++ {
			s := gen.Generate(10)
			if !rule.Regex.MatchString(s) {
				// Generator drift (e.g. reggen emitting > max repeats);
				// only strings the full pattern matches are in scope.
				continue
			}
			assert.Truef(t, gate.MatchString(s),
				"UNSOUND GATE for rule %s: full pattern matched %q but gate rejected it\n  pattern: %s\n  gate:    %s",
				ruleID, s, pattern, gatePattern)
		}
	}

	// The optimization must actually cover the semi-generic rules; if the
	// default config's shapes drift, this fails loudly instead of the gates
	// silently evaporating.
	assert.Greater(t, gated, 0, "no default-config rules produced gates")
	t.Logf("soundness verified over %d gated rules x up to %d generated strings", gated, perRule)
}

func TestRuleGatesPreserveDefaultConfigFindingsOnSyntheticCorpus(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}

	fragment := sources.Fragment{
		Raw: `const Discord_Public_Key = "e7322523fb86ed64c836a979cf8465fbd436378c653c1db38f9ae87bc62a6fd5"
local discord public key token text without assignment
local adobe token placeholder text without assignment`,
		Attributes: map[string]string{sources.AttrPath: "tmp.py"},
	}

	withGates := NewDetector(cfg)
	withGates.MaxDecodeDepth = 0
	withoutGates := NewDetector(cfg)
	withoutGates.MaxDecodeDepth = 0
	withoutGates.ruleGates = nil
	withoutGates.mandatoryAtomGatesByRank = nil
	t.Logf("entropy=%f", shannonEntropy(fragment.Raw))
	t.Logf("rule findings=%#v", withoutGates.detectFragmentWithRule(fragment, fragment.Raw, cfg.Rules["atlassian-api-token"], nil, nil, nil, nil))

	got := withGates.detectFragment(context.Background(), fragment)
	want := withoutGates.detectFragment(context.Background(), fragment)
	if diff := cmp.Diff(want, got, cmpopts.IgnoreUnexported(report.Finding{})); diff != "" {
		t.Fatalf("gated detector findings differed (-want +got):\n%s", diff)
	}
}

func TestRuleGatesPreserveMergedDefaultRuleWithoutOperatorByte(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}

	secret := strings.ReplaceAll(
		"xFfGF0K3irG5tKKi-6u-wwaXQFeGwZ-IHR-hQ3CulkKtMSuteRQFfLZ6jihHThzZCg_UjnDt-4Wl_gIRf4zrZJs5JqaeuBhsfJ4W5GD6yGg3W7903gbvaxZPBjxIQQ7BgFDSkPS8oPispw4KLz56mdK-G6CIvLO6hHRrZHY0Q3tvJ6JxE=C63992E6",
		"=", "A",
	)
	if len(secret) != 186 {
		t.Fatalf("secret length = %d, want 186", len(secret))
	}
	fragment := sources.Fragment{
		Raw:        "ATATT3" + secret,
		Attributes: map[string]string{sources.AttrPath: "tmp.txt"},
	}
	if strings.ContainsAny(fragment.Raw, semiGenericOperatorChars) {
		t.Fatalf("test token unexpectedly contains operator byte: %q", fragment.Raw)
	}
	if !cfg.Rules["atlassian-api-token"].Regex.MatchString(fragment.Raw) {
		t.Fatal("atlassian-api-token regex did not match test token")
	}

	withGates := NewDetector(cfg)
	withGates.MaxDecodeDepth = 0
	withoutGates := NewDetector(cfg)
	withoutGates.MaxDecodeDepth = 0
	withoutGates.ruleGates = nil
	withoutGates.mandatoryAtomGatesByRank = nil

	got := withGates.detectFragment(context.Background(), fragment)
	want := withoutGates.detectFragment(context.Background(), fragment)
	if len(want) == 0 {
		t.Fatal("ungated detector did not find the merged-rule token")
	}
	if diff := cmp.Diff(want, got, cmpopts.IgnoreUnexported(report.Finding{})); diff != "" {
		t.Fatalf("gated detector findings differed (-want +got):\n%s", diff)
	}
}

func TestDetectorRuleGateStatsShowSkippedFullRegexCalls(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	fragment := benchmarkKeywordMissFragment(cfg)

	withGates := NewDetector(cfg)
	withGates.MaxDecodeDepth = 0
	withGates.ruleGateStats = &ruleGateStats{}
	if findings := withGates.detectFragment(context.Background(), fragment); len(findings) != 0 {
		t.Fatalf("gated detector found %d findings, want 0", len(findings))
	}
	withStats := withGates.ruleGateStats.snapshot()
	if withStats.GatedRuleChecks == 0 {
		t.Fatal("GatedRuleChecks = 0, want gate activity")
	}
	if withStats.RequiredAnyRejects == 0 {
		t.Fatal("RequiredAnyRejects = 0, want operator-byte rejections")
	}

	withoutGates := NewDetector(cfg)
	withoutGates.MaxDecodeDepth = 0
	withoutGates.ruleGates = nil
	withoutGates.mandatoryAtomGatesByRank = nil
	withoutGates.ruleGateStats = &ruleGateStats{}
	if findings := withoutGates.detectFragment(context.Background(), fragment); len(findings) != 0 {
		t.Fatalf("ungated detector found %d findings, want 0", len(findings))
	}
	withoutStats := withoutGates.ruleGateStats.snapshot()
	if withStats.FullRegexCalls >= withoutStats.FullRegexCalls {
		t.Fatalf("FullRegexCalls with gates = %d, without gates = %d; want gates to skip calls", withStats.FullRegexCalls, withoutStats.FullRegexCalls)
	}
	if withStats.FullRegexBytes >= withoutStats.FullRegexBytes {
		t.Fatalf("FullRegexBytes with gates = %d, without gates = %d; want gates to skip bytes", withStats.FullRegexBytes, withoutStats.FullRegexBytes)
	}
}

// sourcesFragment wraps raw content the way TestDetect does.
func sourcesFragment(raw string) sources.Fragment {
	return sources.Fragment{
		Raw:        raw,
		Attributes: map[string]string{sources.AttrPath: "corpus.txt"},
	}
}

// findingKeys projects findings onto the identity fields the digest guard
// uses, sorted for order-independent comparison.
func findingKeys(fs []report.Finding) []string {
	keys := make([]string, 0, len(fs))
	for _, f := range fs {
		keys = append(keys, f.RuleID+"\x00"+f.Secret+"\x00"+f.Match)
	}
	sort.Strings(keys)
	return keys
}

// TestRuleGateDifferential runs detection over the rule-test corpus embedded
// in TestDetect-style fragments with gates enabled vs disabled and asserts
// identical findings. This catches unsoundness on realistic secret shapes
// (as opposed to reggen's synthetic ones).
func TestRuleGateDifferential(t *testing.T) {
	cfg, err := config.Default()
	require.NoError(t, err)

	// Corpus: one synthetic fragment per gated rule, generated from the full
	// pattern so it is guaranteed to contain a match, embedded in benign
	// context on both sides.
	type sample struct {
		ruleID string
		raw    string
	}
	var corpus []sample
	for ruleID, rule := range cfg.Rules {
		if rule.Regex == nil {
			continue
		}
		if gateForPattern(rule.Regex.String()) == "" {
			continue
		}
		gen, err := reggen.NewGenerator(rule.Regex.String())
		if err != nil {
			continue
		}
		gen.SetSeed(2)
		secret := gen.Generate(10)
		if !rule.Regex.MatchString(secret) {
			continue
		}
		corpus = append(corpus, sample{
			ruleID: ruleID,
			raw:    "// config for service\nlet unrelated = 42\n" + secret + "\n# trailing comment\n",
		})
	}
	require.NotEmpty(t, corpus)

	newDetector := func(gatesEnabled bool) *Detector {
		d := NewDetector(cfg)
		if !gatesEnabled {
			d.ruleGates = nil // detectFragmentWithRule then always runs the full pattern
			d.mandatoryAtomGatesByRank = nil
		}
		return d
	}

	gatedDet := newDetector(true)
	ungatedDet := newDetector(false)
	require.NotEmpty(t, gatedDet.ruleGates, "gated detector has no gates; differential is vacuous")

	for _, s := range corpus {
		frag := sourcesFragment(s.raw)
		got := findingKeys(gatedDet.Detect(frag))
		want := findingKeys(ungatedDet.Detect(frag))
		assert.Equalf(t, want, got, "findings diverge with gates enabled (corpus rule %s)", s.ruleID)
	}
}

func generatedSemiGenericPattern(prefix, identifiers string) string {
	return prefix + identifiers + gateSemiGenericOperatorPattern + gateSemiGenericSecretPrefix + `[A-Z0-9]{10}` + gateSemiGenericSecretSuffix
}
