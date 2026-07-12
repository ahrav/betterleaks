package detect

import (
	"bytes"
	"regexp/syntax"
	"sort"
	"strings"
	"unicode/utf8"
)

// Hit-window candidate verification: for rules where every regex match
// provably contains one of the rule's keywords, the gate regex and full
// regex only need to scan small windows around keyword occurrences instead
// of the whole fragment. The prefilter already establishes which keywords
// occur; occurrences are found with vectorized bytes.Index over the
// lowered buffer for the ~1.6 candidate rules per fragment.
//
// Soundness (verified by the differential test in rule_window_test.go):
//  1. Windowing applies only when matchGuaranteesKeyword proves every
//     match of the pattern contains a rule keyword (ASCII-lowered) as a
//     substring and the pattern's max match width W is bounded — either
//     statically, or per-fragment when every unbounded repeat is provably
//     newline-free (its runs are then bounded by the fragment's longest
//     line). Anchors need no exemption: by (2)-(3) no match touches an
//     interior window edge, so ^ $ \A \z and \b evaluate on window
//     slices exactly as on the fragment (edges only coincide with
//     fragment edges, where semantics agree).
//  2. A match containing keyword occurrence [s,e) spans at most
//     [e-W, s+W]. The base window [e-W-1, s+W+1) therefore contains any
//     such match with >= 1 byte of margin on both sides (except at true
//     fragment boundaries, where clamping preserves real string edges),
//     so \b and \B evaluate identically on the window slice.
//  3. Any match found in a window slice contains some keyword occurrence
//     o' (by 1, occurrences are byte-identical in window and fragment);
//     o's base window overlaps this window, so interval merging placed
//     them in the same merged window, and by 2 the match sits >= 1 byte
//     inside it: window scanning produces no boundary-spurious matches.
//  4. Merged windows are disjoint and sorted, and every match lies inside
//     exactly one, so concatenating per-window FindAllStringIndex results
//     (rebased) reproduces the full-fragment leftmost non-overlapping
//     match list exactly.
//  5. RE2 (?i) also folds ASCII k/s to U+212A/U+017F, which the ASCII
//     lowering can't surface as occurrences. Fragments containing either
//     rune disable windowing for the decode pass (checked once per pass;
//     both are vanishingly rare in practice).

// kelvinSign and longS are the only non-ASCII case-fold partners of ASCII
// letters under RE2 (?i); their UTF-8 encodings, plus their uppercase-fold
// context, gate windowing per decode pass.
var unicodeFoldTraps = [][]byte{
	[]byte("K"), // KELVIN SIGN, folds with k/K
	[]byte("ſ"), // LATIN SMALL LETTER LONG S, folds with s/S
}

func hasUnicodeFoldTrap(lowerBuf []byte) bool {
	for _, trap := range unicodeFoldTraps {
		if bytes.Contains(lowerBuf, trap) {
			return true
		}
	}
	return false
}

// windowPlan describes how to window a rule: W = bounded +
// nlFreeSites*maxLineLen(fragment). nlFreeSites counts unbounded repeat
// sites whose bodies cannot consume a newline.
type windowPlan struct {
	bounded     int
	nlFreeSites int
}

func (wp windowPlan) valid() bool { return wp.bounded > 0 || wp.nlFreeSites > 0 }

// width returns the per-fragment window half-width.
func (wp windowPlan) width(maxLineLen int) int {
	return wp.bounded + wp.nlFreeSites*maxLineLen
}

// windowPlanForPattern returns the window plan for a rule pattern, or an
// invalid plan when the rule must be scanned full-fragment.
func windowPlanForPattern(pattern string, keywords []string) windowPlan {
	if len(keywords) == 0 {
		return windowPlan{}
	}
	parsed, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return windowPlan{}
	}
	parsed = parsed.Simplify()
	lowered := make([]string, 0, len(keywords))
	for _, kw := range keywords {
		kw = strings.ToLower(kw)
		if kw == "" {
			return windowPlan{}
		}
		lowered = append(lowered, kw)
	}
	if !matchGuaranteesKeyword(parsed, lowered) {
		return windowPlan{}
	}
	bounded, sites, ok := maxMatchBytes(parsed)
	if !ok || (bounded <= 0 && sites <= 0) {
		return windowPlan{}
	}
	return windowPlan{bounded: bounded, nlFreeSites: sites}
}

// matchGuaranteesKeyword reports whether every match of re must contain at
// least one of the (lowercase) keywords as an ASCII-lowered substring. The
// proof walks mandatory structure: a concat guarantees a keyword if any
// contiguous run of exactly-enumerable subexpressions expands to strings
// that all contain a keyword (the simplifier splits literals like "access"
// into "a"+("ccess"|"uth"|"pi"), so single-literal checks are not enough);
// an alternation only if every branch does; repeats with min >= 1 defer to
// their body.
func matchGuaranteesKeyword(re *syntax.Regexp, keywords []string) bool {
	if exact, ok := exactStrings(re); ok && len(exact) > 0 {
		return allContainKeyword(exact, keywords)
	}
	switch re.Op {
	case syntax.OpCapture, syntax.OpPlus:
		return len(re.Sub) == 1 && matchGuaranteesKeyword(re.Sub[0], keywords)
	case syntax.OpRepeat:
		return re.Min >= 1 && len(re.Sub) == 1 && matchGuaranteesKeyword(re.Sub[0], keywords)
	case syntax.OpConcat:
		// Any single sub that guarantees a keyword suffices.
		for _, sub := range re.Sub {
			if matchGuaranteesKeyword(sub, keywords) {
				return true
			}
		}
		// Otherwise look for a contiguous run of exact subs whose joint
		// expansion always contains a keyword. The run is mandatory and
		// contiguous in the matched text.
		for i := 0; i < len(re.Sub); i++ {
			cur, ok := exactStrings(re.Sub[i])
			if !ok {
				continue
			}
			for j := i + 1; j <= len(re.Sub); j++ {
				if len(cur) > 0 && allContainKeyword(cur, keywords) {
					return true
				}
				if j == len(re.Sub) {
					break
				}
				next, ok := exactStrings(re.Sub[j])
				if !ok {
					break
				}
				cur, ok = crossProduct(cur, next)
				if !ok {
					break
				}
			}
		}
		return false
	case syntax.OpAlternate:
		if len(re.Sub) == 0 {
			return false
		}
		for _, sub := range re.Sub {
			if !matchGuaranteesKeyword(sub, keywords) {
				return false
			}
		}
		return true
	}
	return false
}

// exactStringsLimit bounds expansion of alternation cross products.
const exactStringsLimit = 128

// exactStrings returns the exact (ASCII-lowered) set of strings re can
// match, when that set is finite, small, and enumerable.
func exactStrings(re *syntax.Regexp) ([]string, bool) {
	switch re.Op {
	case syntax.OpEmptyMatch:
		return []string{""}, true
	case syntax.OpLiteral:
		return []string{strings.ToLower(string(re.Rune))}, true
	case syntax.OpCharClass:
		var runes []rune
		for i := 0; i+1 < len(re.Rune); i += 2 {
			for r := re.Rune[i]; r <= re.Rune[i+1]; r++ {
				if r > 0x7F || len(runes) >= 8 {
					return nil, false
				}
				runes = append(runes, r)
			}
		}
		out := make([]string, 0, len(runes))
		for _, r := range runes {
			out = append(out, strings.ToLower(string(r)))
		}
		return out, true
	case syntax.OpCapture:
		if len(re.Sub) != 1 {
			return nil, false
		}
		return exactStrings(re.Sub[0])
	case syntax.OpConcat:
		acc := []string{""}
		for _, sub := range re.Sub {
			next, ok := exactStrings(sub)
			if !ok {
				return nil, false
			}
			acc, ok = crossProduct(acc, next)
			if !ok {
				return nil, false
			}
		}
		return acc, true
	case syntax.OpAlternate:
		var acc []string
		for _, sub := range re.Sub {
			next, ok := exactStrings(sub)
			if !ok {
				return nil, false
			}
			acc = append(acc, next...)
			if len(acc) > exactStringsLimit {
				return nil, false
			}
		}
		return acc, true
	}
	return nil, false
}

func crossProduct(a, b []string) ([]string, bool) {
	if len(a)*len(b) > exactStringsLimit {
		return nil, false
	}
	out := make([]string, 0, len(a)*len(b))
	for _, x := range a {
		for _, y := range b {
			out = append(out, x+y)
		}
	}
	return out, true
}

func allContainKeyword(strs, keywords []string) bool {
outer:
	for _, s := range strs {
		for _, kw := range keywords {
			if strings.Contains(s, kw) {
				continue outer
			}
		}
		return false
	}
	return true
}

// maxMatchBytes bounds the byte length of any match of re as
// bounded + nlFreeSites*maxLineLen: bounded covers all finite structure,
// and each unbounded repeat contributes one newline-free run (bounded by
// the fragment's longest line) iff its body provably cannot consume a
// newline. ok is false when an unbounded repeat could span newlines.
func maxMatchBytes(re *syntax.Regexp) (bounded, nlFreeSites int, ok bool) {
	const maxRuneBytes = 4
	switch re.Op {
	case syntax.OpNoMatch, syntax.OpEmptyMatch,
		syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText,
		syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return 0, 0, true
	case syntax.OpLiteral:
		return len(string(re.Rune)), 0, true
	case syntax.OpCharClass:
		w := 1
		for i := 1; i < len(re.Rune); i += 2 {
			if n := utf8.RuneLen(re.Rune[i]); n > w && n > 0 {
				w = n
			}
		}
		return w, 0, true
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return maxRuneBytes, 0, true
	case syntax.OpCapture, syntax.OpQuest:
		if len(re.Sub) != 1 {
			return 0, 0, false
		}
		return maxMatchBytes(re.Sub[0])
	case syntax.OpStar, syntax.OpPlus:
		if len(re.Sub) != 1 || !cannotMatchNewline(re.Sub[0]) {
			return 0, 0, false
		}
		// The repeat's total consumption is one newline-free run; any
		// bounded width inside the body is subsumed by the run bound.
		return 0, 1, true
	case syntax.OpRepeat:
		if len(re.Sub) != 1 {
			return 0, 0, false
		}
		if re.Max < 0 {
			if !cannotMatchNewline(re.Sub[0]) {
				return 0, 0, false
			}
			return 0, 1, true
		}
		b, s, ok := maxMatchBytes(re.Sub[0])
		if !ok {
			return 0, 0, false
		}
		return b * re.Max, s * re.Max, true
	case syntax.OpConcat:
		for _, sub := range re.Sub {
			b, s, ok := maxMatchBytes(sub)
			if !ok {
				return 0, 0, false
			}
			bounded += b
			nlFreeSites += s
		}
		return bounded, nlFreeSites, true
	case syntax.OpAlternate:
		for _, sub := range re.Sub {
			b, s, ok := maxMatchBytes(sub)
			if !ok {
				return 0, 0, false
			}
			if b > bounded {
				bounded = b
			}
			if s > nlFreeSites {
				nlFreeSites = s
			}
		}
		return bounded, nlFreeSites, true
	}
	return 0, 0, false
}

// cannotMatchNewline reports whether re provably never consumes a newline
// byte. Conservative: unknown ops return false.
func cannotMatchNewline(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpNoMatch, syntax.OpEmptyMatch,
		syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText,
		syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return true
	case syntax.OpLiteral:
		return !strings.ContainsRune(string(re.Rune), '\n')
	case syntax.OpCharClass:
		for i := 0; i+1 < len(re.Rune); i += 2 {
			if re.Rune[i] <= '\n' && '\n' <= re.Rune[i+1] {
				return false
			}
		}
		return true
	case syntax.OpAnyCharNotNL:
		return true
	case syntax.OpAnyChar:
		return false
	case syntax.OpCapture, syntax.OpStar, syntax.OpPlus, syntax.OpQuest, syntax.OpRepeat, syntax.OpConcat, syntax.OpAlternate:
		for _, sub := range re.Sub {
			if !cannotMatchNewline(sub) {
				return false
			}
		}
		return true
	}
	return false
}

// ruleWindows returns the merged, disjoint, sorted scan windows for a rule
// over the lowered fragment, or nil when the rule should fall back to a
// full-fragment scan (no occurrences found, or windows would not save
// meaningful work).
func ruleWindows(lowerBuf []byte, keywords [][]byte, maxWidth int) [][2]int {
	var spans [][2]int
	for _, kw := range keywords {
		from := 0
		for {
			idx := bytes.Index(lowerBuf[from:], kw)
			if idx < 0 {
				break
			}
			occStart := from + idx
			occEnd := occStart + len(kw)
			start := occEnd - maxWidth - 1
			if start < 0 {
				start = 0
			}
			end := occStart + maxWidth + 1
			if end > len(lowerBuf) {
				end = len(lowerBuf)
			}
			spans = append(spans, [2]int{start, end})
			from = occStart + 1
		}
	}
	if len(spans) == 0 {
		return nil
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	merged := spans[:1]
	for _, s := range spans[1:] {
		last := &merged[len(merged)-1]
		if s[0] <= last[1] {
			if s[1] > last[1] {
				last[1] = s[1]
			}
		} else {
			merged = append(merged, s)
		}
	}
	// If windows cover most of the fragment, the bookkeeping outweighs the
	// savings; scan full-fragment instead.
	total := 0
	for _, w := range merged {
		total += w[1] - w[0]
	}
	if total*4 >= len(lowerBuf)*3 {
		return nil
	}
	return merged
}

// findAllInWindows runs regex.FindAllStringIndex over each window slice and
// rebases match indices to fragment coordinates. Windows are disjoint and
// sorted, so the concatenation preserves full-scan output order.
func findAllInWindows(regex interface {
	FindAllStringIndex(s string, n int) [][]int
}, raw string, windows [][2]int) [][]int {
	var matches [][]int
	for _, w := range windows {
		for _, m := range regex.FindAllStringIndex(raw[w[0]:w[1]], -1) {
			matches = append(matches, []int{m[0] + w[0], m[1] + w[0]})
		}
	}
	return matches
}

// longestNewlineFreeRun returns the length of the longest run of bytes
// without '\n' in buf — the per-fragment bound for newline-free repeat
// sites in window plans.
func longestNewlineFreeRun(buf []byte) int {
	best, from := 0, 0
	for {
		idx := bytes.IndexByte(buf[from:], '\n')
		if idx < 0 {
			if r := len(buf) - from; r > best {
				best = r
			}
			return best
		}
		if idx > best {
			best = idx
		}
		from += idx + 1
	}
}
