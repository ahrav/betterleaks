# Fast Git-Log Parser — Performance Research Log

Session start: 2026-07-10. Branch: `perf/05-fast-gitlog-parser`.
Machine: 64-core aarch64 (Graviton, NEON/SVE available), Go 1.25.11 linux/arm64.

Goal: find and validate the next meaningful performance improvement to the
Git parsing/scanning path (fast parser in `sources/gitlog_fastparse.go` and
how the scanner consumes its output), without breaking the consumed-output
contract:

- `File.NewName`, `IsDelete`, `IsBinary`
- `PatchHeader`: SHA, Message() (Title/Body), Author name/email, AuthorDate
- TextFragments: `NewPosition`, `rawAddedText` (single OpAdd line or `Raw(OpAdd)`)
- Deleted files skipped before fragment consumption.

Validation floor for every kept change:
- `go test ./sources/ -run 'FastParse|ParseGitLogDate|ParseRangeBytes|RawAddedText'`
- `go test ./...`
- generated-stream differential vs go-gitdiff (5000 seeds, part of the tests)
- real-repo corpus replay
- benchstat with -count>=10

## Baseline (2026-07-10, commit 17c568c)

All tests pass (`go test ./...`). Benchmarks, 64-core Graviton, Go 1.25.11:

- `BenchmarkFastParseGitLog/gitdiff` (generated stream, ~810KB): ~3.29 ms/op, 3.04 MB/op, 25.4k allocs/op
- `BenchmarkFastParseGitLog/fast`: ~1.31 ms/op, 1.005 MB/op, 11.2k allocs/op
- New `BenchmarkFastParseRealCorpus` (self-history 46MB, via BETTERLEAKS_BENCH_PATCH):
  - gitdiff: ~160 ms/op (289 MB/s), 349 MB/op, 1.65M allocs/op
  - fast:     ~51 ms/op (900 MB/s), 145 MB/op, 102k allocs/op

Observation: fast parser allocates ~3.1x the input size per parse of real
corpus. Added-text retention should be ~a fraction of input. Allocation
volume is the first suspect; CPU profile next.

## Profile (baseline, real corpus self-history.patch 46MB)

Corpus shape: 531 commits, 3461 file diffs, 14714 hunks, 765k added
lines, 24.8MB added bytes (54% of stream).

CPU (1.66s samples, 30 iters):
- runtime.memmove 21.7% — 53% of it from growslice (Builder growth),
  25% from bytes.Reader.Read (bufio refill), 22% Builder writes
- bufio.ReadSlice 15.7% flat (per-line call overhead; 1.6M lines/op)
- bytealg.IndexByteString 15.7% (newline scan inside ReadSlice; SIMD, necessary)
- parseHunk 6.6% flat / 74.7% cum
- GC-related (futex, lock2, madvise, allocNeedsZero, mallocgc) ≈ 10-13%

Alloc space: strings.Builder.Write in parseHunk = 76% (109MB/op allocated
for 24.8MB retained added text — 4.4x growth garbage). Builder.WriteString
18% is the benchmark consumer's tf.Raw() copy (production rawAddedText
avoids it for the fast parser; benchmark harness to be aligned).

Candidate list (evidence-ranked):
1. Reusable []byte scratch + exact-size string per hunk (kill Builder growth garbage)
2. Replace bufio.ReadSlice with block-oriented reader (kill per-line call overhead)
3. gitdiff.File/TextFragment slab allocation (alloc count)
4. First-byte dispatch / SWAR prefix checks in header loops (minor)

## Experiments

(Chronological. VERDICT: KEEP / REJECT / DEFER.)

### Exp 1 — reusable scratch buffer + exact-size string per hunk — KEEP

Replaced `strings.Builder` accumulation in parseHunk with a parser-owned
reusable `[]byte` scratch buffer; one exact-size `string(added)` copy at
hunk end. Kills Builder's ~4.4x growth-garbage (76% of alloc space).

Real corpus (self-history 46MB), benchstat n=10 vs baseline:
- sec/op: 53.23m -> 43.27m  (-18.71%, p=0.000)
- B/op:   115.1Mi -> 67.6Mi (-41.26%)
- allocs: 89.5k -> 67.9k    (-24.12%)

Validation: focused tests, full suite, 3x45s fuzz (robustness,
differential, generated) all pass; race pass; gitlab-foss corpus replay
x4 date formats in flight.

Post-change profile: ReadSlice 17% flat / IndexByteString 21% /
memmove 18% / parseHunk 16%. Next target: per-line read machinery.

### Exp 2 — custom window line reader replacing bufio.ReadSlice — KEEP

Parser-owned 512KB window; readLine hot path = IndexByte(buf[pos:end]) +
slice + advance; slow paths (compact, refill, spill, EOF) in
readLineSlow/readLineSpill. Removes bufio wrapper bookkeeping per line.

Real corpus benchstat n=10 vs exp1: sec/op 43.27m -> 40.56m (-6.27%,
p=0.000); B/op, allocs unchanged. Cumulative vs baseline: -23.8%.

Validation: focused+full suite pass, 3x45s fuzz pass, race pass,
gitlab-foss corpus x4 date formats pass (background run: all subtests ok).

Post-change profile: IndexByteString 33.5%, memmove 18.8%, parseHunk
14.1%, readLine 11.8%. Read machinery now ≈2/3 of parse time.

### Exp 4 — inline 32-byte SWAR pre-scan in readLine before IndexByte — REJECT

Hypothesis: at ~30-byte average lines the IndexByte call prologue
dominates; four 8-byte SWAR words inline should win. Result: +12.25%
SLOWER (p=0.000, n=10) vs exp2. arm64 IndexByte's NEON prologue is
already cheaper than a scalar SWAR loop with per-word bounds checks and
a dependent branch per word. Reverted. NEGATIVE RESULT recorded in a
readLine comment to stop future re-attempts.

Also: corpus validation switched from live `git log` subprocess (30min
test timeout on 1.8GB x 4 formats) to captured patch files:
gitlab-foss-400 (96MB, default date) + 120-commit iso/rfc/unix captures,
all replayed via TestFastParseMatchesPatchFile — all pass.

### Exp 5 — 2x growth floor for the hunk scratch buffer — KEEP

append's policy tapers to ~1.25x for large slices; the two 7.7MB hunks
in the corpus each allocated ~5x their size in ramp garbage. Explicit
2x-or-need growth: sec/op 40.56m -> 38.08m (-6.12%), B/op -30.3%
(67.6Mi -> 47.1Mi). Validated: focused tests + differential fuzz.

### Exp 6 — newline-inclusive payload append — KEEP (neutral)

Append add-line payloads with their trailing '\n' and strip the final
byte on a trailing no-newline marker, replacing the pendingAddNewline
flag + per-line branch. Time-neutral (p=0.19); simpler hot loop; kept
for the code-shape win. Differential fuzz + small-window tests pass.

### Exp 7 — span-based zero-copy hunk accumulation — REJECT

Record [start,end) window offsets per added line; flush to scratch only
when a refill invalidates the window; join once at hunk end (single copy
of each added byte). Correct (small-window torture test written for the
aliasing discipline — kept in tree), but +11.8% time and +12.3% B/op vs
exp6. Why: two int32 appends + bounds checks per added line cost more
than the one memcpy they defer (payloads average ~32 bytes: memcpy is
~2-3ns, span bookkeeping ~4-5ns), and the final join re-touches window
lines that are no longer L1-hot, where the incremental copy consumed
them while hot. NEGATIVE RESULT: for short-line streams, copy-on-consume
beats deferred zero-copy joins.

### Exp 8 — manually inline readLine fast path in the hunk-body loop — KEEP

The hunk loop consumes ~98% of lines; readLine costs 160 inline units vs
the 80 budget, so every line paid a call. Duplicating the two-line fast
path inline: -1.87% (p=0.000). Fuzz + tests pass.

### Exp 9 — PGO (default.pgo from real-corpus profile) — REJECT (neutral)

Both default.pgo and -pgo=file builds: no significant change (p>=0.3).
The hot functions are already static-call monomorphic; inliner decisions
that matter (readLine) exceed even PGO-boosted budgets.

### Exp 10 — inline 8-byte chunked copy for <=64B payloads — REJECT

Hand-rolled PutUint64 copy loop to avoid memmove call overhead:
+13.64% SLOWER. arm64 memmove is already optimal at these sizes and the
manual loop defeated append's fast path. Reverted.

### Exp 11 — hoist hunk-loop cursor into locals — KEEP

line/buf/pos live in locals inside the hunk body loop; struct sync only
around readLineSlow. Kills the per-line p.line slice-header store to the
heap-resident parser struct + write barrier (line-level profile showed
`p.line = p.buf[...]` at 170ms of 1.45s). -6.31% (p=0.000) vs exp8.
Full battery: unit, small-window, 3x45s fuzz, 4x corpus replay, full
suite — all pass.

### Exp 12 — added-text arena + unsafe.String aliasing — KEEP

Payloads append into a chunked arena (64KB chunks; oversized hunks get a
private 2x chunk); the OpAdd Line string aliases the arena range via
unsafe.String. One copy per added byte instead of two. Chunks are
append-only, abandoned on growth, never recycled — exposed strings stay
immutable. Trailing no-newline strip happens before exposure.
-2.53% time, -18.5% allocs, +2.7% B/op (arena rounding). Race + fuzz +
small-window + corpus + full suite pass.
Safety note: relies only on append-only discipline within this file;
consumer (detect) takes subslices of fragment.Raw, which alias the arena
— same lifetime semantics as any Go string.

### Exp 13 — slab-batch TextFragment + Lines allocations — KEEP

frag := &TextFragment{...} and the 1-element Lines slice were 56% of
remaining object count (2 allocs/hunk). 256-entry slabs; cap-pinned
subslices. -6.92% time, -49.4% allocs (55.3k -> 28.0k). Full battery pass.

### Exp 14 — defer fallback-name parse + (rejected) per-file slab — KEEP partial

Deferring parseHeaderPathPair to finishFile (runs only when no header
line named the file): time-neutral, -12% allocs. KEEP.
Slab-batching gitdiff.File: +3.7% time — the consumer goroutine reads
file N while the parser writes N+1 into the adjacent slab slot: false
sharing across cores. REJECT; documented in code. Fragment slabs don't
suffer this (whole file emitted before consumer touches fragments).

### Exp 15 — end-to-end scan validation + wall-time noise lesson — DONE

Full `betterleaks git` runs, baseline (17c568c) vs HEAD binaries:
- self repo: findings byte-identical (6/6); scan 3.3-3.5s both.
- gitlab-foss full history: findings identical (4695/4695; deep key
  compare incl. Line/Match/Secret/columns).
- gitlab-foss -n 400, --git-workers 8, interleaved x3:
  base user=40.5-40.7s wall=34.8-35.0s; new user=40.0-40.8s
  wall=34.7-35.1s — parity. End-to-end on this repo shape is dominated
  by git-subprocess CPU and the 34s wall floor; the parser's ~20ms/46MB
  is a small share. Wins surface as freed CPU/GC headroom, not wall.
- A one-off full-history A/B suggested the slab commit cost +27% wall;
  interleaved + single-worker repeats showed it was machine-load noise
  (same binary varied 1m23s-2m9s across runs; single-worker all variants
  34.4-35.0s). Slabs KEPT on parser-bench evidence (-6.9%, n=10).
  Lesson: full-scan wall times on a shared box need interleaved repeats.

### Exp 16 — window size sweep (256KB / 512KB / 1MB) — KEEP 512KB

1MB: ~ (p=0.14); 256KB: +5.2% (more refills + compactions). 512KB stays.

### Exp 4 — inline 32-byte SWAR pre-scan in readLine before IndexByte — REJECT (see above)

### Exp 3 — batch newline pre-indexing per refill (SWAR + IndexByte variants) — REJECT

Hypothesis: replace per-line IndexByte with one bulk newline scan per
512KB refill (positions into []int32), making readLine a pure index pop.
Tried (a) 8-byte SWAR zero-map scan, (b) IndexByte-driven bulk index.
Results vs exp2: (a) +3.9% slower, (b) +19.0% slower. Why: the window is
already fully cache-resident when parsed line-by-line — a second full
pass doubles L1/L2 traffic and the index adds append+load per line, while
per-line IndexByte starts hot exactly at the line head. NEGATIVE RESULT:
scan-once-per-line is already bandwidth-optimal for ~30-byte lines.
Reverted to exp2 code (re-benchmarked: within noise of exp2).

### Exp 17 — port session optimizations onto a5fecc0 native-record architecture — KEEP

a5fecc0 (pushed to origin/perf/05-fast-gitlog-parser after this session
baselined) added scanner-native records (fastGitFile/fastGitFragment —
no gitdiff object graph on the scan path), 64-file batches, and context
cancellation — but kept the bufio.ReadSlice + strings.Builder internals.
This branch (perf/05-native-plus-session, 66a9d13) rebases the session's
byte-level wins onto that architecture: window line reader (512KB),
arena + unsafe.String aliasing + 2x growth floor, local cursors +
inlined readLine fast path, deferred fallback-name parse, and the
small-window torture test extended to exercise BOTH emit paths.
The session's TextFragment/Lines slabs were intentionally dropped:
native records eliminate those objects entirely — the cleaner fix.

Final interleaved 5-way (self-history 46MB corpus, n=8, one harness):

| state             | sec/op | vs pre | B/op    | allocs/op |
|-------------------|--------|--------|---------|-----------|
| pre (17c568c)     | 50.65m | —      | 117.2Mi | 89.6k     |
| session (1950dd8) | 31.55m | -37.7% | 49.4Mi  | 24.6k     |
| a5fecc0 native    | 48.92m | -3.4%  | 114.1Mi | 53.1k     |
| ported native     | 30.97m | -38.9% | 46.9Mi  | 15.5k     |
| ported compat     | 34.09m | -32.7% | 49.5Mi  | 52.0k     |

Ported-native is the best state on every metric: 5.8x fewer allocs than
pre, 3.4x fewer than a5fecc0 alone, faster than the session branch, and
architecturally cleaner (no slabs, no false-sharing caveat — batches are
fully written before the consumer sees them). The two changesets compose
nearly additively because they removed different parts of the same
allocation pressure.

Validation: full suite, 3x60s fuzz (all targets), gitlab-foss corpus
replay x4 date formats, race (incl. a5fecc0's concurrency tests),
small-window torture on native+compat; e2e findings deep-identical on
self (6/6) and gitlab-foss full history (4695/4695). E2e wall/CPU parity
with all other states (git-subprocess-bound); maxrss flat at 572MB.
