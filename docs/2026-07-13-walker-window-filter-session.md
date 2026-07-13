# Session: walker stat deferral, AhoC window positions, filter-runtime gate

> **Status:** experiment record for the perf/27–perf/29 stacked branches.
> Reviews three externally proposed optimizations against the stack tip
> (`betterleaks-stack-prs` @ `335cec2`): two keeps, one evidence-backed
> discard.
>
> - Corpora: gitlab-foss checkout (`~/scratch/gitlab-foss`, 70,322 files,
>   2.7 GB) for dir mode; self-repo and gitlab-foss history (117k commits)
>   for git mode.
> - Harnesses: `scripts/benchcpu2.sh` (git mode), `scripts/benchdir2.sh`
>   (dir-mode sibling, added in perf/27). CPU-primary (getrusage children),
>   cgroup `memory.peak`, load gate, findings digest on every row.
> - Keep gate: byte-identical findings digest + measured CPU win.

---

## Measurement validity: the fork pin incident

The `go.mod` replace points the prefilter at the shared checkout
`/local/home/ahrav/scratch/aho-corasick`. During this session another
session was **actively committing to that checkout** (branch
`perf/15-dual-table-scans`, commits at 04:04→04:45); the first
measurement round unknowingly embedded **three different prefilter
libraries** in the three binaries (verified via binary mtimes vs the
fork's reflog). Those rows are struck from the record.

Fix, and the pattern for future sessions: freeze the fork at the pin the
stack tip documents (`f5db8d1`, `docs/27-chain-index`, named in
`335cec2`'s message) via a read-only worktree plus a `go.work` **outside
the repo** whose workspace replace overrides `go.mod`:

```
# /tmp/.../go.work
go 1.25.0
use <betterleaks checkout>
replace github.com/BobuSumisu/aho-corasick => ~/scratch/aho-corasick-pin-f5db8d1
```

Every binary below embeds exactly `f5db8d1`. The committed `go.mod` is
untouched. Contaminated-round digests all matched regardless (the fork
revisions were findings-neutral), so correctness evidence carried over;
only the CPU deltas were invalid.

## Results at the pinned fork (all digests byte-identical)

| Corpus | base `335cec2` | perf/27 | perf/28 | digest |
|---|---|---|---|---|
| dir gitlab-foss, cpu core-s (5 runs, σ≤0.12) | 23.76 | 23.04 (−3.0%) | 21.12 (**−8.3%** vs 27) | `06ba854e`/282 |
| git self, cpu core-s (5 runs) | 12.71 | n/a (walker unused) | 12.12 (−4.6%) | `73ed1ee6`/6 |
| git gitlab-foss, cpu_min core-s (3 runs) | 1679.6 | n/a | 1683.2 (+0.2%, noise) | `f1370fb4`/4709 |

Baseline dir-scan profile attribution (20.17 core-s sampled):
`ruleWindows` 2.07 s cum (10.3%, 94% inside `bytes.Index`), `os.Lstat`
via `d.Info()` 0.45 s (2.2%), expr VM 0.78 s (3.9%).

## perf/27 — walker stat deferral + per-entry logger removal (KEEP)

`sources/files.go`. Both walkers (parallel default, serial WalkDir
reference) stat'd every entry though the size feeds only the filescan
experiment and the `--max-target-megabytes` gate (default off), and
built a contextual zerolog logger per entry. Now: stat only when
`FileScan != nil || MaxFileSize > 0`, in lockstep (the serial/parallel
differential test is the invariant); inline level events replace logger
contexts. Empty files are discovered at open time and yield zero
fragments — findings unchanged. dir CPU −3.0%; after the change the
dir-profile shows `Lstat` gone and `emitTarget` 0.94→~0.3 s cum.

## perf/28 — AhoC walk positions feed hit windows (KEEP)

`detect/detect.go`. Trigram mode already recorded occurrence positions
(occRecorder, perf/23) for window construction; default ahoc mode threw
Walk's `(end, n, pattern)` away and rediscovered occurrences with
`bytes.Index` per rule. Now the ahoc callback records `end+1−n` for
fragments ≥ `windowMinFragment`, same soundness contract (complete
lists or fall back). The fork's Walk reports every occurrence including
dict-link suffix matches — its differential fuzz target pins this.
dir CPU −8.3%, git-self −4.6%, git gitlab-foss neutral (git-bound,
small fragments dominate). `ruleWindows` drops to 0.23 s cum (1.3%).
New `TestAhoCRecordedPositionsEquivalence` pins Walk-fed positions to a
naive scan and recorded windows to the `bytes.Index` builder.

## Filter-runtime rewrite (PR2/PR3 proposal) — DISCARD, evidence below

Proposal: bounded match-window filter helpers
(`EvalFilterWithMatchWindow` / `containsAnyNearMatch`) or, cheaper, an
allocation-free `containsAny` (today: `strings.ToLower` copy + rrethy
`FindAllString` allocating `[]*Match` per eval,
`internal/exprruntime/bindings_filter.go:114–130`).

Profile gate across all three corpora, pinned binaries:

- dir gitlab-foss: `containsAny` absent from a 400-node cum listing;
  whole expr VM 0.63 s / 3.7%.
- git self: absent; expr VM 0.12 s / 2.8%.
- git gitlab-foss (heaviest, 246.92 core-s sampled): `containsAny`
  **0.25 s cum = 0.10%** (0.11 s FindAllString + 0.10 s ToLower).
  Expr VM total 13.53 s (5.5%), dominated by `matchesAny` regex
  4.84 s (already list-ID memoized; cost is RE2 matching, not
  allocation) and the tokenizer 2.37 s — neither addressed by the
  proposal.

Perfect-zero-cost bound: ~0.1% e2e. Risks: an ASCII-lowering fast path
diverges from `strings.ToLower` semantics on non-ASCII case-fold
partners (Kelvin sign → `k`), so a naive rewrite is not
findings-neutral in general. Filters run per candidate finding, not per
byte — the volume is structurally too low for this to matter on these
corpora. Revisit only if a corpus shows expr-VM share dominated by
`containsAny`, and prefer an all-ASCII fast path + fallback if so.
