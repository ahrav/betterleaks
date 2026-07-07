package detect

import (
	"strings"

	blregexp "github.com/betterleaks/betterleaks/regexp"
)

// Semi-generic rules (built by utils.GenerateSemiGenericRegex) start with an
// optional identifier prefix. The prefix triples the regex engine's per-byte
// scan cost, yet most fragments contain no match at all: on a large corpus
// ~93% of generic-api-key scans return nothing. Because the prefix is
// optional ({0,50}), any match of the full pattern implies a match of the
// pattern with the prefix removed (start at the keyword instead). That makes
// the prefix-stripped pattern a sound rejection gate: run it first with a
// cheap "does it match at all" check, and only pay for the full pattern on
// fragments the gate accepts. Findings are unchanged — the full pattern
// still produces the actual matches.
const (
	gateCaseInsensitivePrefix = `(?i)[\w.-]{0,50}?(?:`
	gateGroupedPrefix         = `[\w.-]{0,50}?(?i:`
)

// gateForPattern returns the prefix-stripped gate pattern, or "" when the
// pattern doesn't have the semi-generic prefix shape.
func gateForPattern(pattern string) string {
	if rest, ok := strings.CutPrefix(pattern, gateCaseInsensitivePrefix); ok {
		return `(?i)(?:` + rest
	}
	if rest, ok := strings.CutPrefix(pattern, gateGroupedPrefix); ok {
		return `(?i:` + rest
	}
	return ""
}

// compileRuleGates builds gate regexes for every rule whose pattern has the
// semi-generic prefix shape. Gates that fail to compile are skipped (the
// full pattern simply runs ungated).
func compileRuleGates(ruleIDToPattern map[string]string) map[string]*blregexp.Regexp {
	gates := make(map[string]*blregexp.Regexp)
	for ruleID, pattern := range ruleIDToPattern {
		gatePattern := gateForPattern(pattern)
		if gatePattern == "" {
			continue
		}
		gate, err := blregexp.Compile(gatePattern)
		if err != nil {
			continue
		}
		gates[ruleID] = gate
	}
	return gates
}
