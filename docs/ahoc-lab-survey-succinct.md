# Succinct/Static Index Structures Survey — Raw Research Data

## (1) Double-Array Tries: daachorse / cedar / DAAC

### Memory Layout
- **12 bytes/state** [MEASURED — docs.rs/daachorse 3.0.2]. The compact double-array encodes base, check, and fail in 12 bytes per state.
- For 1889 states: **~22.7 KB** [ESTIMATED: 1889 × 12 = 22,668 bytes]. This is dramatically smaller than the current failTrans16 at 944KB.
- Paper: Kanda, Akabe, Oda, "Engineering faster double-array Aho-Corasick automata," SPE 53(6):1332–1361, 2023 (arXiv:2207.13870).

### Throughput Claims
- The paper abstract claims "Daachorse outperforms other AC-automaton implementations" [MEASURED — paper claim, no absolute MB/s in abstract].
- Constant-time state-to-state traversal [MEASURED — docs.rs].
- Linear-time over input length [MEASURED — docs.rs].

### Serial Dependency Analysis
- **The double-array does NOT eliminate serial loads.** Each transition requires: (1) load base[state], (2) compute base[state]+label, (3) load check[base[state]+label], (4) verify check==state. This is 2 dependent loads minimum per transition. The fail transition adds another dependent load. [ESTIMATED — structural analysis of double-array]
- However: at 22.7KB total, the **entire automaton fits in L1 cache** (typically 32-64KB on Graviton3). This means those dependent loads hit L1 (~4 cycles each on Graviton3) rather than L2 (~10-12 cycles). [ESTIMATED]
- Compared to current 944KB failTrans16 (L2-resident, ~10-12 cycle loads): dependent loads from L1 at ~4 cycles would be ~3x faster per transition step. [ESTIMATED]

### Key Insight
The win isn't eliminating serial dependency — it's shrinking the structure enough to fit L1 instead of L2. At 22.7KB for 1889 states, the entire automaton lives in L1 with room to spare.

---

## (2) Byte-Class Compression

### How It Works (from regex-automata and aho-corasick source)
- Byte equivalence classes map the 256 byte values to a smaller alphabet. Bytes that are interchangeable in all transitions share one class. [MEASURED — docs.rs/regex-automata ByteClasses]
- Example: for `[a-z]+`, only **4 classes** exist (from 257 including EOI). [MEASURED — regex-automata docs]
- Stride is rounded to next power-of-2 for shift-based indexing: `stride = 2^stride2` [MEASURED — aho-corasick dfa.rs source]

### Class Count Estimate for 366 Keywords
- The 366 lowercase keywords use: 26 lowercase letters + 10 digits + `_` + `-` + `.` = ~39 distinct byte values [ESTIMATED from "39 distinct first bytes, ~40 distinct bytes total" in the prompt context].
- With case-insensitive matching, uppercase variants map to same classes as lowercase.
- Estimated equivalence classes: **~42-48** (40 "interesting" bytes get their own classes, remaining 216 bytes collapse into a few "other" classes, plus EOI). [ESTIMATED]
- Next power-of-2 stride: **64** [ESTIMATED: ceil_pow2(~45) = 64]

### Class-Compressed AC Table Size
- Full DFA (premultiplied): 1889 states × 64 stride × 4 bytes = **484 KB** [ESTIMATED]. Still L2-resident but 2x smaller than 944KB.
- With 16-bit state IDs (sufficient for 1889 states): 1889 × 64 × 2 = **242 KB** [ESTIMATED]. Solidly L2-resident.
- With actual alphabet_len (~45, not padded): 1889 × 45 × 2 = **170 KB** [ESTIMATED — but loses shift-indexing advantage].

### The BurntSushi aho-corasick Contiguous NFA
- Most states are "one-transition" states (3 × u32 = 12 bytes) — "by far the most common state" [MEASURED — source comments]
- Dense states cost (2 + alphabet_len) × 4 bytes [MEASURED — source]
- For 1889 states mostly one-transition: estimated ~30-50 KB total [ESTIMATED]
- Single contiguous allocation; "an order of magnitude less heap memory than a noncontiguous NFA" [MEASURED — source comment]

### Comparison
| Representation | Size (1889 states) | Cache Level | Transition Cost |
|---|---|---|---|
| Current failTrans16 | 944 KB | L2 | 1 load (L2) |
| Byte-class DFA (u16) | ~242 KB | L2 | 1 load (L2) + class lookup |
| daachorse double-array | ~22.7 KB | **L1** | 2 dependent loads (L1) |
| Contiguous NFA (ONE states) | ~30-50 KB | L1/L2 boundary | 1-2 loads |

---

## (3) Minimal Perfect Hashing over 263 Anchor Trigrams

### Available Go Libraries

**cespare/mph** [MEASURED — GitHub/pkg.go.dev]:
- Algorithm: "Hash, displace, and compress" (CHD variant) with Murmur3
- API: `Build([]string) *Table`, `Lookup(s string) (uint32, bool)`
- Lookup: **~30 ns** on i7-8700K [MEASURED — benchmark in README]
- 27% faster than `map[string]uint32` [MEASURED]
- Build: ~18ms for 102k words [MEASURED]
- MIT, 75 stars, 1 release (v0.1.0), stable "done" library by Caleb Spare (well-known Go contributor)
- **String-native** — no need to pre-hash to uint64

**relab/bbhash** [MEASURED — GitHub]:
- Algorithm: BBHash (Limasset et al. 2017)
- API: `bbhash.New(keys []uint64, ...options)`, `bb.Find(key) uint64`
- Default gamma=2.0, ~3.06 bits/key at gamma=1.0 [MEASURED — BBHash C++ repo]
- Lookup: ~244 ns in C++ reference at 10M keys [MEASURED — BBHash C++ README]
- Go implementation, 148 commits, includes assembly hot paths
- Requires pre-hashing strings to uint64

**dgryski/go-boomphf** [MEASURED — pkg.go.dev]:
- Algorithm: BBHash
- API: `New(gamma float64, keys []uint64) *H`, `Query(k uint64) uint64`
- MIT, 73 stars, unmaintained since 2020, no go.mod, 0 importers
- Experimental/reference quality only

### MPHF Probe Cost Analysis (263 keys)

**BBHash query mechanics** [ESTIMATED from algorithm description]:
- At gamma=2: ~3.7 bits/key → total bitvector = 263 × 3.7 ≈ 122 bytes [ESTIMATED]
- Query traverses ~2-3 hash levels on average, each requiring one hash + one rank query (popcount over bitvector prefix) [ESTIMATED from BBHash algorithm]
- Each level: 1 hash computation + 1 cache line load for popcount = ~2-3 cache line touches total [ESTIMATED]

**CHD (cespare/mph) query mechanics** [ESTIMATED]:
- Two hash computations + 2 table lookups + 1 key comparison [ESTIMATED from algorithm description]
- For 263 keys: displacement table ~263 × 4 = ~1 KB, keys stored separately [ESTIMATED]
- Total structure: ~2-3 KB [ESTIMATED]

### Comparison with Current Bitmap+Table

| Approach | Total Size | Loads per Probe | L1 Residency |
|---|---|---|---|
| Current (8KB bitmap + 1KB table) | **9 KB** | 1 (bitmap) + 1-2 (table) = 2-3 | YES (both fit L1) |
| cespare/mph (CHD) | ~2-3 KB | 2 hash + 2 loads + 1 verify = ~3-4 | YES |
| BBHash (gamma=2) | ~0.5 KB bitvec + ~1 KB bucket | 2-3 hash + 2-3 rank = ~3-5 | YES |

### Verdict on MPHF vs Current
**The current bitmap approach is likely already near-optimal for this use case.** [ESTIMATED]
- The bitmap provides a single-load negative filter (97% of bytes hit this path). At 8KB it straddles L1/L2 boundary on some architectures, but on Graviton3 with 64KB L1D it fits entirely.
- MPHF would save ~6KB of L1 footprint but add computational cost (2+ hash computations vs 1 Fibonacci hash + 1 load).
- The key optimization opportunity is that 98.57% of positions are filtered by the bitmap in a single load. MPHF cannot improve this hot path.
- **Not recommended** unless L1 pressure from other structures becomes the binding constraint.

---

## (4) Factor Oracle / SBOM / Wu-Manber Sublinear Skips

### Wu-Manber Algorithm
- Original paper: Wu & Manber, "A fast algorithm for multi-pattern searching" (1994) [MEASURED — citation]
- Uses block-size B (typically 2-3) hash of text characters as shift index
- **Maximum shift = m - B + 1** where m = shortest pattern length [ESTIMATED — standard algorithm description]
- For minLen=3, B=2: max shift = **2** characters [ESTIMATED: 3-2+1=2]
- For minLen=3, B=3: max shift = **1** character (degenerates) [ESTIMATED: 3-3+1=1]
- With 366 patterns of minLen=3: the shift table (typically 2^16 entries for B=2) has **heavy collision** — most entries map to shift=0 or 1 because many 2-byte substrings of the 366 keywords are present [ESTIMATED]

### Expected Skip with Short Patterns
- With minLen=3: Wu-Manber max skip is **at most 2 bytes** [ESTIMATED from formula m-B+1 with B=2]
- This is **sublinear in name only** — in practice, scanning every 2 bytes with hash+lookup overhead is slower than byte-at-a-time AC for pattern sets of this size [ESTIMATED]
- The algorithm becomes genuinely useful only when minLen ≥ ~8-10 and pattern count is moderate (< 100) [ESTIMATED from algorithm analysis]

### If We Split Off Short Keywords
- If we restrict to keywords with len ≥ 6 (say 280 of 366): max shift = 6-2+1 = **4 bytes** [ESTIMATED]
- With 280 patterns, the 2^16 shift table still has significant collision; expected average shift likely ~2-3 bytes [ESTIMATED]
- Still not competitive with the current trigram scanner at 520 MB/s, which processes input in 16-byte NEON chunks [ESTIMATED comparison]

### Commentz-Walter
- Combines Boyer-Moore shifts with AC automaton [MEASURED — Wikipedia]
- Skip distance bounded by shortest pattern length [MEASURED — Wikipedia: "performance increased linearly as the shortest pattern within the pattern set increased"]
- With minLen=3: maximum shift of ~3 bytes, similar limitation [ESTIMATED]
- "Only outperforms Aho-Corasick for long patterns" [MEASURED — Wikipedia summary of empirical analysis]

### SBOM (Set Backward Oracle Matching)
- Builds a factor oracle of reversed patterns, scans backward through text [MEASURED — Wikipedia factor oracle article]
- Linear-time/linear-space construction [MEASURED — Wikipedia]
- Skip distances similarly bounded by minimum pattern length [ESTIMATED from algorithm structure]
- **No production Go implementation found** [MEASURED — search yielded no results]

### Verdict on Sublinear Skippers
**Not viable for this workload.** [ESTIMATED]
- MinLen=3 limits all Boyer-Moore-family skippers to max 2-3 byte shifts
- 366 patterns creates heavy hash collisions in shift tables
- The current trigram scanner (520 MB/s, 16-byte SIMD chunks, 1.43% hit rate) already processes ~16x more bytes per "step" than any sublinear skipper could with minLen=3
- These algorithms are designed for scenarios with few long patterns (e.g., virus signatures at 20+ bytes); they degenerate on many short patterns

---

## (5) Hit-Window Verification: Linear Merge vs Alternatives

### Current Scale Parameters
- ≤48 occurrences per pattern (cap)
- ≤10 keywords per rule (typical)
- Total: ≤480 positions to merge per rule invocation [ESTIMATED from prompt parameters]
- Positions are pre-sorted (come from sequential scan) [ESTIMATED from "position recorder" description]

### Interval Tree
- O(n log n) construction + O(log n + m) query [MEASURED — Wikipedia]
- For n=48, O(log 48) ≈ 6 comparisons per query [ESTIMATED]
- But construction overhead: allocating tree nodes, building balanced structure, pointer chasing [ESTIMATED]
- At n ≤ 50, **cache-unfriendly pointer structure dominates** [ESTIMATED]

### Linear Sorted Merge
- O(n) for pre-sorted input [MEASURED — algorithmic complexity]
- With ≤48 positions: scan through ≤48 entries = ~48 comparisons worst case [ESTIMATED]
- All data in a contiguous array → one or two cache lines on Graviton3 (64B lines, 48 × 4-byte positions = 192 bytes = 3 cache lines) [ESTIMATED]
- **No allocation, no pointer chasing, branch-predictable sequential access** [ESTIMATED]

### Implicit/Cache-Friendly Alternatives
- **Sorted array + binary search**: O(log n) per query, but n=48 means only ~6 comparisons saved vs sequential. Branch misprediction cost (~12 cycles on Graviton3) likely exceeds the saved comparisons [ESTIMATED]
- **Bitmap over line range**: If text is ≤64KB per chunk, a bitmap of "active windows" could be O(1) per position check. But this requires window start/end bookkeeping and doesn't save much at ≤48 occurrences [ESTIMATED]
- **Implicit treap / van Emde Boas**: Way over-engineered for n ≤ 50 [ESTIMATED]

### When Would Alternatives Pay?
- **n > ~500-1000**: interval tree starts winning due to O(log n) vs O(n) per lookup [ESTIMATED]
- **n > ~100**: binary search over sorted spans starts being worthwhile [ESTIMATED]
- At the current cap of 48: **linear merge is already optimal** [ESTIMATED — cache-friendly sequential access on 3 cache lines beats any pointer-based structure]

### Verdict
**Linear sorted merge is already optimal at this scale.** [ESTIMATED]
- Pre-sorted input eliminates the O(n log n) merge cost
- 48 positions fit in 3 cache lines — sequential scan is ~12 cycles total on Graviton3 with prefetch
- Any tree/fancy structure adds allocation + pointer-chasing overhead that exceeds the O(n) scan cost
- The only optimization worth considering: SIMD-accelerated window overlap check if the merge becomes a bottleneck (unlikely at current scale)

---

## Source Index

| # | Source | Content | Access |
|---|---|---|---|
| 1 | docs.rs/daachorse 3.0.2 | 12 bytes/state, constant-time traversal, linear search | Fetched |
| 2 | arXiv:2207.13870 (abstract) | Paper title/claims, SPE 2023 publication | Fetched |
| 3 | docs.rs/regex-automata (ByteClasses) | Equivalence class mechanics, [a-z]+ = 4 classes, stride2 | Fetched |
| 4 | aho-corasick src/dfa.rs (GitHub) | Premultiplied IDs, byte classes, stride, memory formula | Fetched |
| 5 | aho-corasick src/nfa/contiguous.rs | ONE/Dense/Sparse state encoding, 12B min, single allocation | Fetched |
| 6 | docs.rs/aho-corasick DFA struct | Dense transitions, precomputed fail, memory warning | Fetched |
| 7 | pkg.go.dev/github.com/cespare/mph | Build([]string), Lookup → (uint32, bool), 30ns, CHD+Murmur3 | Fetched |
| 8 | GitHub cespare/mph | 75 stars, v0.1.0, MIT, benchmarks: 30ns lookup, 18ms build 102k | Fetched |
| 9 | GitHub relab/bbhash | Go BBHash, gamma=2 default, 148 commits, uint64 keys | Fetched |
| 10 | GitHub rizkg/BBHash (C++) | 244ns query/10M keys, 3.06 bits/key at gamma=1, 10s build 100M | Fetched |
| 11 | arXiv:1702.03154 (BBHash abstract) | 3.7 bits/element, 10^10 keys in 7min, parallel construction | Fetched |
| 12 | pkg.go.dev/dgryski/go-boomphf | Go BBHash, Gamma=2, uint64 keys, unmaintained since 2020 | Fetched |
| 13 | Wikipedia: Factor oracle | Linear construction, substring search, no SBOM detail | Fetched |
| 14 | Wikipedia: Commentz-Walter | Shift bounded by min pattern length, worse than AC for short patterns | Fetched |
| 15 | Wikipedia: Interval tree | O(n log n) build, O(log n + m) query, no small-n guidance | Fetched |
| 16 | Wikipedia: String-searching algorithm | Commentz-Walter Θ(Mn) worst, Set-BOM listed, no Wu-Manber | Fetched |
| 17 | GitHub petar-dambovaliev/aho-corasick | Go AC, BurntSushi-inspired, DFA+NFA, 20x faster than Cloudflare | Fetched |
| 18 | GitHub cloudflare/ahocorasick | Go AC, 724 stars, BSD-3, minimal single-file | Fetched |
| 19 | GitHub flier/gohs | Go Hyperscan binding, v1.2.3 Dec 2024, 317 stars, Vectorscan for ARM | Fetched |