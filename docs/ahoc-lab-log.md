# AhoC Prefilter Lab — autoresearch session log

Date: 2026-07-11. Worktree: `betterleaks-ahoc-lab` (branch `perf/ahoc-prefilter-lab`,
base b8c1d68). Aho fork copy: `/local/home/ahrav/scratch/aho-lab` (go.mod replace
repointed so the upstream `aho-integration` checkout stays untouched).

## Config

- **Goal**: reduce end-to-end scan time of the detector by rethinking the
  keyword-prefilter stage: (a) can Aho-Corasick be dropped entirely on either
  regex engine, (b) can data-type-aware techniques (first-byte density, SIMD
  skips, succinct structures) beat the current AhoC walk.
- **Metric**: wall-clock scan time (lower better) / throughput (higher better),
  measured by `scripts/benchcpu2.sh` — wall_min per corpus; cpu_min recorded as
  cross-check because the box is shared.
- **Corpora**: `corpora/git` (375M), `corpora/cpython` (1G), `gitlab-foss` (2G .git).
- **Guard (strict)**: findings digest identical to baseline on all three corpora
  AND `go test ./...` green (wazero build) AND `go test -tags re2_cgo ./detect`
  green. Zero tolerance for detection behavior change.
- **Stopping**: unlimited with plateau pause after 15 consecutive non-improving
  iterations.
- **Engines under test**: default wazero build (`CGO_ENABLED=0`) and cgo RE2
  (`-tags re2_cgo`). Keep/discard decided on the default wazero build; cgo
  tracked to answer the "drop AhoC with native RE2" question.

## Results TSV

`docs/ahoc-lab-results.tsv` — columns:
label runs cpu_mean cpu_std cpu_min wall_mean wall_min tree_peak_rss_mb findings digest load1

## Iteration log

| # | change | verdict | notes |
|---|--------|---------|-------|
| 0 | baseline (AhoC prefilter, both engines) | — | see TSV rows `base-wazero/*`, `base-cgo/*` |
| 1 | prefilter=none (drop keyword stage entirely) | **discard — guard fail + 12x CPU** | git corpus: 7999 findings vs 15 (digest f1e9e70a…); 1058 CPU-s vs 86. The keyword prefilter is *semantically load-bearing*: rule keywords are independent hints not implied by the rule regexes, so removing the stage changes detection behavior. Any AhoC replacement must still enforce keyword presence. Commit 685f6de. |
| 2 | prefilter=re2set, wazero (RE2::Set keyword scan replaces Go AhoC walk) | **discard — 4.5x CPU on git** | digest parity holds, but git 385.5 cpu_min vs 86.2 base; cpython 453 vs 369. Per-decode-pass wasm call + full buffer copy into wasm memory swamps any DFA advantage. Commit 685f6de. |
| 3 | prefilter=trigram (rolling 3-gram bitmap + anchored verify) | **keep — git cpu −3.1%, digest parity 3/3** | git 83.53 vs 86.17, gitlab-foss 1352.7 vs 1374.0 (−1.5%). Microbench walk 286→319 MB/s. Differential vs production trie: identical sets. Commit 99a9728. |
| 4 | fused lowercase+trigram single pass | **keep — cumulative cpython cpu −11.6% (cgo)** | trigram2 rows: cpython-cgo 326.4 vs 362.0 base (−9.8%); includes chunked reader (#5). Commit 6e8cd2b. |
| 5 | chunked readUntilSafeBoundary (peek/discard, same state machine) | **keep — boundary scan 140→965 MB/s (6.9x)** | 22% flat CPU on cpython file scans → 5.4%. Differential-fuzzed 5000 trials byte-identical. Commit b5de91a. |
| 6 | word-at-a-time trigram windows, LE keys (no serial chain) | **keep — stage 261→315 MB/s (+21%)** | trigram3 rows: cpython-wazero cpu 333.3 vs 369.1 base (−9.7%), wall 42.2 vs 56.8 (−26%); cpython-cgo 326.5 vs 362.0 (−9.8%); git-cgo 81.8 vs 83.9 (−2.5%). gitlab-foss wall rows for trigram3 ran at load1≈72, cpu_min 1362 vs 1374 (−0.9%). Full `go test ./...` green. Digest parity 9/9 rows. Commit 7860ab3. |

### Cumulative state after iteration 6 (kept stack: trigram+fused+chunked+word-at-a-time)

cpu_min (s), digest parity everywhere:
| corpus | base wazero | now wazero | base cgo | now cgo |
|---|---|---|---|---|
| git | 86.17 | 83.61 (−3.0%) | 83.91 | 81.81 (−2.5%) |
| cpython | 369.09 | 333.29 (−9.7%) | 361.98 | 326.51 (−9.6%) |
| gitlab-foss | 1374.00 | 1362.50 (−0.9%) | 1318.48 | pending | |

Interpretation: the AhoC walk *can* be dropped — not for the regex engine's
own prefilters (that loses 12x/4.5x, iterations 1-2) but for a flat
data-aware trigram fingerprint scan exploiting (a) distinct-set-only
semantics, (b) minLen>=3, (c) measured 4-6% 3-gram density on source code.
gitlab-foss gains least because its scan is pack-object/parse dominated
rather than detector dominated.

| 7 | self-pruning scan state (bitmap-copy version) | superseded by 8 | stage 366 MB/s but copies 8KB bitmap + clears per scan. trigram4 rows: cpython-wazero 335.4 (vs 322.6 ahoc-control — still losing). Commit cf0c8f9. |
| 8 | epoch-stamped scan state (O(1) reset, no copies) | keep (folded into 9) | stage 325 MB/s with zero per-scan setup cost. |
| 9 | rarest-anchor trigram selection (corpus frequency table) | **keep — stage 263→434 MB/s (+65%)** | 16K-entry generated `commonTrigrams` table; keyword indexed under its rarest window, verify anchored at i-offset. Bitmap-hit density on cpython: 5.75%→**1.43%**. "---" alone was 27% of hits (matched -----BEGIN at every table/comment ruler). Differential green. Commit cb6833b. trigram5 e2e: cpython-wazero 333.7 (≈trigram3 — prefilter is no longer the cpython bottleneck; findEncodingMatches + bzip2 + boundary are), git-wazero 84.0, gitlab-cgo 1298.9 (best gitlab yet). **Real-corpus stage bench: lower+ahoc 212.7 MB/s → fused trigram 403.9 MB/s (1.90x).** |
| 10 | boring-byte skip in findEncodingMatches | **keep — scanner 426→465 MB/s** | one-table skip loop for bytes no dispatch branch handles; invariant test pins the set. trigram6 rows: cpython-wazero 321.1 / wall 36.4s (base wall 56.8s, −36%); cpython-cgo 314.3 (base 362.0, −13.2%); gitlab-cgo 1295.1 (base 1318.5, −1.8%). |
| 11 | direct-index pair bitmap gate before trigram hash | **discard — real-corpus stage 403→219 MB/s** | second 8KB bitmap competes for L1 and adds a 27%-taken unpredictable branch per window; the Fibonacci-hash probe it "saves" costs one multiply. Reverted. Measured densities that killed it: anchor first-byte 69.4%, anchor pair 27.0%. |
| 12 | allow-signature gate (`leaks:allow` single scan) | keep | 2.8% flat CPU on gitlab-foss was two strings.Contains per finding line (huge minified lines). Invariant test pins gate ⊂ every signature. Commit in trigram7 batch. |
| 13 | words.HasAnyMatchInList early-exit + maxWordLen cap | **keep — 3x word-match microbench** | expr token-efficiency filters called O(n²)-map-probe HasMatchInList for a boolean; ~2% gitlab CPU. Differential over 20k random secrets. Commit beccaa1. trigram7 rows: cpython-cgo 310.8 (best yet, base 362.0 = −14.1%); cpython-wazero 321.4. |
| 14 | unrolled testWindows (straight-line hash block) | **keep — real-corpus stage 404→520 MB/s** | variable-shift loop blocked unrolling; now 8 dependency-free hashes then 8 predictable tests. Stage now 2.45x production. trigram8: git-cgo 82.30, cpython-cgo 309.00 (best), gitlab-cgo 1290.87 (best). wazero trigram8 rows ran at load 32-76, treat as noisy. |

### Honest e2e attribution (final-ahoc-wazero control: all kept non-prefilter changes, AhoC prefilter)

control cpu_min: git 84.98, cpython 310.58, gitlab 1360.15. vs trigram8-wazero
83.91/317.50/1355.97 (higher load). **On cpython the prefilter swap is now
e2e-neutral**: after the chunked reader + boring-skip + word-match fixes,
prefilter is no longer the bottleneck there (bzip2/encodings/boundary are).
Trigram's e2e edge is clearest on git (−1.3%) and stage-level (2.45x); its
architectural value is unlocking hit positions for windowed verification.

| 15 | hit-window candidate verification (E1, static widths) | superseded by 16 | 62/330 rules windowable; git full_regex_bytes 83.4→77.9MB only. window-* rows: cpython-wazero 316.2 (vs trigram6 321.1). |
| 16 | line-bounded window plans (W = static + nlFreeSites×maxLine) | **keep — 291/330 rules windowable** | unbounded-but-newline-free repeats bounded per fragment by longest line; anchors proven safe under windowing (no interior-edge matches). git corpus: gate bytes 87.7→64.6MB (−26%), full-regex bytes 83.4→61.0MB (−27%). Differential + full suite green. window2 rows: cpython-cgo 310.4/wall 33.1 (base 362.0/56.8); gitlab-cgo 1296.2/wall 48.4 (base 1318.5/49.5); git-cgo 82.4 (base 83.9). e2e delta vs window1 is small because gate volumes on these corpora were already modest; the win concentrates on fragments with many candidate rules. |

| 17 | windowing cost floor (≥16KB fragments, W*8 < fragment) | keep | ruleWindows bytes.Index was 7% of gitlab CPU when windowed unconditionally; floor removes it for small fragments where a single pass wins. window3 rows confirm gitlab recovery. Commit 5157fc2. |

## Round 2 (2026-07-12, continuing loop)

| # | change | verdict | notes |
|---|--------|---------|-------|
| 18 | occurrence recorder: trigram scan feeds hit positions to window construction | **keep — best gitlab rows yet; stage 8.3→471 MB/s (57x) on keyword-dense 256KB** | positions recorded for window-eligible patterns (cap 48, overflow→bytes.Index fallback); soundness = completeness invariant, differential-pinned (recorder vs strings.Index 500 trials; full detector recorded-vs-full 40 large fragments incl. overflow). e2e occrec rows: gitlab-cgo **1288.7** cpu/48.6 wall (prior best 1290.9/48.4), gitlab-wazero 1333.0 (prior 1341.6, −0.6%), cpython/git parity-or-better. The 57x stage win compresses to <1% e2e because the cost floor (iter 17) already keeps small fragments off the bytes.Index path — the recorder mainly wins on large keyword-dense fragments (gitlab's regime). |

| 19 | scalar Teddy-style position masks (M0&M1&M2 bucket sets, byte-serial rolling) | **discard — 499→305 MB/s on real corpus** | replacing the multiply-hash + 8-probe word-parallel loop with per-byte 32B row loads + 2 ANDs loses: the word-at-a-time structure (8 independent probes per 64-bit word) matters more than removing the multiply. Confirms round-2 survey's implicit point — mask tests only win when SIMD tests 16+ positions at once (real Teddy); serialized per-byte they're worse than hashing. Differential was green (semantics fine); perf killed it. |

| 20 | parallel bzip2 decode (cosnicolaou/pbzip2 for archives.Bz2) | keep-candidate, RSS fix in 21 | decoder microbench: stdlib 25.5, dsnet 30.5, pbzip2 112 MB/s. e2e pbz2 rows: **cpython wall 28.0/27.2s (from 34.4/34.2) — −20% wall**; cpu +4% (parallel overhead + duplicate-block rescans); git/gitlab unchanged (no bz2 content). **BUT tree_peak_rss 4.9GB→22GB** — unbounded block decoders × concurrent archive scans. Digest parity 6/6. |
| 21 | bound pbzip2 concurrency (4/reader + GOMAXPROCS shared pool) | **keep — cpython wall 25.5s (base 57.5, −56%), RSS 22GB→6.8GB** | bounding also *improved* wall vs unbounded (25.5 vs 28.0 — less thrash); cpu 326.5 vs 316.7 pre-pbzip2 (+3% parallel overhead, bought −25% wall). gitlab/git untouched. RSS 6.8GB vs 4.9GB base is the remaining cost — acceptable; tunable via pool size. Digest parity all rows. |

| 22 | bz2 per-reader concurrency 4→16 A/B | **discard — 25.4s vs 26.3s wall** | pbzip2's scanner feeds blocks serially; more decoders per stream just buffer more. CPU sampling shows the cpython tail runs ~4 cores busy — the tail is scanner-serial, not decoder-starved. 4 kept with rationale comment. |

### Round-2 synthesis (surveys in docs/ahoc-lab-survey-*.md)

The far-fetched end is now bounded by evidence:
- **Teddy is inapplicable at 366 patterns** — hard caps at 64 (aho-corasick
  crate default; 128 absolute) and 64-128 (Hyperscan, which falls back to
  FDR beyond). Fat Teddy doesn't exist on aarch64 at all.
- **Scalar FDR/shift-or ≈ 400-700 MB/s ESTIMATED** vs our measured 520 —
  not a clear win, and per-bucket confirm at 366/64 patterns/bucket costs
  more than our rarest-anchor verify. Empirically confirmed directionally by
  iteration 19: per-byte mask/table tests lose to word-parallel hash probes.
- **The only remaining 4-10x stage path is real SIMD** (NEON asm with
  VTBL/VCMEQ + VADDP-syndrome — SHRN missing from Go's assembler — or
  GOEXPERIMENT=simd on Go 1.27+ which has arm64 128-bit with PermuteOrZero).
  Not worth it today: the stage is 4-6% of detector CPU, so even 10x nets
  ~4-5% e2e. Revisit if keyword count grows 10x or Go SIMD graduates.
- **Succinct structures**: MPHF loses to the 8KB bitmap's single-load
  negative path (97%+ of positions); daachorse-style double-array shrinks
  the AC table 944KB→~23KB but keeps the serial-load chain — only relevant
  if AhoC (not trigram) must stay default; sublinear skippers degenerate at
  minLen=3.
- **Position-feeding architecture** (the user's "real value is as substrate"
  hypothesis): CONFIRMED — iteration 18's occurrence recorder is exactly the
  literature-standard literal-engine→verifier handoff (TruffleHog spans,
  Hyperscan FDR confirm), and it produced the best gitlab rows of the lab.

## FINAL A/B (4 runs each, same session, cpu_min/wall_min seconds)

Baseline binaries rebuilt from b8c1d68 with the same aho-lab pin; digest
parity on all 18 rows; full `go test ./...` + `-race` + cgo-flavor suites green.

| config | git | cpython | gitlab-foss |
|---|---|---|---|
| baseline b8c1d68, wazero | 85.2/4.5 | 365.6/57.5 | 1368.9/52.8 |
| baseline b8c1d68, cgo | 82.7/4.2 | 358.9/57.2 | 1314.2/50.1 |
| lab stack + trigram, wazero | 84.2/4.3 | 316.7/**34.4** | 1341.6/53.0 |
| lab stack + trigram, cgo | 82.5/4.3 | 310.7/**34.2** | 1297.0/50.5 |
| lab stack + AhoC, wazero | 84.8/4.5 | 310.1/**30.0** | 1354.7/52.2 |
| lab stack + AhoC, cgo | 83.0/4.2 | 304.2/**29.7** | 1309.3/50.0 |

Headline: **cpython wall −48% (57.5→30.0s wazero; 57.2→29.7s cgo), detector
CPU −15%; git CPU −1..2%; gitlab CPU −1.3..2%** — dominated by the corpus-
independent fixes (chunked boundary reader, encoding-scanner skip, allow-
signature gate, word-match early exit, windowing floor). Prefilter verdict at
final pins: the trigram scan wins the stage 2.45x in isolation but is e2e-
neutral-to-slightly-negative against AhoC once everything else is fast
(cpython: AhoC control beats trigram by ~2% CPU — AhoC's state machine
amortizes the keyword-soup regime better than per-occurrence verify). The
trigram path is kept behind `BETTERLEAKS_PREFILTER=trigram` as an option and
as the substrate for future position-aware work (E1 windows currently
recompute occurrences; feeding trigram hit positions through would remove
that redundancy — top remaining idea).

## Research synthesis (docs/ahoc-lab-research-synthesis.md)

Ranked plan from the deep-research workflow (E1..E6). Verdicts confirmed by
lab: E6 RE2::Set = don't (matches iter 2); E2 trigram skip = done (iters
3-14). **E1 hit-window candidate verification = highest EV remaining**:
convert O(fragment × candidateRules) post-prefilter scanning to
O(hitWindows × rulesPerHit). TruffleHog ships ±512B windows; ripgrep does
inner-literal→line windowing. Gate stats (git corpus): requiredAny 70.8MB +
gate regex 87.7MB + full regex 83.4MB scanned per 378MB pass volume.

Soundness design for E1 (derived here, must hold for windowed rules):
1. Only rules whose every match provably contains a rule keyword
   (semi-generic shape embeds the keyword alternation) — others exempt.
2. Bounded max match width W from regexp/syntax (unbounded ⇒ exempt);
   window = [occEnd−W−1, occStart+W+1) per keyword occurrence (±1 makes \b
   evaluate identically and makes edge-spurious matches impossible), merged.
3. No ^ $ \A \z anchors (text-anchor semantics change under slicing) ⇒ exempt.
4. Occurrences via bytes.Index over lowerBuf per candidate rule keyword
   (byte offsets identical to raw); match indices rebased by window start.
5. Non-overlap/leftmost parity: disjoint merged windows + every match ⊆ some
   window ⇒ FindAllStringIndex output identical to full-fragment scan.

### CORRECTION from control run (iteration 6b)

`chunked-ahoc-wazero` (AhoC + chunked reader, no trigram): git 84.28,
cpython **322.56**, gitlab-foss 1360.97. So stack-on-stack the trigram scan
wins git (−0.8%), **loses cpython (+3.3%)**, ties gitlab-foss. The cpython
win in the table above was the chunked reader, not trigram. Profile
attribution: testWindows+verify ≈ 19.7% cum under trigram vs AhoC walk ≈
7.5% cum — repeated verification of high-frequency keywords (api, key,
token appear constantly in Python source and *fully match*) makes verify
the dominant cost; AhoC amortizes repeats in its state machine. Next
iteration: per-fragment bitmap with bits cleared once a trigram's bucket
is fully marked, so repeated occurrences cost one L1 probe only.

## Density evidence for a fingerprint prefilter (measured, 400 files/corpus)

Keyword-prefix density at all byte positions of ASCII-lowered source:
| fingerprint | distinct | git corpus | cpython |
|---|---|---|---|
| first byte | 39 | 69.7% | 70.5% |
| first 2 bytes | 174 | 22.9% | 24.1% |
| first 3 bytes | 263 | **4.15%** | **5.51%** |

Trie stats for the 366-keyword set: 1889 states, failTrans16 = 944KB (L2, not
L1); the serial state->state dependent-load chain is the fundamental cost.

## re2set full results (iteration 2, discard)

cgo Set is parity-neutral on git (86.09 vs 83.91 cpu_min) but +5% cpython,
+3.5% gitlab-foss: RE2's DFA over 366 alternations ≈ AC, but the per-pass
cre2 call and match-array marshalling add overhead, and Set can't beat the
in-Go walk it replaces. wazero Set is catastrophic (4.6x on git, 2.9x on
gitlab-foss): every decode pass copies the whole buffer into wasm memory and
JIT'd DFA execution is slower than native Go table walk. **Verdict: dropping
the Go prefilter for an engine-side Set loses on both engines; keyword
prefiltering must stay on the Go side.**

## Orthogonal hotspot found while profiling (cpython corpus)

`sources.readUntilSafeBoundary` = 22% flat / 62% cum CPU on the cpython file
scan: byte-at-a-time `bufio.ReadByte`+`WriteByte` loop hunting for `\n\n`.
bzip2 decode (dsnet/compress) adds ~20%. Candidate later experiment (outside
the prefilter question, inside the scan-time goal): buffered `ReadSlice`/
IndexByte scan with identical two-newline boundary semantics.
3-gram density sits *below* the fork's measured ~6.25% skip break-even, and a
3-gram test needs no serial chain: position-independent hash + L1-resident
bitmap, with rare anchored memcmp verification (buckets avg 1.4 keywords).
minLen=3 makes the 3-gram fingerprint sound (every keyword occurrence implies
its own first-3-gram at the start position).

## Baseline (iteration 0), 2026-07-11

Digest guard values (findings count / sha256, identical across engines):
- git: 15 / cbbf7d1aa3f8fccd598524c7b1e0c8fdc1daec3d73432ab2017eb0ce5908aeab
- cpython: 60 / 82a6ff1537317e063f2398f5eec12a8ae13018634b9a5fcb2a8c25ec86e38fcf
- gitlab-foss: 4709 / a2a180faddd8bdb0a6f55aeaf7775bd6dcc1a8b1d73d7516a7fd2d72cd6dee4a

wall_min (s) / cpu_min (s):
| corpus | wazero | cgo |
|---|---|---|
| git | 4.324 / 86.17 | 4.074 / 83.91 |
| cpython | 56.765 / 369.09 | 56.741 / 361.98 |
| gitlab-foss | 51.662 / 1374.00 | 49.548 / 1318.48 |

Note: cpython wall is dominated by a long single-threaded tail (wall 57s vs
cpu/32 ≈ 11.5s); gitlab-foss run overlapped with load1≈50 so wall there is
noisy — cpu_min is the honest cross-check on this shared box.

## Profile evidence (git corpus, wazero build, full scan)

Top detector-attributable CPU: `_ExternalCode` (wazero JIT'd RE2) 22%,
codec.findEncodingMatches 7%, **AhoC walkTable+skipRootTable ~7.2%+1.6% cum**,
fastLogParser.parseHunk 7%, asciiLower 1.3%. detectFragment cum 44.7%.

## Gate-stats evidence (git corpus as file tree, 22088 fragments, 378MB)

- rule_checks 34,711 → only **1.57 candidate rules per fragment** on average.
  The AhoC prefilter + gates already reduce 331 rules to ~1.6 candidates.
- full_regex_calls 3,579 on 83MB total — the regex engine sees only 22% of
  corpus bytes once per accepted rule; gates reject the rest.
- Implication: **dropping AhoC means 331 rules × 378MB = ~125GB of regex-gate
  scanning vs today's ~90MB** unless RE2-side prefilters absorb it. The
  interesting question is whether one *combined* RE2 pass (Set/alternation,
  determinized ≈ AC DFA) can replace the Go AhoC walk, not whether per-rule
  direct scanning can (it can't — 200x byte blowup).
