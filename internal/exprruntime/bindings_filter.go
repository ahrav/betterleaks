package exprruntime

import (
	"math"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"github.com/betterleaks/betterleaks/internal/words"
	blregexp "github.com/betterleaks/betterleaks/regexp"
	tiktoken "github.com/pkoukk/tiktoken-go"
	ahocorasick "github.com/rrethy/ahocorasick"
)

var (
	regexCache  sync.Map // string -> *blregexp.Regexp
	acTrieCache sync.Map // string -> *ahocorasick.Matcher

	// Pattern lists reaching matchesAny/containsAny are almost always expr
	// VM constants: the same []any backing array is passed on every eval.
	// Memoizing by that array's identity skips the per-call []string
	// conversion and joined-key construction entirely.
	regexByListID  sync.Map // unsafe.Pointer -> listCacheEntry[*blregexp.Regexp]
	acTrieByListID sync.Map // unsafe.Pointer -> listCacheEntry[*ahocorasick.Matcher]
)

// listCacheEntry pins the cached list so its backing array can never be
// freed and its address reused by a different list, which is what makes
// keying by backing-array pointer sound. The length guards against distinct
// prefix-slices of one array (same base, different len).
type listCacheEntry[T any] struct {
	list     []any
	compiled T
}

// listIdentity returns a stable identity for a non-empty []any: the pointer
// to its backing array.
func listIdentity(v any) (unsafe.Pointer, bool) {
	ss, ok := v.([]any)
	if !ok || len(ss) == 0 {
		return nil, false
	}
	return unsafe.Pointer(unsafe.SliceData(ss)), true
}

func filterNamespace(rt *runtimeBindings) map[string]any {
	return map[string]any{
		"matchesAny":           matchesAny,
		"containsAny":          containsAny,
		"entropy":              shannonEntropy,
		"failsTokenEfficiency": rt.failsTokenEfficiency,
	}
}

func orderedKey(ss []string) string { return strings.Join(ss, "\x00") }

func sortedKey(ss []string) string {
	cp := make([]string, len(ss))
	copy(cp, ss)
	sort.Strings(cp)
	return strings.Join(cp, "\x00")
}

func getOrCompileJoinedRegex(patterns []string) *blregexp.Regexp {
	if len(patterns) == 0 {
		return nil
	}
	key := orderedKey(patterns)
	if v, ok := regexCache.Load(key); ok {
		return v.(*blregexp.Regexp)
	}
	parts := make([]string, len(patterns))
	for i, p := range patterns {
		parts[i] = "(?:" + p + ")"
	}
	re, err := blregexp.Compile(strings.Join(parts, "|"))
	if err != nil {
		return nil
	}
	regexCache.Store(key, re)
	return re
}

func getOrBuildTrie(terms []string) *ahocorasick.Matcher {
	if len(terms) == 0 {
		return nil
	}
	key := sortedKey(terms)
	if v, ok := acTrieCache.Load(key); ok {
		return v.(*ahocorasick.Matcher)
	}
	trie := ahocorasick.CompileStrings(terms)
	acTrieCache.Store(key, trie)
	return trie
}

func matchesAny(s string, patterns any) bool {
	if id, ok := listIdentity(patterns); ok {
		if v, hit := regexByListID.Load(id); hit {
			e := v.(listCacheEntry[*blregexp.Regexp])
			if len(e.list) == len(patterns.([]any)) {
				return e.compiled != nil && e.compiled.MatchString(s)
			}
		}
		re := getOrCompileJoinedRegex(toStringSlice(patterns))
		regexByListID.Store(id, listCacheEntry[*blregexp.Regexp]{list: patterns.([]any), compiled: re})
		return re != nil && re.MatchString(s)
	}
	re := getOrCompileJoinedRegex(toStringSlice(patterns))
	return re != nil && re.MatchString(s)
}

func containsAny(s string, terms any) bool {
	var trie *ahocorasick.Matcher
	if id, ok := listIdentity(terms); ok {
		if v, hit := acTrieByListID.Load(id); hit {
			e := v.(listCacheEntry[*ahocorasick.Matcher])
			if len(e.list) == len(terms.([]any)) {
				trie = e.compiled
				return trie != nil && len(trie.FindAllString(strings.ToLower(s))) > 0
			}
		}
		trie = getOrBuildTrie(toStringSlice(terms))
		acTrieByListID.Store(id, listCacheEntry[*ahocorasick.Matcher]{list: terms.([]any), compiled: trie})
	} else {
		trie = getOrBuildTrie(toStringSlice(terms))
	}
	return trie != nil && len(trie.FindAllString(strings.ToLower(s))) > 0
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

func (rt *runtimeBindings) failsTokenEfficiency(secret string) bool {
	// rt may be shared by concurrent evals; never write to it here. The
	// provider memoizes internally (sync.Once), so skipping the local cache
	// costs one indirect call.
	tke := rt.tokenizer
	if tke == nil {
		if rt.tokenizerProvider == nil {
			return false
		}
		tke = rt.tokenizerProvider()
		if tke == nil {
			return false
		}
	}
	return failsTokenEfficiency(tke, secret)
}

func failsTokenEfficiency(tke *tiktoken.Tiktoken, secret string) bool {
	analyzed := secret
	if len(analyzed) < 20 && strings.ContainsAny(analyzed, "\n\r") {
		analyzed = newlineReplacer.Replace(analyzed)
	}
	tokens := tke.Encode(analyzed, nil, nil)
	if len(tokens) == 0 {
		return false
	}
	if words.HasAnyMatchInList(analyzed, 5) {
		return true
	}
	threshold := 2.5
	if len(analyzed) < 12 {
		threshold = 2.1
		if !words.HasAnyMatchInList(analyzed, 4) {
			threshold = 2.5
		}
	}
	return float64(len(analyzed))/float64(len(tokens)) >= threshold
}
