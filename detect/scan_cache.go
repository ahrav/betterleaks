package detect

import (
	"hash/fnv"
	"sync"
	"sync/atomic"
)

// scanCache memoizes per-rule regex match results keyed by fragment content.
// Git history scans see the same hunk content many times (cherry-picks,
// reverts, vendored-file churn, decode passes), and FindAllStringIndex is a
// pure function of (pattern, content), so results can be reused safely.
//
// Only fragments of at least scanCacheMinLen bytes are cached: small
// fragments are cheap to rescan and would dominate the entry count.
const (
	scanCacheMinLen     = 1024
	scanCacheMaxEntries = 4 << 20
	scanCacheShards     = 64
)

type scanCacheKey struct {
	h1, h2 uint64
	ruleID string
}

type scanCacheShard struct {
	mu sync.Mutex
	m  map[scanCacheKey][][]int
}

type scanCache struct {
	shards  [scanCacheShards]scanCacheShard
	entries atomic.Int64
}

func newScanCache() *scanCache {
	c := &scanCache{}
	for i := range c.shards {
		c.shards[i].m = make(map[scanCacheKey][][]int)
	}
	return c
}

// hashContent returns a 128-bit FNV-1a hash of s. 128 bits keeps the
// collision probability negligible even at hundreds of millions of unique
// fragments, which matters because a collision would silently corrupt
// findings.
func hashContent(s string) (uint64, uint64) {
	h := fnv.New128a()
	_, _ = h.Write([]byte(s))
	var sum [16]byte
	h.Sum(sum[:0])
	h1 := uint64(sum[0])<<56 | uint64(sum[1])<<48 | uint64(sum[2])<<40 | uint64(sum[3])<<32 |
		uint64(sum[4])<<24 | uint64(sum[5])<<16 | uint64(sum[6])<<8 | uint64(sum[7])
	h2 := uint64(sum[8])<<56 | uint64(sum[9])<<48 | uint64(sum[10])<<40 | uint64(sum[11])<<32 |
		uint64(sum[12])<<24 | uint64(sum[13])<<16 | uint64(sum[14])<<8 | uint64(sum[15])
	return h1, h2
}

func (c *scanCache) shard(k scanCacheKey) *scanCacheShard {
	return &c.shards[(k.h1^uint64(len(k.ruleID)))%scanCacheShards]
}

// get returns a defensive copy of the cached matches (callers mutate match
// index slices during finding construction).
func (c *scanCache) get(k scanCacheKey) ([][]int, bool) {
	s := c.shard(k)
	s.mu.Lock()
	v, ok := s.m[k]
	s.mu.Unlock()
	if !ok {
		return nil, false
	}
	if len(v) == 0 {
		return nil, true
	}
	out := make([][]int, len(v))
	for i, m := range v {
		cp := make([]int, len(m))
		copy(cp, m)
		out[i] = cp
	}
	return out, true
}

// put stores a defensive copy of matches. Insertion stops once the global
// entry cap is reached; existing entries keep serving hits.
func (c *scanCache) put(k scanCacheKey, matches [][]int) {
	if c.entries.Load() >= scanCacheMaxEntries {
		return
	}
	var v [][]int
	if len(matches) > 0 {
		v = make([][]int, len(matches))
		for i, m := range matches {
			cp := make([]int, len(m))
			copy(cp, m)
			v[i] = cp
		}
	}
	s := c.shard(k)
	s.mu.Lock()
	if _, exists := s.m[k]; !exists {
		s.m[k] = v
		c.entries.Add(1)
	}
	s.mu.Unlock()
}
