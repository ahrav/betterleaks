package exprruntime

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/betterleaks/betterleaks/v2/internal/confidence"
	"github.com/betterleaks/betterleaks/v2/internal/tokenizer"
	"github.com/betterleaks/betterleaks/v2/internal/words"
	blregexp "github.com/betterleaks/betterleaks/v2/regexp"
	ahocorasick "github.com/rrethy/ahocorasick"
)

var (
	regexCache  sync.Map // string -> *blregexp.Regexp
	acTrieCache sync.Map // string -> *ahocorasick.Matcher

	// listRegexCache remembers the compiled regex for a pattern list by the
	// identity of the []any the Expr VM hands to matchesAny. Expr folds a
	// constant list literal into one slice that every evaluation reuses, so
	// this skips converting and joining ~30 patterns into a 2 KB cache key on
	// every path and finding. Entries verify their patterns before use, so a
	// recycled address with different contents cannot return a stale regex.
	listRegexCache sync.Map // unsafe.Pointer(first element) -> *listRegexEntry
	listRegexCount atomic.Int32
)

// maxListRegexEntries bounds the identity cache; dynamic lists built per
// evaluation would otherwise grow it without limit.
const maxListRegexEntries = 1024

type listRegexEntry struct {
	patterns []any
	re       *blregexp.Regexp
	err      error
}

func (e *listRegexEntry) matches(list []any) bool {
	if len(e.patterns) != len(list) {
		return false
	}
	for i, p := range e.patterns {
		if p != list[i] {
			return false
		}
	}
	return true
}

// joinedRegexForList resolves the alternation regex for an Expr pattern list.
func joinedRegexForList(patterns any) (*blregexp.Regexp, error) {
	list, ok := patterns.([]any)
	if !ok || len(list) == 0 {
		return getOrCompileJoinedRegex(toStringSlice(patterns))
	}
	key := unsafe.Pointer(unsafe.SliceData(list))
	if v, ok := listRegexCache.Load(key); ok {
		if e := v.(*listRegexEntry); e.matches(list) {
			return e.re, e.err
		}
	}
	re, err := getOrCompileJoinedRegex(toStringSlice(list))
	if listRegexCount.Add(1) <= maxListRegexEntries {
		listRegexCache.Store(key, &listRegexEntry{patterns: slices.Clone(list), re: re, err: err})
	} else {
		listRegexCount.Add(-1)
	}
	return re, err
}

func (rt *runtimeBindings) setConfidence(value string) (string, error) {
	if !confidence.Valid(value) {
		return "", fmt.Errorf("setConfidence: invalid confidence %q (expected low, medium, or high)", value)
	}
	rt.attrs.(map[string]string)[confidence.Attribute] = value
	return value, nil
}

func orderedKey(ss []string) string { return strings.Join(ss, "\x00") }

func sortedKey(ss []string) string {
	cp := make([]string, len(ss))
	copy(cp, ss)
	sort.Strings(cp)
	return strings.Join(cp, "\x00")
}

func getOrCompileJoinedRegex(patterns []string) (*blregexp.Regexp, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	key := orderedKey(patterns)
	if v, ok := regexCache.Load(key); ok {
		return v.(*blregexp.Regexp), nil
	}
	parts := make([]string, len(patterns))
	for i, p := range patterns {
		parts[i] = "(?:" + p + ")"
	}
	re, err := blregexp.Compile(strings.Join(parts, "|"))
	if err != nil {
		for _, pattern := range patterns {
			if _, patternErr := blregexp.Compile(pattern); patternErr != nil {
				return nil, fmt.Errorf("invalid regex pattern %q: %w", pattern, patternErr)
			}
		}
		return nil, fmt.Errorf("compile regex patterns: %w", err)
	}
	regexCache.Store(key, re)
	return re, nil
}

func getOrBuildTrie(terms []string) *ahocorasick.Matcher {
	if len(terms) == 0 {
		return nil
	}
	normalized := make([]string, len(terms))
	for i, term := range terms {
		normalized[i] = strings.ToLower(term)
	}
	key := sortedKey(normalized)
	if v, ok := acTrieCache.Load(key); ok {
		return v.(*ahocorasick.Matcher)
	}
	trie := ahocorasick.CompileStrings(normalized)
	acTrieCache.Store(key, trie)
	return trie
}

func matchesAny(values, patterns any) (bool, error) {
	re, err := joinedRegexForList(patterns)
	if err != nil || re == nil {
		return false, err
	}
	return anyString(values, re.MatchString), nil
}

func findMatch(s, pattern string) (string, error) {
	re, err := getOrCompileJoinedRegex([]string{pattern})
	if err != nil || re == nil {
		return "", err
	}
	return re.FindString(s), nil
}

func containsAny(values, terms any) bool {
	trie := getOrBuildTrie(toStringSlice(terms))
	return trie != nil && anyString(values, func(value string) bool {
		return len(trie.FindAllString(strings.ToLower(value))) > 0
	})
}

func startsWithAny(values, prefixes any) bool {
	prefixesList := toStringSlice(prefixes)
	return len(prefixesList) > 0 && anyString(values, func(value string) bool {
		for _, prefix := range prefixesList {
			if strings.HasPrefix(value, prefix) {
				return true
			}
		}
		return false
	})
}

func intersects(values, candidates any) bool {
	candidateList := toStringSlice(candidates)
	return len(candidateList) > 0 && anyString(values, func(value string) bool {
		for _, candidate := range candidateList {
			if value == candidate {
				return true
			}
		}
		return false
	})
}

// anyString applies match to a string or every string in a list. Returning
// false for mixed-type lists keeps malformed dynamic Expr values conservative.
func anyString(value any, match func(string) bool) bool {
	switch value := value.(type) {
	case string:
		return match(value)
	case []string:
		for _, item := range value {
			if match(item) {
				return true
			}
		}
	case []any:
		for _, item := range value {
			if _, ok := item.(string); !ok {
				return false
			}
		}
		for _, item := range value {
			if match(item.(string)) {
				return true
			}
		}
	}
	return false
}

func toStringSlice(v any) []string {
	switch ss := v.(type) {
	case []string:
		return ss
	case []any:
		out := make([]string, 0, len(ss))
		for _, elem := range ss {
			s, ok := elem.(string)
			if !ok {
				return nil
			}
			out = append(out, s)
		}
		return out
	default:
		return nil
	}
}

func shannonEntropy(s string) float64 {
	if len(s) == 0 {
		return 0
	}
	var freq [256]float64
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
	}
	n := float64(len(s))
	var h float64
	for _, f := range freq {
		if f > 0 {
			p := f / n
			h -= p * math.Log2(p)
		}
	}
	return h
}

var newlineReplacer = strings.NewReplacer("\n", "", "\r", "")

func (rt *runtimeBindings) tokenCounterInstance() *tokenizer.Counter {
	if rt.tokenCounter == nil {
		if rt.tokenCounterProvider == nil {
			return nil
		}
		rt.tokenCounter = rt.tokenCounterProvider()
	}
	return rt.tokenCounter
}

func (rt *runtimeBindings) failsTokenEfficiency(secret string) bool {
	counter := rt.tokenCounterInstance()
	return counter != nil && failsTokenEfficiency(counter, secret)
}

func (rt *runtimeBindings) tokenRatio(secret string) float64 {
	counter := rt.tokenCounterInstance()
	if counter == nil {
		return 0
	}
	_, ratio, _ := calculateTokenRatio(counter, secret)
	return ratio
}

func calculateTokenRatio(counter *tokenizer.Counter, secret string) (string, float64, bool) {
	analyzed := secret
	if len(analyzed) < 20 && strings.ContainsAny(analyzed, "\n\r") {
		analyzed = newlineReplacer.Replace(analyzed)
	}
	tokenCount := counter.Count(analyzed)
	if tokenCount == 0 {
		return analyzed, 0, false
	}
	return analyzed, float64(len(analyzed)) / float64(tokenCount), true
}

func failsTokenEfficiency(counter *tokenizer.Counter, secret string) bool {
	analyzed, ratio, ok := calculateTokenRatio(counter, secret)
	if !ok {
		return false
	}
	if len(words.HasMatchInList(analyzed, 5)) > 0 {
		return true
	}
	threshold := 2.5
	if len(analyzed) < 12 {
		threshold = 2.1
		if len(words.HasMatchInList(analyzed, 4)) == 0 {
			threshold = 2.5
		}
	}
	return ratio >= threshold
}
