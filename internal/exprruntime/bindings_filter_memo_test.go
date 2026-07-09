package exprruntime

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestMatchesAnyPrefixSlicesShareBackingArray guards the len check in the
// identity memo: two prefix slices of ONE backing array share the same
// backing-array pointer (the cache key), so without the length guard the
// second lookup would return the first slice's compiled artifact.
func TestMatchesAnyPrefixSlicesShareBackingArray(t *testing.T) {
	backing := []any{`^aaa$`, `^bbb$`}
	one := backing[:1] // matches only "aaa"
	two := backing[:2] // matches "aaa" and "bbb"

	// Both orders, repeatedly: whichever populates the cache first, the
	// other must not be served its artifact.
	for i := 0; i < 3; i++ {
		assert.False(t, matchesAny("bbb", one), "prefix slice [:1] must not match bbb (contaminated by [:2]?)")
		assert.True(t, matchesAny("bbb", two), "prefix slice [:2] must match bbb (contaminated by [:1]?)")
		assert.True(t, matchesAny("aaa", one))
		assert.True(t, matchesAny("aaa", two))
	}
}

// TestContainsAnyPrefixSlicesShareBackingArray: same invariant for the
// aho-corasick trie memo.
func TestContainsAnyPrefixSlicesShareBackingArray(t *testing.T) {
	backing := []any{"alpha", "beta"}
	one := backing[:1]
	two := backing[:2]

	for i := 0; i < 3; i++ {
		assert.False(t, containsAny("has beta inside", one), "prefix slice [:1] must not contain beta")
		assert.True(t, containsAny("has beta inside", two))
		assert.True(t, containsAny("has alpha inside", one))
		assert.True(t, containsAny("has alpha inside", two))
	}
}
