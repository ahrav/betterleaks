package detect

import "sync"

// trigramPrefilter finds which keywords occur in a fragment without an
// Aho-Corasick state machine. The observation: detectFragment only needs the
// DISTINCT set of keywords present (to build the candidate rule set), not
// match positions or counts, and every keyword is >= 3 bytes. So a keyword
// occurrence implies each of its trigrams appears at the corresponding
// offset; each keyword is indexed under one ANCHOR trigram — its rarest
// window per the corpus frequency table (commonTrigrams) — so common
// prefixes ("---" of -----BEGIN, "con", "int") don't trigger verification
// at every English-word occurrence.
//
// The scan tests each position's 3-gram against an 8KB bitmap (L1-resident)
// of hashed keyword prefixes. Only on a bitmap hit does it consult an exact
// open-addressed table keyed by the 24-bit trigram and verify the bucket's
// keywords with anchored byte comparisons. Measured on real source corpora
// the keyword 3-gram density is ~4-6% of positions, so >94% of bytes cost
// one shift/or, one multiply, one L1 load, and one predictable branch — with
// no serial dependent-load chain through a multi-hundred-KB transition
// table, unlike the Aho-Corasick walk.
//
// Because common keywords ("api", "key", "token") occur thousands of times
// per fragment, the scan carries pooled per-scan state: found patterns are
// never re-verified, and once every keyword behind a bitmap bit has been
// found the verify path short-circuits after one counter load. State reset
// is O(1) via epoch stamping — no per-fragment copies or clears, so small
// fragments pay nothing for it.
type trigramPrefilter struct {
	// bitmap has bit h set iff some keyword's first trigram hashes to h.
	bitmap [1 << 13]byte // 65536 bits
	// table is open-addressed on trigramHash; entries resolve hash
	// collisions with the exact trigram key. tableMask sizes the probe.
	table     []trigramSlot
	tableMask uint32
	// entries holds keyword bodies grouped by first trigram; a slot's
	// bucket is entries[start:end].
	entries []trigramEntry
	// initialRemaining[bitIdx] counts the keyword entries across all
	// slots whose hash owns bitmap bit bitIdx-1 (bitIdx is 1-based; 0 in
	// a slot means unassigned and never occurs on used slots).
	initialRemaining []uint16
	// patterns is the number of keywords (pattern index space).
	patterns int

	statePool sync.Pool // of *trigramScanState
}

type trigramSlot struct {
	tri   uint32 // 24-bit trigram key | trigramSlotUsed
	start uint32 // bucket start in entries
	end   uint32 // bucket end in entries
	// bitIdx is the 1-based dense index of the slot's bitmap bit; slots
	// whose trigram hashes collide on one bit share a bitIdx.
	bitIdx uint16
}

const trigramSlotUsed = 1 << 31

type trigramEntry struct {
	kw      string // full keyword (lowercase)
	pattern uint32 // pattern index aligned with prefilterRuleRanks
	// offset is the anchor trigram's position within kw: a scan hit at
	// input position i verifies kw anchored at i-offset.
	offset int32
}

// trigramScanState is the pooled per-scan mutable state. All entries are
// epoch-stamped: acquireState bumps epoch, implicitly invalidating every
// slot, so reset costs O(1) regardless of fragment size.
type trigramScanState struct {
	epoch uint32
	// seenEpoch[pattern] == epoch marks the pattern found this scan.
	seenEpoch []uint32
	// remaining[bitIdx] counts unfound keywords behind a bitmap bit;
	// lazily re-seeded from initialRemaining when remEpoch is stale.
	remaining []uint16
	remEpoch  []uint32
}

// seen reports whether pattern was already marked this scan.
func (st *trigramScanState) seen(pattern uint32) bool {
	return st.seenEpoch[pattern] == st.epoch
}

// bitDone reports whether every keyword behind bitIdx has been found,
// lazily seeding the counter on first touch this scan.
func (st *trigramScanState) bitDone(bitIdx uint16, initial []uint16) bool {
	if st.remEpoch[bitIdx] != st.epoch {
		st.remEpoch[bitIdx] = st.epoch
		st.remaining[bitIdx] = initial[bitIdx]
	}
	return st.remaining[bitIdx] == 0
}

// trigramHash folds a 24-bit trigram to 16 bits. Fibonacci hashing spreads
// the low entropy of ASCII text across the table.
func trigramHash(tri uint32) uint32 {
	return (tri * 2654435761) >> 16 & 0xFFFF
}

// newTrigramPrefilter builds the scanner, or returns nil if any keyword is
// shorter than 3 bytes (the caller falls back to Aho-Corasick).
func newTrigramPrefilter(keywords []string) *trigramPrefilter {
	for _, kw := range keywords {
		if len(kw) < 3 {
			return nil
		}
	}
	p := &trigramPrefilter{patterns: len(keywords)}

	// Rank common trigrams: rank 0 = most frequent in source corpora.
	// Trigrams absent from the table are rarer than everything in it.
	triRank := make(map[uint32]int, len(commonTrigrams))
	for rank, tri := range commonTrigrams {
		triRank[tri] = rank
	}

	// Group keywords by ANCHOR trigram (little-endian key b0|b1<<8|b2<<16):
	// the window with the highest rank (rarest). Ties break toward the
	// earliest offset for cheap left-anchored verification.
	byTri := make(map[uint32][]trigramEntry, len(keywords))
	for i, kw := range keywords {
		bestOff, bestRank := 0, -1
		for off := 0; off+3 <= len(kw); off++ {
			tri := uint32(kw[off]) | uint32(kw[off+1])<<8 | uint32(kw[off+2])<<16
			rank, ok := triRank[tri]
			if !ok {
				rank = len(commonTrigrams)
			}
			if rank > bestRank {
				bestRank, bestOff = rank, off
			}
		}
		tri := uint32(kw[bestOff]) | uint32(kw[bestOff+1])<<8 | uint32(kw[bestOff+2])<<16
		byTri[tri] = append(byTri[tri], trigramEntry{kw: kw, pattern: uint32(i), offset: int32(bestOff)})
	}

	// Size the exact table to a power of two at load factor <= 1/2.
	size := uint32(64)
	for size < uint32(len(byTri))*2 {
		size *= 2
	}
	p.table = make([]trigramSlot, size)
	p.tableMask = size - 1

	bitIdxByHash := make(map[uint32]uint16, len(byTri))
	p.initialRemaining = append(p.initialRemaining, 0) // index 0 unused
	for tri, bucket := range byTri {
		h := trigramHash(tri)
		p.bitmap[h>>3] |= 1 << (h & 7)

		bitIdx, ok := bitIdxByHash[h]
		if !ok {
			bitIdx = uint16(len(p.initialRemaining))
			p.initialRemaining = append(p.initialRemaining, 0)
			bitIdxByHash[h] = bitIdx
		}
		p.initialRemaining[bitIdx] += uint16(len(bucket))

		start := uint32(len(p.entries))
		p.entries = append(p.entries, bucket...)
		end := uint32(len(p.entries))

		slot := h & p.tableMask
		for p.table[slot].tri&trigramSlotUsed != 0 {
			slot = (slot + 1) & p.tableMask
		}
		p.table[slot] = trigramSlot{tri: tri | trigramSlotUsed, start: start, end: end, bitIdx: bitIdx}
	}

	p.statePool = sync.Pool{New: func() any {
		return &trigramScanState{
			seenEpoch: make([]uint32, p.patterns),
			remaining: make([]uint16, len(p.initialRemaining)),
			remEpoch:  make([]uint32, len(p.initialRemaining)),
		}
	}}
	return p
}

// acquireState returns per-scan state; epoch bump invalidates prior scans.
func (p *trigramPrefilter) acquireState() *trigramScanState {
	st := p.statePool.Get().(*trigramScanState)
	st.epoch++
	if st.epoch == 0 {
		// Epoch wrapped: stale stamps could alias. Hard-reset once per
		// 2^32 scans.
		clear(st.seenEpoch)
		clear(st.remEpoch)
		st.epoch = 1
	}
	return st
}

func (p *trigramPrefilter) releaseState(st *trigramScanState) {
	p.statePool.Put(st)
}

// collectPatterns scans input (already ASCII-lowercased) and marks each
// distinct keyword found via mark(pattern), called exactly once per
// distinct pattern.
func (p *trigramPrefilter) collectPatterns(input []byte, mark func(pattern uint32)) {
	if len(input) < 3 {
		return
	}
	st := p.acquireState()
	tri := uint32(input[0]) | uint32(input[1])<<8
	end := len(input) - 2
	for i := 0; i < end; i++ {
		tri |= uint32(input[i+2]) << 16
		h := trigramHash(tri)
		if p.bitmap[h>>3]&(1<<(h&7)) != 0 {
			p.verify(input, i, tri, st, mark)
		}
		tri >>= 8
	}
	p.releaseState(st)
}

// verify resolves a bitmap hit: find the exact-trigram bucket and compare
// its keywords anchored at position i. Found patterns are marked once;
// when a bit's keyword set completes, the bit is cleared so subsequent
// occurrences take the miss path.
func (p *trigramPrefilter) verify(input []byte, i int, tri uint32, st *trigramScanState, mark func(pattern uint32)) {
	slot := trigramHash(tri) & p.tableMask
	for {
		s := p.table[slot]
		if s.tri&trigramSlotUsed == 0 {
			return // hash collision from a different trigram
		}
		if s.tri&^trigramSlotUsed == tri {
			if st.bitDone(s.bitIdx, p.initialRemaining) {
				return
			}
			for _, e := range p.entries[s.start:s.end] {
				start := i - int(e.offset)
				if !st.seen(e.pattern) && start >= 0 && len(input)-start >= len(e.kw) &&
					string(input[start:start+len(e.kw)]) == e.kw {
					st.seenEpoch[e.pattern] = st.epoch
					mark(e.pattern)
					st.remaining[s.bitIdx]--
				}
			}
			return
		}
		slot = (slot + 1) & p.tableMask
	}
}

// verifySrc is verify for the fused lowering scan: anchored keyword
// comparison reading src through the lowerByte table, since dst is only
// lowered up to the current window.
func (p *trigramPrefilter) verifySrc(src string, i int, tri uint32, st *trigramScanState, mark func(pattern uint32)) {
	slot := trigramHash(tri) & p.tableMask
	for {
		s := p.table[slot]
		if s.tri&trigramSlotUsed == 0 {
			return
		}
		if s.tri&^trigramSlotUsed == tri {
			if st.bitDone(s.bitIdx, p.initialRemaining) {
				return
			}
		bucket:
			for _, e := range p.entries[s.start:s.end] {
				start := i - int(e.offset)
				if st.seen(e.pattern) || start < 0 || len(src)-start < len(e.kw) {
					continue
				}
				// The anchor window [i,i+3) is already known equal;
				// compare the rest through the lowercase table.
				for j := 0; j < len(e.kw); j++ {
					if j >= int(e.offset) && j < int(e.offset)+3 {
						continue
					}
					if lowerByte[src[start+j]] != e.kw[j] {
						continue bucket
					}
				}
				st.seenEpoch[e.pattern] = st.epoch
				mark(e.pattern)
				st.remaining[s.bitIdx]--
			}
			return
		}
		slot = (slot + 1) & p.tableMask
	}
}

// collectPatternsLowering is collectPatterns fused with ASCII lowercasing:
// it reads raw bytes from src, writes the lowercased copy into dst
// (len(dst) == len(src)), and marks keywords found in the lowered text.
// One pass over the fragment replaces the separate asciiLower pass plus
// the scan pass, halving prefilter-stage memory traffic.
//
// The scan works a 64-bit word at a time: SWAR-lowercase eight bytes,
// store them, then test the eight trigram windows that start inside the
// word. With little-endian trigram keys each window is one shift and one
// mask of the word (the last two windows borrow bytes from the next
// word), so the probes are independent and pipeline freely — no serial
// dependency chain like a rolling hash or an automaton walk.
func (p *trigramPrefilter) collectPatternsLowering(dst []byte, src string, mark func(pattern uint32)) {
	n := len(src)
	if n < 8 {
		for i := 0; i < n; i++ {
			dst[i] = lowerByte[src[i]]
		}
		if n < 3 {
			return
		}
		st := p.acquireState()
		for i := 0; i+3 <= n; i++ {
			tri := uint32(dst[i]) | uint32(dst[i+1])<<8 | uint32(dst[i+2])<<16
			h := trigramHash(tri)
			if p.bitmap[h>>3]&(1<<(h&7)) != 0 {
				p.verifySrc(src, i, tri, st, mark)
			}
		}
		p.releaseState(st)
		return
	}

	st := p.acquireState()

	// Lowercase the first word.
	w := swarLower(leUint64(src))
	putLEUint64(dst, w)

	i := 0
	for i+16 <= n {
		next := swarLower(leUint64(src[i+8:]))
		putLEUint64(dst[i+8:], next)
		p.testWindows(src, i, w, next, st, mark)
		w = next
		i += 8
	}
	// Tail: lowercase remaining bytes, then scan remaining windows
	// byte-wise (at most 15 window starts).
	for j := i + 8; j < n; j++ {
		dst[j] = lowerByte[src[j]]
	}
	for ; i+3 <= n; i++ {
		tri := uint32(dst[i]) | uint32(dst[i+1])<<8 | uint32(dst[i+2])<<16
		h := trigramHash(tri)
		if p.bitmap[h>>3]&(1<<(h&7)) != 0 {
			p.verifySrc(src, i, tri, st, mark)
		}
	}
	p.releaseState(st)
}

// testWindows tests the eight trigram windows starting at positions
// base..base+7, where w holds lowered bytes [base,base+8) and next holds
// lowered bytes [base+8,base+16), both little-endian. Manually unrolled:
// all eight hashes are computed up front as one straight-line dependency-
// free block (the multiplies pipeline), then eight predictable-untaken
// branch tests follow. bitmap is passed by pointer so the compiler hoists
// the base address once.
func (p *trigramPrefilter) testWindows(src string, base int, w, next uint64, st *trigramScanState, mark func(pattern uint32)) {
	bm := &p.bitmap
	hi := w>>48 | next<<16

	t0 := uint32(w) & 0xFFFFFF
	t1 := uint32(w>>8) & 0xFFFFFF
	t2 := uint32(w>>16) & 0xFFFFFF
	t3 := uint32(w>>24) & 0xFFFFFF
	t4 := uint32(w>>32) & 0xFFFFFF
	t5 := uint32(w>>40) & 0xFFFFFF
	t6 := uint32(hi) & 0xFFFFFF
	t7 := uint32(hi>>8) & 0xFFFFFF

	h0 := trigramHash(t0)
	h1 := trigramHash(t1)
	h2 := trigramHash(t2)
	h3 := trigramHash(t3)
	h4 := trigramHash(t4)
	h5 := trigramHash(t5)
	h6 := trigramHash(t6)
	h7 := trigramHash(t7)

	if bm[h0>>3]&(1<<(h0&7)) != 0 {
		p.verifySrc(src, base+0, t0, st, mark)
	}
	if bm[h1>>3]&(1<<(h1&7)) != 0 {
		p.verifySrc(src, base+1, t1, st, mark)
	}
	if bm[h2>>3]&(1<<(h2&7)) != 0 {
		p.verifySrc(src, base+2, t2, st, mark)
	}
	if bm[h3>>3]&(1<<(h3&7)) != 0 {
		p.verifySrc(src, base+3, t3, st, mark)
	}
	if bm[h4>>3]&(1<<(h4&7)) != 0 {
		p.verifySrc(src, base+4, t4, st, mark)
	}
	if bm[h5>>3]&(1<<(h5&7)) != 0 {
		p.verifySrc(src, base+5, t5, st, mark)
	}
	if bm[h6>>3]&(1<<(h6&7)) != 0 {
		p.verifySrc(src, base+6, t6, st, mark)
	}
	if bm[h7>>3]&(1<<(h7&7)) != 0 {
		p.verifySrc(src, base+7, t7, st, mark)
	}
}

// swarLower ASCII-lowercases eight bytes at once; same transformation as
// asciiLower (see utils.go for the SWAR lane-mask derivation and the
// exhaustive equivalence proof in the package tests).
func swarLower(x uint64) uint64 {
	notHigh := ^x & swarHigh
	gt40 := ((x & swarLow7) + (swarLow7 - swarOnes*0x40)) & swarHigh
	gt5A := ((x & swarLow7) + (swarLow7 - swarOnes*0x5A)) & swarHigh
	return x + (gt40 &^ gt5A & notHigh >> 2)
}

// lowerByte maps each byte to its ASCII-lowercase form ('A'-'Z' gain 0x20,
// everything else unchanged), matching asciiLower semantics exactly.
var lowerByte = func() (t [256]byte) {
	for i := range t {
		b := byte(i)
		if b >= 'A' && b <= 'Z' {
			b += 0x20
		}
		t[i] = b
	}
	return
}()
