package detect

import (
	"bytes"
	"regexp/syntax"
	"slices"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

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
	gateCaseInsensitivePrefix      = `(?i)[\w.-]{0,50}?(?:`
	gateGroupedPrefix              = `[\w.-]{0,50}?(?i:`
	gateSemiGenericOperatorPattern = `)(?:[ \t\w.-]{0,20})[\s'"]{0,3}(?:=|>|:{1,3}=|\|\||:|=>|\?=|,)`
	gateSemiGenericSecretPrefix    = `[\x60'"\s=]{0,5}(`
	gateSemiGenericSecretSuffix    = `)(?:\\?['"\x60]|[\s;]|\\[nr]|$)`
	semiGenericOperatorChars       = "=>:|?,"
)

type ruleGate struct {
	regex       *blregexp.Regexp
	requiredAny string
}

func (g ruleGate) MatchString(s string) bool {
	return g.matchString(s, nil, nil)
}

// matchWindows is matchString for hit-window verification: requiredAny is
// evaluated against the FULL fragment (both because the shared facts cache
// is fragment-scoped and because a fragment-wide reject is sound), while
// the gate regex runs only over the window slices. Sound because every
// gate match provably contains a rule keyword and fits within the merged
// windows (see rule_window.go).
func (g ruleGate) matchWindows(s string, windows [][2]int, facts *ruleGateScanFacts, stats *ruleGateStats) bool {
	if g.regex == nil {
		if stats != nil {
			stats.gateCompileFailOpens.Add(1)
		}
		return true
	}
	if err := g.regex.Compile(); err != nil {
		if stats != nil {
			stats.gateCompileFailOpens.Add(1)
		}
		return true
	}
	if g.requiredAny != "" {
		if stats != nil {
			stats.requiredAnyChecks.Add(1)
		}
		if !requiredAnyContains(facts, s, g.requiredAny, stats) {
			if stats != nil {
				stats.requiredAnyRejects.Add(1)
			}
			return false
		}
	}
	for _, w := range windows {
		if stats != nil {
			stats.regexGateChecks.Add(1)
			stats.regexGateBytes.Add(uint64(w[1] - w[0]))
		}
		if g.regex.MatchString(s[w[0]:w[1]]) {
			if stats != nil {
				stats.regexGateAccepts.Add(1)
			}
			return true
		}
	}
	if stats != nil {
		stats.regexGateRejects.Add(1)
	}
	return false
}

func (g ruleGate) matchString(s string, facts *ruleGateScanFacts, stats *ruleGateStats) bool {
	if g.regex == nil {
		if stats != nil {
			stats.gateCompileFailOpens.Add(1)
		}
		return true
	}
	if err := g.regex.Compile(); err != nil {
		if stats != nil {
			stats.gateCompileFailOpens.Add(1)
		}
		return true
	}
	if g.requiredAny != "" {
		if stats != nil {
			stats.requiredAnyChecks.Add(1)
		}
		if !requiredAnyContains(facts, s, g.requiredAny, stats) {
			if stats != nil {
				stats.requiredAnyRejects.Add(1)
			}
			return false
		}
	}
	if stats != nil {
		stats.regexGateChecks.Add(1)
		stats.regexGateBytes.Add(uint64(len(s)))
	}
	if g.regex.MatchString(s) {
		if stats != nil {
			stats.regexGateAccepts.Add(1)
		}
		return true
	}
	if stats != nil {
		stats.regexGateRejects.Add(1)
	}
	return false
}

type ruleGateScanFacts struct {
	semiGenericOperatorComputed bool
	semiGenericOperatorPresent  bool
}

func requiredAnyContains(facts *ruleGateScanFacts, s, requiredAny string, stats *ruleGateStats) bool {
	if facts == nil || requiredAny != semiGenericOperatorChars {
		if stats != nil {
			stats.requiredAnyBytes.Add(uint64(len(s)))
		}
		return strings.ContainsAny(s, requiredAny)
	}

	if !facts.semiGenericOperatorComputed {
		if stats != nil {
			stats.requiredAnyCacheMisses.Add(1)
			stats.requiredAnyBytes.Add(uint64(len(s)))
		}
		facts.semiGenericOperatorPresent = strings.ContainsAny(s, requiredAny)
		facts.semiGenericOperatorComputed = true
	} else if stats != nil {
		stats.requiredAnyCacheHits.Add(1)
	}
	return facts.semiGenericOperatorPresent
}

type ruleGateStats struct {
	fragments              atomic.Uint64
	decodePasses           atomic.Uint64
	decodePassBytes        atomic.Uint64
	ruleChecks             atomic.Uint64
	gatedRuleChecks        atomic.Uint64
	requiredAnyChecks      atomic.Uint64
	requiredAnyRejects     atomic.Uint64
	requiredAnyBytes       atomic.Uint64
	requiredAnyCacheHits   atomic.Uint64
	requiredAnyCacheMisses atomic.Uint64
	regexGateChecks        atomic.Uint64
	regexGateRejects       atomic.Uint64
	regexGateAccepts       atomic.Uint64
	regexGateBytes         atomic.Uint64
	mandatoryAtomChecks    atomic.Uint64
	mandatoryAtomRejects   atomic.Uint64
	mandatoryAtomAccepts   atomic.Uint64
	mandatoryAtomBytes     atomic.Uint64
	gateCompileFailOpens   atomic.Uint64
	fullRegexCalls         atomic.Uint64
	fullRegexNoMatchCalls  atomic.Uint64
	fullRegexMatchCalls    atomic.Uint64
	fullRegexMatchSpans    atomic.Uint64
	fullRegexBytes         atomic.Uint64
	filteredFindings       atomic.Uint64
}

type ruleGateStatsSnapshot struct {
	Fragments              uint64
	DecodePasses           uint64
	DecodePassBytes        uint64
	RuleChecks             uint64
	GatedRuleChecks        uint64
	RequiredAnyChecks      uint64
	RequiredAnyRejects     uint64
	RequiredAnyBytes       uint64
	RequiredAnyCacheHits   uint64
	RequiredAnyCacheMisses uint64
	RegexGateChecks        uint64
	RegexGateRejects       uint64
	RegexGateAccepts       uint64
	RegexGateBytes         uint64
	MandatoryAtomChecks    uint64
	MandatoryAtomRejects   uint64
	MandatoryAtomAccepts   uint64
	MandatoryAtomBytes     uint64
	GateCompileFailOpens   uint64
	FullRegexCalls         uint64
	FullRegexNoMatchCalls  uint64
	FullRegexMatchCalls    uint64
	FullRegexMatchSpans    uint64
	FullRegexBytes         uint64
	FilteredFindings       uint64
}

func (s *ruleGateStats) snapshot() ruleGateStatsSnapshot {
	if s == nil {
		return ruleGateStatsSnapshot{}
	}
	return ruleGateStatsSnapshot{
		Fragments:              s.fragments.Load(),
		DecodePasses:           s.decodePasses.Load(),
		DecodePassBytes:        s.decodePassBytes.Load(),
		RuleChecks:             s.ruleChecks.Load(),
		GatedRuleChecks:        s.gatedRuleChecks.Load(),
		RequiredAnyChecks:      s.requiredAnyChecks.Load(),
		RequiredAnyRejects:     s.requiredAnyRejects.Load(),
		RequiredAnyBytes:       s.requiredAnyBytes.Load(),
		RequiredAnyCacheHits:   s.requiredAnyCacheHits.Load(),
		RequiredAnyCacheMisses: s.requiredAnyCacheMisses.Load(),
		RegexGateChecks:        s.regexGateChecks.Load(),
		RegexGateRejects:       s.regexGateRejects.Load(),
		RegexGateAccepts:       s.regexGateAccepts.Load(),
		RegexGateBytes:         s.regexGateBytes.Load(),
		MandatoryAtomChecks:    s.mandatoryAtomChecks.Load(),
		MandatoryAtomRejects:   s.mandatoryAtomRejects.Load(),
		MandatoryAtomAccepts:   s.mandatoryAtomAccepts.Load(),
		MandatoryAtomBytes:     s.mandatoryAtomBytes.Load(),
		GateCompileFailOpens:   s.gateCompileFailOpens.Load(),
		FullRegexCalls:         s.fullRegexCalls.Load(),
		FullRegexNoMatchCalls:  s.fullRegexNoMatchCalls.Load(),
		FullRegexMatchCalls:    s.fullRegexMatchCalls.Load(),
		FullRegexMatchSpans:    s.fullRegexMatchSpans.Load(),
		FullRegexBytes:         s.fullRegexBytes.Load(),
		FilteredFindings:       s.filteredFindings.Load(),
	}
}

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

func requiredAnyForGate(pattern string) string {
	if hasTopLevelAlternation(pattern) {
		return ""
	}
	if strings.Contains(pattern, gateSemiGenericOperatorPattern+gateSemiGenericSecretPrefix) &&
		strings.HasSuffix(pattern, gateSemiGenericSecretSuffix) {
		return semiGenericOperatorChars
	}
	return ""
}

func hasTopLevelAlternation(pattern string) bool {
	depth := 0
	escaped := false
	inClass := false
	for _, r := range pattern {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if inClass {
			if r == ']' {
				inClass = false
			}
			continue
		}

		switch r {
		case '[':
			inClass = true
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case '|':
			if depth == 0 {
				return true
			}
		}
	}
	return false
}

func mandatoryAtomGateForPattern(pattern string, keywords []string) [][]byte {
	parsed, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return nil
	}
	atoms := usefulMandatoryAtoms(requiredLiteralAtoms(parsed.Simplify()))
	if len(atoms) == 0 {
		return nil
	}
	gate := make([][]byte, 0, len(atoms))
	for _, atom := range atoms {
		if atomCoveredByKeywords(atom, keywords) {
			continue
		}
		gate = append(gate, []byte(atom))
	}
	return gate
}

func mandatoryAtomsPresent(lowerBuf []byte, atoms [][]byte) bool {
	for _, atom := range atoms {
		if !bytes.Contains(lowerBuf, atom) {
			return false
		}
	}
	return true
}

func requiredLiteralAtoms(re *syntax.Regexp) []string {
	if re == nil {
		return nil
	}
	switch re.Op {
	case syntax.OpLiteral:
		return []string{string(re.Rune)}
	case syntax.OpCapture, syntax.OpPlus:
		if len(re.Sub) == 0 {
			return nil
		}
		return requiredLiteralAtoms(re.Sub[0])
	case syntax.OpRepeat:
		if re.Min <= 0 || len(re.Sub) == 0 {
			return nil
		}
		return requiredLiteralAtoms(re.Sub[0])
	case syntax.OpConcat:
		var atoms []string
		for _, sub := range re.Sub {
			atoms = append(atoms, requiredLiteralAtoms(sub)...)
		}
		return dedupeStrings(atoms)
	case syntax.OpAlternate:
		if len(re.Sub) == 0 {
			return nil
		}
		common := stringSet(requiredLiteralAtoms(re.Sub[0]))
		for _, sub := range re.Sub[1:] {
			next := stringSet(requiredLiteralAtoms(sub))
			for atom := range common {
				if _, ok := next[atom]; !ok {
					delete(common, atom)
				}
			}
		}
		return sortedSet(common)
	default:
		return nil
	}
}

func usefulMandatoryAtoms(atoms []string) []string {
	const minAtomLen = 4
	useful := make([]string, 0, len(atoms))
	for _, atom := range atoms {
		atom = strings.ToLower(atom)
		if len(atom) < minAtomLen || !isASCIIPrintable(atom) {
			continue
		}
		useful = append(useful, atom)
	}
	return dedupeStrings(useful)
}

func atomCoveredByKeywords(atom string, keywords []string) bool {
	for _, keyword := range keywords {
		keyword = strings.ToLower(keyword)
		if strings.Contains(atom, keyword) || strings.Contains(keyword, atom) {
			return true
		}
	}
	return false
}

func isASCIIPrintable(s string) bool {
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		if r > unicode.MaxASCII || r < 0x20 || r == 0x7f {
			return false
		}
		s = s[size:]
	}
	return true
}

func stringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func sortedSet(set map[string]struct{}) []string {
	values := make([]string, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	slices.Sort(values)
	return values
}

func dedupeStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	set := stringSet(values)
	return sortedSet(set)
}

// compileRuleGates builds gate regexes for every rule whose pattern has the
// semi-generic prefix shape. Gates that fail to parse are skipped. If a gate
// later fails engine compilation, ruleGate.MatchString accepts the fragment so
// the full pattern still runs ungated.
func compileRuleGates(ruleIDToPattern map[string]string) map[string]ruleGate {
	gates := make(map[string]ruleGate)
	for ruleID, pattern := range ruleIDToPattern {
		gatePattern := gateForPattern(pattern)
		if gatePattern == "" {
			continue
		}
		gate, err := blregexp.Compile(gatePattern)
		if err != nil {
			continue
		}
		gates[ruleID] = ruleGate{
			regex:       gate,
			requiredAny: requiredAnyForGate(pattern),
		}
	}
	return gates
}
