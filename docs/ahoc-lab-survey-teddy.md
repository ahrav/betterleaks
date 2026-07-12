# SIMD Multi-Pattern Literal Matcher Survey

## (1) ripgrep/aho-corasick Teddy

### Algorithm Overview
Teddy is a SIMD prefilter that uses nibble-indexed shuffle instructions to simultaneously test whether each byte in a vector could be the start of any pattern from a bucketed set.

**Core steps per vector:**
1. Load 16 bytes (128-bit NEON) of haystack
2. Split each byte into low and high nibbles: `lo = byte & 0xF`, `hi = byte >> 4`
3. Use each nibble as an index into a precomputed 16-byte shuffle mask (via `vqtbl1q_u8` on NEON / `pshufb` on x86). Each position in the mask is an 8-bit bitset indicating which buckets have a pattern with that nibble at the corresponding fingerprint position
4. AND the lo-result and hi-result: a bit set in the output means "bucket N has a pattern whose first byte has this exact lo AND hi nibble" — i.e., exactly this byte
5. For multi-byte fingerprints (mask_len 2-4): repeat for positions 1,2,3 using `vextq_u8` to align across chunk boundaries, AND all results together
6. If result vector is non-zero, verify candidates; otherwise advance by 16 bytes

### Slim vs Fat
- **Slim**: 8 buckets, processes full vector width per iteration. Each byte in the candidate vector is an 8-bit bitset (one bit per bucket). Works on 128-bit NEON and 128/256-bit x86.
- **Fat**: 16 buckets, AVX2-only (256-bit). The haystack window is only 128 bits repeated in both halves, but each half represents different buckets (0-7 in low, 8-15 in high). **Fat Teddy is NOT available on aarch64** — the code explicitly states "aarch64 has no fat variant (at least 256-bit vectors would be wanted)."

### Bucket Assignment (Hyperscan)
NOT a hash — it's a greedy agglomerative packing:
- Each pattern starts as its own `TeddySet` with its nibble footprint
- Repeatedly merge the pair with lowest combined false-positive probability: `heuristic = probability() * (2 + litCount())`
- `probability()` = product of popcounts of each nibble set
- Flood-prone merges are avoided
- Merging stops when at bucket count; if it can't fit, Teddy build fails

### Bucket Assignment (BurntSushi crate)
Simpler: `bucket = (BUCKETS - 1) - (pattern_id % BUCKETS)` — round-robin in reverse order. The comment says this is to "make it harder to get leftmost match semantics accidentally correct."

### Mask Length Selection
```rust
mask_len = min(4, patterns.minimum_len())
```
Longer masks = fewer false positives but more per-byte work (extra shuffle + shift + AND per mask position).

### Pattern Count Limits — CRITICAL FOR 366 PATTERNS

**BurntSushi aho-corasick crate (what ripgrep uses):**
- Global hard limit: **64 patterns** (with heuristic limits enabled, which is the default)
- aarch64-specific soft limits: mask_len 1 → 16 patterns; mask_len 2 → 32; mask_len 3 → 48; mask_len 4 → no limit
- Limits are bypassable via `heuristic_pattern_limits(false)` but performance degrades

**At 366 patterns, the crate will NOT build a Teddy searcher** — it returns `None` from `build()`, falling back to standard Aho-Corasick NFA/DFA. The PATTERN_LIMIT in `api.rs` is **128**, and the builder further restricts to 64 with heuristics.

**Hyperscan Teddy:**
- Limit is `numBuckets * TEDDY_BUCKET_LOAD` (value not in the source I could access, but with 8 buckets, typical loads of 8-16 patterns per bucket → 64-128 patterns max)
- Beyond that limit, Hyperscan falls back to FDR

### Verification Step
Candidates are extracted as 64-bit lanes via `vreinterpretq_u64_u8` + `vgetq_lane_u64`. For each set bit: `byte_offset = bit / BUCKETS`, `bucket = bit % BUCKETS`. Then iterate that bucket's pattern list doing prefix comparison (`is_prefix_raw`).

### NEON-specific Implementation (from `vector.rs`)
| Operation | NEON Intrinsic |
|---|---|
| Shuffle (nibble lookup) | `vqtbl1q_u8` |
| Load | `vld1q_u8` |
| AND/OR | `vandq_u8` / `vorrq_u8` |
| Cross-boundary shift | `vextq_u8(prev, cur, 15/14/13)` |
| Is-zero check | `vpmaxq_u8(v,v)` then check one u64 lane |
| Nibble extract | `vshrq_n_u8` (native, unlike x86 which emulates) |
| Lane extract | `vgetq_lane_u64` on reinterpreted vector |

Key advantage on NEON: `vshrq_n_u8` is a single native instruction (x86 must use `_mm_srli_epi16` + mask). Key disadvantage: no `movemask` — must extract two 64-bit lanes and iterate bits with `trailing_zeros`.

### When Packed Searcher is Used vs AC Fallback
1. Build fails (>64 patterns, no SIMD features, big-endian) → standard AC
2. Haystack < ~34 bytes → Rabin-Karp fallback (64 buckets, rolling hash)
3. Otherwise → Teddy

**VERDICT for 366 patterns: Teddy is inapplicable.** The crate would use standard AC (NFA contiguous or DFA, depending on memory). MEASURED: ripgrep blog shows Teddy gives "order of magnitude" speedup over grep for small alternation sets (16-32 patterns), but no data for 300+ patterns.

---

## (2) Hyperscan FDR

### Algorithm
FDR (Fast Direct Retrieval) is a shift-or style filter for medium-to-large literal sets (where Teddy's bucket count is insufficient). It uses a **hash table indexed by 9-15 bit "super-characters"** spanning 1-2 input bytes.

**Key parameters:**
- Domain: 9-15 bits (`assert(eng.bits > 8 && eng.bits < 16)`)
- Domain mask: `(1 << bits) - 1`
- Table entries are bitmasks with one bit per bucket
- Buckets confirmed via `FDRConfirm` structures (exact string comparison)

**Per-byte operation flow (from ARM `fdr_impl.h`):**
1. Load 16 input bytes as two u64 words
2. For each position (stride 1/2/4): compute table index = `domain_mask & (shifted_input)`
3. Load 128-bit filter entry from table
4. Byte-shift to align with position: `lshiftbyte_m128(entry, N)`
5. OR-accumulate into persistent state register `*s`
6. Extract and complement low 64 bits → confirm bitvector
7. For each set bit: `byte = bit/8 + offset`, `bucket = bit%8`
8. Look up `FDRConfirm`, verify with full string compare

**Stride concept:** Higher strides (2, 4) process fewer positions per cycle but are cheaper; stride 1 checks every byte. Stride selection is compile-time per engine configuration.

### Pattern Capacity
- `fdr->numStrings = verify_u32(lits.size())` — theoretically up to ~4 billion patterns
- Practical limit: chunking assumes ≤512 chunks of same-length strings
- MEASURED (from Hyperscan docs): system "runs faster with small numbers of patterns and slower with large numbers" in a smooth fashion

### FDR vs Teddy Selection
```cpp
if (grey.fdrAllowTeddy) {
    auto proto = teddyBuildProtoHinted(...);
    // if teddy build fails:
    "build with teddy failed, will try with FDR"
}
```
Teddy is tried first; FDR is the fallback for larger/incompatible sets.

### Bucket Assignment (FDR)
Dynamic programming over a 2D score table:
- Literals sorted by length, grouped into chunks (≤512, lengths ≤16)
- Score function: `pow(count, 1.05) * pow(len, -3)` — penalizes short literals and crowded buckets
- Longer literals come first after DP assignment
- Identical-case variants kept together

### Vectorscan NEON Port Status
- **ARM NEON/ASIMD: "100% functional"** per Vectorscan README (v5.4.12, July 2025)
- The `src/fdr/arm/` directory contains `fdr_impl.h` — the ARM FDR runtime
- FDR on ARM uses Vectorscan's portable SIMD abstraction (`m128` → NEON intrinsics via `util/simd_utils.h`)
- Teddy on ARM: the `src/fdr/teddy.cpp` 128-bit path compiles unchanged for NEON (same abstraction layer)
- SVE2 support is "ongoing" — not yet production-ready for Teddy/FDR
- No Teddy-specific ARM file exists — it reuses the generic 128-bit slim path

### Reported Throughputs
- Hyperscan docs mention "3.0 Gbps" (~375 MB/s) as an ILLUSTRATIVE example (not a measured figure for any specific configuration). ESTIMATED.
- No published FDR-specific throughput benchmarks found in open documentation
- The NSDI 2019 paper (Wang et al.) would have real numbers but is behind paywall

---

## (3) Scalar (GPR) FDR Variant for Pure Go

### Concept: 64-bit Shift-Or Multi-Pattern
The FDR filter uses shift-or at its core: for each input position, look up a bitmask and OR into running state; a surviving zero bit indicates a candidate. On x86/NEON, the "lookup" uses 128-bit shuffle. For a scalar Go version with 8 buckets × 8-bit domain shift-or:

**Per-byte operation count (scalar shift-or):**
1. Compute hash/index from input byte(s): 1-2 ops (mask + shift)
2. Load table entry (8 bytes for 8 buckets): 1 memory load
3. OR into state: 1 op
4. Shift state (aging): 1 op
5. Check for zero bits (candidates): 1 AND + 1 branch
**Total: ~5-6 ops/byte** for single-byte domain

For a 2-byte domain (9-15 bits, like FDR):
1. Combine two bytes into index: 2 ops (shift + OR + mask)
2. Load table entry: 1 load
3. OR into state: 1 op
4. Shift state: 1 op
5. Check: 1 op
**Total: ~6-7 ops/byte** — but indexing a larger table (512-32768 entries × 8 bytes = 4-256 KB)

### Comparison to Current Trigram Scanner (520 MB/s)
Current scanner: multiply+mask+load per 8-byte window (SWAR), ~8 probes per 64-bit word
- Effective: ~1 op/byte for the hash, ~1 load/byte for bitmap check = ~2-3 ops/byte for the anchor test alone, plus lowercasing overhead
- At 520 MB/s on Graviton3 (~3.25 GHz), that's ~6.25 cycles/byte

**Scalar FDR estimate for 366 keywords:**
- 8 buckets → ~46 patterns/bucket average (quite crowded)
- With 2-byte fingerprint (9-bit domain, 512-entry table × 8 bytes = 4 KB — fits L1): ~6-7 ops/byte = ~2 cycles/byte at best with ILP
- But confirmation overhead with 46 patterns/bucket would be devastating
- Need more buckets: 16 buckets → u16 state per bucket → 16-bit shift-or → 32 KB table (marginal L1)
- Or longer fingerprints: 3 bytes reduces false positive rate to ~(popcount/256)^3 ≈ very low, but now table is enormous

**Viable configuration for 366 patterns:**
- 64 buckets (like BurntSushi's Rabin-Karp): ~5.7 patterns/bucket
- 2-byte fingerprint with 10-bit domain: 1024 entries × 8 bytes = 8 KB table
- Per-byte: index compute (2 ops) + load (1) + OR state (1) + shift (1) + zero-check (1) = 6 ops
- ESTIMATED throughput: ~550-700 MB/s if pipeline-friendly (no branches in hot path), ~400-500 MB/s realistically

**vs current 520 MB/s:** Marginal win at best; the trigram scanner already achieves excellent throughput with simpler logic. The scalar FDR's advantage would be EXACT candidate identification (which bucket hit), enabling direct confirmation rather than hash-table lookup. But the confirmation cost with 5-6 patterns per bucket is still substantial.

### Prior Art: Scalar Multi-Pattern Shift-Or in Production
1. **agrep** (Wu & Manber, 1992): Original shift-or/bitap for approximate single-pattern matching. Extended to multi-pattern in the Wu-Manber algorithm but using a different hash-based skip approach.

2. **Snort IDS (before Hyperscan)**: Used AC-BM (Aho-Corasick with Boyer-Moore-style skipping) — not shift-or. When Hyperscan replaced it, the shift-or style matching was SIMD-only.

3. **BNDM/BDM family**: Backward nondeterministic DAWG matching uses shift-or in the reverse direction. Multiple patterns handled by factoring into a single NFA encoded in bit-parallel registers. Limited to `w` patterns (word size 64) for single-register variants.

4. **nrgrep** (Navarro 2001): Multi-pattern BNDM for practical use in grep. Scalar, uses 64-bit shift-or. Limited to patterns whose combined character-class alphabet fits in 64-bit registers. MEASURED: reported faster than GNU grep for small pattern sets (<10) on texts with low match density.

5. **No production system found using scalar FDR specifically** — FDR was designed for SIMD from the start (Intel's Hyperscan team). The domain table approach only makes sense when you can parallel-lookup with shuffle instructions.

**VERDICT:** A scalar shift-or for 366 case-insensitive patterns would need either (a) very wide state (366 bits = 6 u64s, each needing shift+OR per byte = 12 ops/byte minimum — slower than current scanner), or (b) bucketed approach with confirmation overhead. Neither clearly beats the 520 MB/s trigram scanner for this specific workload. The trigram scanner's advantage is that it skips 8 bytes at a time via SWAR, effectively amortizing the cost across multiple positions.

---

## (4) Go Multi-Pattern Libraries with Vectorization

### cloudflare/ahocorasick
- **Language:** Go
- **SIMD/vectorization:** None. Pure Go implementation.
- **Throughput:** No benchmarks published on README
- **Status:** 724 stars, 20 commits, appears unmaintained (no releases, 4 open issues)
- **Notes:** Basic AC implementation, no optimization claims

### anknown/ahocorasick
- **Language:** Go
- **SIMD/vectorization:** None. Pure Go double-array trie.
- **Throughput claims (MEASURED, self-reported):**
  - Chinese (153K patterns): 1,814 ms vs cloudflare's 28,926 ms (16x faster)
  - English (127K patterns): 1,619 ms vs cloudflare's 19,835 ms (12x faster)
  - ~12x less memory than cloudflare
- **How it works:** AC on double-array trie (cedar port). MIT license.
- **Status:** 73 commits, no releases, probably unmaintained

### iohub/ahocorasick
- **Language:** Go
- **SIMD/vectorization:** None. Cedar-based double-array trie.
- **Throughput:** No absolute numbers. Claims "fast, compact and low memory" via benchmark chart vs cloudflare/anknown.
- **Status:** 126 stars, 73 commits, GPL-2.0

### petar-dambovaliev/aho-corasick
- **Language:** Go
- **SIMD/vectorization:** None.
- **Claims:** "x20 faster than cloudflare, x3 faster than anknown, 1/8 memory of cloudflare" (no methodology/hardware disclosed). CLAIMED, unverified.
- **How it works:** NFA O(N+M) or DFA O(N) modes. Inspired by BurntSushi's Rust crate.
- **Status:** 95 stars, 34 commits, 3 open issues

### wasilibs/go-aho-corasick
- **Language:** Go (wraps Rust aho-corasick via WASM/wazero)
- **SIMD/vectorization:** Indirectly — the Rust aho-corasick crate's Teddy/SIMD paths are compiled to WASM, but **WASM sandbox likely prevents native SIMD** (wazero's SIMD support is limited to WASM SIMD128 opcodes, not native AVX2/NEON).
- **Throughput (MEASURED, from benchmarks page):**
  - 4+ pattern cases: "performs significantly better" than pure-Go alternatives
  - sherlock/5000words: 8.4ms vs 21.6ms (pure Go) — ~2.5x faster
  - Auto-detection of NFA/DFA/contiguous-NFA helps for unknown inputs
- **Status:** Latest release v0.6.0 (Apr 2024), MIT license
- **Platform:** Any Go platform (no CGo required). ARM64 works via wazero.
- **Optional CGo mode:** Can link native Rust library directly (would get full SIMD), but loses portability.

### flier/gohs (Hyperscan/Vectorscan bindings)
- **Language:** Go (CGo bindings to Hyperscan C library)
- **SIMD:** Full native SIMD via Hyperscan/Vectorscan (Teddy+FDR on x86, 128-bit Teddy+FDR on ARM via Vectorscan)
- **ARM support:** Via Vectorscan — "ARM NEON/ASIMD 100% functional"
- **Throughput:** No published numbers, but inherits Hyperscan's performance
- **Status:** v1.2.3 (Dec 2024), 321 stars, 317 commits, reasonably maintained
- **Caveats:** Requires CGo, Hyperscan/Vectorscan C library installation, significant build complexity

**VERDICT:** No pure-Go library uses SIMD vectorization for multi-pattern matching. The only vectorized option is CGo binding to Hyperscan/Vectorscan (flier/gohs). wasilibs gets Rust's smart NFA/DFA selection via WASM but likely loses native SIMD.

---

## Summary Table

| Approach | Pattern Limit | Throughput (est) | Fits 366 patterns? | Go-viable? |
|---|---|---|---|---|
| Teddy (aho-corasick crate) | 64 (default) | ~2-10 GB/s (x86 SIMD) | **NO** | Via WASM (no SIMD) |
| Teddy (Hyperscan/Vectorscan) | ~64-128 | ~2-5 GB/s est | **NO** | CGo only |
| FDR (Hyperscan) | Thousands | ~375 MB/s - 2 GB/s est | **YES** | CGo only |
| Scalar shift-or (64 buckets) | ~384 | ~400-500 MB/s est | Marginal | Yes (pure Go) |
| Current trigram scanner | Unlimited | 520 MB/s MEASURED | **YES** | Yes |
| wasilibs (WASM AC) | Unlimited | ~200-400 MB/s est | Yes | Yes |
| flier/gohs (CGo Vectorscan) | Thousands | ~1-3 GB/s est | Yes | CGo required |

---

## Source Index

1. BurntSushi/aho-corasick `src/packed/api.rs` — pattern limits, fallback logic (GitHub master)
2. BurntSushi/aho-corasick `src/packed/teddy/generic.rs` — full Teddy algorithm, nibble masks, slim/fat, verification (GitHub master)
3. BurntSushi/aho-corasick `src/packed/teddy/builder.rs` — aarch64 limits, mask_len selection, slim/fat choice (GitHub master)
4. BurntSushi/aho-corasick `src/packed/vector.rs` — NEON intrinsic mapping, vqtbl1q_u8, vextq_u8 (GitHub master)
5. BurntSushi/aho-corasick `src/packed/rabinkarp.rs` — 64-bucket rolling hash fallback (GitHub master)
6. Intel/hyperscan `src/fdr/fdr_compile.cpp` — FDR bucket DP, domain 9-15 bits, super-characters (GitHub master)
7. Intel/hyperscan `src/fdr/teddy_compile.cpp` — Teddy greedy packing, TEDDY_BUCKET_LOAD limit (GitHub master)
8. VectorCamp/vectorscan README — "ARM NEON/ASIMD 100% functional" (GitHub develop)
9. VectorCamp/vectorscan `src/fdr/arm/fdr_impl.h` — ARM FDR runtime, portable SIMD abstraction (GitHub develop)
10. VectorCamp/vectorscan `src/fdr/teddy_runtime_common.h` — shared confirm logic, bucket structure (GitHub develop)
11. VectorCamp/vectorscan `src/fdr/teddy.cpp` — 128-bit Teddy runtime, mask lookup, reinforced masks (GitHub develop)
12. burntsushi.net/ripgrep/ — Teddy described as "order of magnitude" faster than grep (blog post)
13. Hyperscan performance docs — "3.0 Gbps" illustrative example
14. cloudflare/ahocorasick — pure Go, no SIMD, no benchmarks (GitHub)
15. anknown/ahocorasick — pure Go double-array trie, 16x faster than cloudflare (self-measured)
16. iohub/ahocorasick — pure Go cedar-based, no throughput numbers (GitHub)
17. petar-dambovaliev/aho-corasick — pure Go NFA/DFA, claims 20x over cloudflare (GitHub)
18. wasilibs/go-aho-corasick — Rust via WASM/wazero, 2.5x faster at 5000 patterns (GitHub benchmarks)
19. flier/gohs — CGo Hyperscan/Vectorscan bindings, ARM via Vectorscan (GitHub, v1.2.3)
20. Bouma2 paper (arXiv:1209.4554) — 2x throughput of AC-Snort at 10% memory (claimed)