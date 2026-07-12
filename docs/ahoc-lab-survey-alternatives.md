## Structural Alternatives Survey — Raw Research Data

---

### (a) Batch Regex over Stitched Windows

**RE2 null-byte semantics (verified):** RE2 and Go's `regexp` (which is RE2-semantics) treat `\x00` as an ordinary byte. Confirmed experimentally:
- `dot` (both with and without `s` flag) matches `\x00`
- `foo.bar` matches `foo\x00bar` even without `(?s)`
- `abc.*def` matches across embedded `\x00` bytes
- `[^\n]` matches `\x00`

**Implication for separator choice:** You CANNOT use `\x00` as a safe separator because RE2 patterns will happily match across it. The only sound separator is a byte sequence that the specific rule regex provably cannot match — which requires per-pattern analysis of the regex grammar. In practice you would need either:
1. A multi-byte sentinel guaranteed absent from all windows (e.g., a 4-byte magic unlikely in real data — fragile)
2. A newline (`\n`) separator — works only if patterns use `(?-s)` mode (dot doesn't match newline) AND don't contain explicit `\n` or `\s` matching
3. Per-regex static analysis of what bytes the pattern cannot consume (expensive to compute generally)

**Who does batch-stitching in production:**
- **Hyperscan**: Does NOT stitch user data. Its scratch space is internal DFA state, not concatenated input. It scans the single input buffer against all patterns simultaneously via its compiled multi-pattern DFA. The "scratch" is temporary working memory for the automata, not concatenated windows.
- **CodeQL / Semgrep**: No evidence of input-stitching for regex. Semgrep delegates regex matching to its core engine per-rule.
- **The wazero per-call overhead**: The wasilibs/go-re2 library wraps C++ RE2 compiled to WASM. Per the RATIONALE.md, all memory allocated in the C++ side is actually allocated within the WASM module's linear memory (which the Go GC manages). Each `FindAllStringIndex` call must copy the input string into WASM linear memory and copy results back. Batching N windows into one call amortizes this copy + JIT-entry overhead from N copies to 1.

**Win regime:** When a rule has many windows per fragment (say 5-10+), the per-call wasm-memory-copy and JIT entry overhead dominates. Concatenating with a carefully chosen separator into one FindAll call would amortize this.
**Loss regime:** When most rules have 1-2 windows per fragment (the common case given 1.6 avg candidate rules), the concatenation overhead (malloc, copy, separator logic) exceeds the per-call savings.
**Go implementation cost:** Medium — need per-regex separator analysis or a universal sentinel strategy. The separator correctness problem is the killer.
**Verdict: Marginal.** The separator soundness problem makes this fragile for arbitrary patterns. Could work for a curated subset of patterns proven not to match the sentinel, but the engineering cost for 331 rules is high relative to the ~1.6 avg candidates/fragment baseline.

---

### (b) Line-index Precomputation

**ripgrep's line_buffer approach (verified from source):** ripgrep does NOT precompute a full newline-offset index. Its `LineBuffer` is chunk-oriented: it reads 64KB chunks, does a single `rfind_byte('\n')` backwards on new bytes to find the last complete line boundary, and exposes the complete-lines portion. Individual line splitting is left to the consumer. There is no upfront `memchr` pass building a full line-offset table.

**What it costs/saves at 1MB:** A single `memchr('\n')` pass over 1MB on Graviton3 (NEON-accelerated via Go's `bytes.IndexByte` or equivalent): roughly 1MB / ~16GB/s memchr throughput = ~60 microseconds. ESTIMATED. The resulting index would be a `[]int` of maybe 10k-50k entries (typical code has 20-50 bytes/line), costing 80-400KB of allocation.

**Win regime:** If your pipeline does repeated line-number lookups (e.g., for match context, `CurrentLine()` calls), a precomputed index turns each lookup from O(n) scan to O(log n) bisect. This matters when you have many matches per fragment.
**Loss regime:** Fragments with 0-1 matches (the overwhelmingly common case after prefiltering) never use the index — the 60µs scan is pure waste. Also, the allocation pressure of a 50k-entry slice per fragment would be significant at throughput (GC pressure).
**Go implementation cost:** Low — trivial to implement. One `bytes.Count` + preallocated slice.
**Verdict: Not worthwhile as a per-fragment precomputation.** The lazy approach (compute only on first match, as already done with `newlineComputed` flag in detect.go:974) is correct. Could be useful only if match density is high AND context lookups are frequent — but the profile shows this is not a bottleneck.

---

### (c) Per-fragment Rule Scheduling (cost/probability ordering)

**Prior art in IDS:**
- **Suricata**: Uses Multi-Pattern Matching (MPM) with Aho-Corasick or Hyperscan as a "prefilter". Signature groups (SGH) cluster rules by their `fast_pattern` keyword — the longest/most-selective literal in each rule. Once MPM reports which literals hit, only those rules' remaining conditions are evaluated. Suricata's `detect.profile` controls how aggressively rules are merged into groups. The ordering within a group is NOT adaptive/learned — it's static based on pattern properties.
- **Snort**: Uses "fast_pattern" selection — each rule nominates its most-selective content match as the prefilter pattern. Evaluation order within a group is determined at compile time by pattern specificity (longest literal, earliest in rule, etc.), not learned at runtime.
- **Database query optimizers**: Adaptive predicate ordering (e.g., CockroachDB's adaptive query optimization, PostgreSQL's JIT-compiled expression evaluation) reorders filter predicates by observed selectivity. The classic approach: order predicates by `(1 - selectivity) / cost` descending (most-rejecting-per-unit-cost first).

**Application to betterleaks:** The existing `rulesBySpecificity` ordering (detect.go:151, "ruleRank") already provides a static specificity-based ordering. Making this adaptive would mean:
1. Track per-rule gate-rejection rate from the `ruleGateStats` counters
2. Re-sort candidate rules per fragment by `(rejection_probability / gate_cost)` descending
3. Short-circuit: once a rule passes all gates and full-regex, skip remaining rules if single-match semantics apply

**Win regime:** Corpora where a few rules consistently pass gates but fail full-regex (wasting regex time). If you can cheaply reject 80% of candidates before the expensive ones run, you save their regex cost.
**Loss regime:** With only 1.6 avg candidate rules per fragment, there's almost nothing to reorder. Most fragments see 0-2 candidate rules — sorting 2 items provides zero benefit. Also, the gate system already rejects most candidates before full regex.
**Go implementation cost:** Low — maintain exponentially-weighted rejection rates, sort by rate before evaluation.
**Verdict: This loses.** At 1.6 avg candidates/fragment, the scheduling overhead exceeds any reordering benefit. The existing static specificity ordering is already close to optimal for the common case. Would only help if candidate density were 10x higher (e.g., 15+ rules per fragment), which the trigram prefilter prevents.

---

### (d) Parallel bzip2 Decode in Pure Go

**Existing Go library: `cosnicolaou/pbzip2` (verified, production-quality):**
- Pure Go parallel bzip2 decompressor. API: `bzip2.NewReader(input)` — drop-in io.Reader.
- Algorithm: scans for bzip2 block magic numbers (bit-aligned, 6-byte patterns `0x314159265359`), decompresses each block concurrently, reassembles into ordered stream.
- Block boundary discovery: uses three lookup tables (256-entry hash + two 32-bit pattern tables for all 8 bit-shift positions of the magic). Handles false positives by merging blocks that fail to decompress.
- Performance claim: "8 times faster than the serial version on an 8 core machine" (tested on wikidata dumps). MEASURED (their README).
- Scales linearly with cores "given the coarse nature of the operations."

**lbzip2 algorithm (C reference):** The `ALGORITHM` file references Burrows-Wheeler, Huffman coding, and suffix array construction papers. The decompression parallelism comes from the same block-independence property — bzip2 blocks are self-contained after the bit-aligned header is found.

**Block-boundary discovery challenge:** Not a killer. The `cosnicolaou/pbzip2` implementation proves it's tractable — the magic scan is fast relative to decompression (which is the bottleneck at 31 MB/s). The scan needs only a few bit operations per byte.

**Application to betterleaks:** bzip2 decode is ~20% of detector CPU on source-heavy corpora. At 31 MB/s single-stream (dsnet/compress), a 1MB bzip2 entry takes ~32ms. With 8 parallel blocks (typical bzip2 has 100-900KB blocks), you could get 4-8x speedup on individual large archives, reducing the 20% to 3-5%.

**Win regime:** Large bzip2 archives with multiple blocks (>900KB compressed = multiple blocks at default 900KB block size). Most source-code tarballs in git history are multi-block.
**Loss regime:** Small bzip2 entries (single block, <900KB) where parallelism provides no benefit — overhead of scanning + goroutine creation exceeds savings. Also: bzip2 that arrives via streaming (mholt/archives) may not provide the seekable io.ReaderAt interface that `cosnicolaou/pbzip2` requires for parallel scanning.
**Go implementation cost:** Medium — integrate `cosnicolaou/pbzip2` or adapt its block-scanner to work with `mholt/archives`' streaming decompression. The streaming vs seekable interface mismatch is the main challenge. If archives are memory-buffered (which they may be at 1MB fragment cap), this is solvable.
**Verdict: Strong candidate.** The library exists, is production-tested, and addresses the single largest remaining CPU consumer (20%). The integration challenge is the streaming interface, but since fragments are bounded at 1MB and likely buffered, a `bytes.Reader` adapter would work. Expected impact: 20% CPU * 0.6-0.75 reduction = 12-15% overall CPU savings on bzip2-heavy corpora. ESTIMATED.

---

### (e) Regex Set-Matching (RE2::Set)

**RE2::Set architecture (verified from source):**
- `RE2::Set` compiles multiple patterns into a single alternation (`Regexp::Alternate`), then compiles that into one `Prog` (NFA/DFA program).
- `Match()` runs a single DFA pass over the input and returns which pattern indices matched.
- It uses `Prog::CompileSet` — a specialized compilation that concatenates each pattern with a `HaveMatch(index)` marker, so the DFA can report which patterns are in the match set.
- From `set.h`: "Returns true if text matches at least one of the regexps in the set. Fills v with the indices of the matching regexps."
- Implementation note: `options_.set_never_capture(true)` — no capture groups in set mode.

**The project already benchmarked this (from `re2set_research_bench_test.go` and git log `685f6de`):**
The codebase has an experiment (`experiment(detect): prefilter mode knob — ahoc | re2set | none`) that tested compiling all 331 rule regexes into a single RE2::Set. The exhaustion backlog (A5) marks this as "Rejected for this patch" — meaning it was measured and didn't win against the existing Aho-Corasick keyword prefilter.

**Why it loses for this use case:**
- 331 patterns compiled into one DFA creates a very large automaton. RE2's lazy DFA may hit memory limits (`max_mem`) and fall back to NFA execution, which is much slower.
- The existing pipeline (trigram prefilter → 1.6 candidates → per-rule gates → full regex) already eliminates 99%+ of work. An RE2::Set scanning the full fragment against all 331 patterns does MORE work than the current pipeline on typical fragments.
- RE2::Set provides no captures/submatches — you'd still need to re-run individual regexes on matches to extract secrets.

**Hyperscan's approach (for comparison):** Hyperscan compiles all patterns into a single deterministic automaton using graph decomposition, not simple alternation. It uses SIMD-accelerated literal matching (Teddy/FDR) for the literal portions and specialized NFAs for complex patterns. This is fundamentally different from RE2::Set's "compile an alternation" approach — Hyperscan has pattern-specific optimizations that RE2::Set lacks.

**Win regime:** Workloads where most patterns match (high match density) and you need to know "which of these N patterns match this text" without individual per-pattern calls. Also wins when N is small (10-30 patterns) and patterns share common prefixes.
**Loss regime:** Large pattern sets (331) with low match density (1.6/fragment) where the prefilter already eliminates most patterns. The DFA state explosion with 331 arbitrary regexes makes the compiled program enormous.
**Go implementation cost:** Already prototyped via `github.com/betterleaks/go-re2/experimental.CompileSet`.
**Verdict: This loses (confirmed by project's own measurement).** The existing trigram prefilter + per-rule gates is more efficient than a single-pass DFA over all patterns when match density is low. The RE2::Set DFA is too large for 331 complex patterns.

---

### (f) Roaring Bitmaps / Learned Indexes over Trigram Postings

**At 366 patterns, credible?** No.

**Roaring bitmap overhead analysis:**
- Roaring bitmaps are designed for large, sparse universes with millions of elements. Their container overhead (array containers for sparse ranges, bitmap containers for dense ranges) has a minimum footprint that is wasteful at small scale.
- From the Roaring docs: "if you have a small universe size (e.g., n=64 or n=128), if you can use uncompressed BitSet and it does not blow up your memory usage, then compressed bitmaps are probably not useful to you."
- With 366 patterns, a posting list per trigram is at most 366 bits = 46 bytes as a raw bitset. Roaring's container overhead (minimum 8 bytes per container header + serialization metadata) adds nothing over a plain `[6]uint64` bitset.

**Current trigram prefilter design:** The existing implementation maps each trigram to a set of pattern indices (at most 366 patterns). At this scale, a simple array or bitset per trigram bucket is already optimal. The lookup is O(1) per trigram (hash to bucket), and intersection of hit sets is bitwise AND on 6 uint64s.

**Learned indexes:** Learned indexes (e.g., PGM-index, RMI) replace B-trees for range queries over sorted keys. Trigram lookups are exact-match (not range), and the key space (16M possible 3-byte trigrams, of which maybe 500-2000 are populated) is tiny. A simple hash map or direct-indexed array is already optimal.

**Win regime:** Would help if you had millions of patterns with complex posting list operations (intersection, union, rank queries). Think web search engines with billions of documents.
**Loss regime:** 366 patterns. Full stop. The problem is too small for these data structures to amortize their overhead.
**Go implementation cost:** Trivial to add a Roaring bitmap library, but pointless.
**Verdict: Noise.** At 366 patterns, a plain bitset per trigram is already optimal. Roaring/learned indexes solve problems at 10^6-10^9 scale. The existing prefilter is already at the theoretical minimum for this pattern count.

---

## Source Index

1. RE2 syntax documentation: `https://github.com/google/re2/blob/main/doc/syntax.txt` — confirms `\C` matches any single byte, `[[:ascii:]]` == `[\x00-\x7F]`, `[[:cntrl:]]` includes `\x00`
2. RE2 null-byte behavior: Experimentally verified via Go `regexp` package (RE2 semantics) — dot matches `\x00` with or without `s` flag
3. RE2::Set source: `re2/set.h` and `re2/set.cc` — `Regexp::Alternate` compilation, `Match()` returns `vector<int>` of matching pattern indices
4. wasilibs/go-re2 RATIONALE.md: WASM memory is Go GC-managed; no `Close()` needed because GC tracks allocations
5. ripgrep line_buffer: `crates/searcher/src/line_buffer.rs` — chunk-oriented, single `rfind_byte` per fill, no full newline index
6. Hyperscan compilation docs: `doc/dev-reference/compilation.rst` — `hs_compile_multi` for concurrent multi-pattern scanning
7. Hyperscan performance docs: `doc/dev-reference/performance.rst` — prefer separate databases over union; scratch pre-allocation; prefer literals
8. Suricata tuning: `doc/userguide/performance/tuning-considerations.rst` — MPM algo (ac/hs/ac-ks), detect.profile for group merging, sgh-mpm-context
9. cosnicolaou/pbzip2 README: Pure Go parallel bzip2, 8x on 8 cores, block-boundary magic-scan with fallback merge
10. lbzip2 ALGORITHM file: References BWT, Huffman, suffix array literature; confirms block-independence for parallel decode
11. betterleaks exhaustion backlog: `autoresearch.research/regex-scan-time/exhaustion-backlog.md` — A5 (regex set prefilter) rejected; A4 (candidate-window scanning) deferred
12. betterleaks re2set bench: `detect/re2set_research_bench_test.go` — all 331 rules compiled into RE2::Set, already measured
13. Roaring docs: "small universe size" and "sparse scenario" are not suitable for compressed bitmaps
14. RE2 test suite (`re2/testing/re2_test.cc`): `TEST(QuoteMeta, HasNull)` — confirms RE2 handles embedded null bytes in patterns and input