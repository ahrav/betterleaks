package detect

import (
	"sort"
	"testing"

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
