# Production A/B/C verdict: persistent custom-Git engine vs ParallelGit

Date: 2026-07-11. Branch: `perf/06-engine-production-ab`.
Host: 64-core Graviton (aarch64), 123.5 GiB RAM, Amazon Linux 2023, kernel
6.12.92. Warm page cache throughout (pack 2.0 GiB; zero block-input
operations and ≤121 major faults in every run).

## Verdict

**The custom native-frame engine is faster than the current optimized
ParallelGit production path on the frozen GitLab FOSS corpus: 26.0% lower
median end-to-end wall against the deployed baseline (system git 2.50.1;
bootstrap 95% CI 8.6%–34.5% of baseline median, Mann-Whitney p=0.0035, n=11
per arm) and 25.3% lower against the same-source stock-Git sibling (CI
17.5%–30.1%, p=0.0001) — but the win comes from the scanner side, not the
Git side, and process persistence contributes nothing.**

## Experiment design

Same scheduler, batch ownership (`buildBatches`), worker count (32), rules
(default config), detector, host, and frozen snapshot for every arm. The
engine was wired into the real `betterleaks git` scan path behind
`BETTERLEAKS_GIT_ENGINE_BIN` (opt-in, preflight-only fallback,
`_STRICT` mode used in all benchmark arms so a fallback can never
masquerade as an engine run). `DiffFilesCh` and `--log-opts` behavior are
unchanged; user log-opts always take the text path.

Arms (Latin-square rotated, 11 blocks total, predeclared horizon, 300 s
censoring threshold, per-run snapshot digest checks):

- **A_stock** — production text path (`git log --no-walk --stdin -p -U0` +
  fast parser), stock git v2.51.0 sibling built from the same pinned source
  (c44beea485) with the same flags as the patched binary.
- **C1** — native-frame engine, helper recycled after every batch
  (no cross-batch persistence; isolates the native-frame mechanism).
- **Cp** — native-frame engine, one persistent helper per worker
  (adds persistence on top of C1).
- **A_sys** — deployment baseline: text path, distro git 2.50.1.

Arm B from the original plan (persistent stock-text `diff-tree --stdin`)
was dropped for cause: `diff-tree` cannot apply `--use-mailmap`, and this
corpus has mailmap-remapped authors, so that arm cannot pass the
findings-equivalence gate. C1 replaces it as the persistence/mechanism
separator.

Snapshot (differs from the 2026-07-10 macOS snapshot — all expectations
re-derived): 197,178 commits, ref digest `deed23c8…`, rev digest
`e2e8f26e…`, HEAD 754df18208dd903e1b05c7a4343da272079bea3d.

## Correctness evidence (gates passed before any timing)

- Patched Git t4218 protocol suite: 8/8 pass.
- Go `internal/gitengine` + `sources` suites pass, including race subset;
  a race-built engine scan of the self repo is clean.
- Record gate: the engine reproduced the stock-reference canonical multiset
  digest on the full corpus — 24,253,390 records (197,178 commits /
  2,820,879 files / 21,235,333 hunks), 5,495,475,641 canonical bytes,
  digest `49139d357cb54d84b515204f23ef221be21fe1210287101cda1002cda1393b93`.
- Findings gate: all four arms produced 4,709 findings;
  `scripts/compare_reports.py` reports equivalence for every pair
  (order-only differences). Every one of the 44 timed runs contains the
  `leaks found: 4709` marker; zero helper/protocol errors; helper_starts
  as expected (32 for Cp, 257 for C1 per scan).

## Benchmark table (admitted n=43; one C1 run quarantined for documented
concurrent-load contamination)

| Arm | n | Wall p50 (s) | p25–p75 | min–max | CPU p50 (u+s, s) | Git-side CPU (s) | Helper agg PSS p50 | Helper agg RSS p50 | Findings |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| A_stock | 11 | 111.89 | 105.8–115.1 | 88.5–138.1 | 4,032 | ~1,076 | 8.07 GiB | 17.78 GiB | 4,709 ✓ |
| C1 (engine, recycle-1) | 10 | 83.55 | 80.7–87.0 | 80.2–93.4 | 2,511 | ~1,112 | 7.88 GiB | 17.94 GiB | 4,709 ✓ |
| Cp (engine, persistent) | 11 | 85.39 | 83.2–91.0 | 80.1–103.1 | 2,528 | ~1,097 | 10.33 GiB | 39.72 GiB | 4,709 ✓ |
| A_sys (deployed baseline) | 11 | 115.39 | 98.1–121.3 | 81.2–130.3 | 4,228 | ~1,080 | 7.92 GiB | 17.45 GiB | 4,709 ✓ |

Pairwise (median difference, percentile-bootstrap 95% CI, two-sided
Mann-Whitney):

| Comparison | Δ median wall | CI (s) | p |
|---|--:|--:|--:|
| C1 vs A_stock (mechanism A/B) | −28.35 s (−25.3%) | [−33.7, −19.5] | 0.0001 |
| Cp vs A_stock | −26.50 s (−23.7%) | [−31.4, −16.7] | 0.0003 |
| Cp vs C1 (persistence effect) | +1.85 s (+2.2%) | [−2.9, +9.5] | 0.40 |
| Cp vs A_sys (deployment A/B) | −29.99 s (−26.0%) | [−39.8, −9.9] | 0.0035 |
| A_stock vs A_sys (git version/build) | −3.50 s (−3.0%) | [−15.3, +17.3] | 0.92 |

RSS vs PSS: Cp's 39.7 GiB aggregate helper RSS is dominated by the shared
pack mmap counted 32 times; PSS (shared pages divided) is 10.3 GiB. Summed
RSS is not unique RAM consumption. Peak individual helper RSS: 1.36 GiB
(Cp) vs 0.90–0.93 GiB (others).

## Attribution: where the speedup actually comes from

1. **Persistence: nothing.** Cp vs C1 is +2.2% (not significant) at 3.9 GiB
   more helper working set (PSS) — 32 vs 257 helper starts per scan. Process
   start/cache-retention effects are immaterial at this batch size
   (~2,400 commits/batch) on a warm corpus. This contradicts the
   2026-07-10 macOS three-run series (persistent 9% faster than recycle-1);
   that series is superseded for this host by n=21 admitted runs.
2. **Git-side work: unchanged.** Git-side CPU is ~1.1 kcpu-s in all four
   arms. Native frames do not materially reduce Git's own cost — patch-text
   formatting is cheap next to inflate/diff. The old idea that the engine
   saves Git formatting work is refuted as a mechanism.
3. **The real mechanism is scanner-side.** The text-path Go scanner
   stochastically enters a high-CPU state: sampled scanner CPU 2.6–4.4
   kcpu-s in 17/22 text runs vs 1.2–2.4 kcpu-s in 20/21 engine runs.
   In the low state the text path's wall (~88.5 s pooled median) is within
   ~4% of the engine (~84.7 s). GC-assist was refuted by GODEBUG=gctrace
   (528 GC cycles, ~20 CPU-s total GC on a high run — two orders of
   magnitude short); GOMAXPROCS=32 did not eliminate it. The state's root
   cause is unresolved; candidates are scheduler/pipe-read interaction with
   32 bursty pipes and allocation-pattern differences feeding go-re2.
   The engine path (typed frames, far fewer allocations, steadier feed)
   almost never enters the state — that stability is the bulk of the
   measured win.

## Integration recommendation

The result is sufficient to integrate the engine behind the existing
explicit opt-in (`BETTERLEAKS_GIT_ENGINE_BIN`), **in recycle-after-one
mode (C1)**: it keeps the entire wall win, holds aggregate helper PSS at
parity with the text path (7.9 vs 8.1 GiB), caps individual helper RSS at
~0.9 GiB, and avoids betting on helper lifetime. Persistent mode buys
nothing measurable here and costs 1.36 GiB/worker peak RSS growth.

It is *not* sufficient to change the production default: the win's dominant
mechanism is avoidance of an unattributed text-path scanner state, and if
that state is fixable in the text path directly, the residual engine
advantage on this corpus is ~4%. Attributing and fixing that state is
cheaper to ship than a patched Git.

## Rejected candidates and negative results

- **Persistent helper processes (vs per-batch recycle):** +2.2% n.s.;
  rejected as a mechanism. Helper start cost (~257 starts) is invisible.
- **Persistent stock-text arm (`diff-tree --stdin`):** rejected before
  timing — no `--use-mailmap`, fails semantic equivalence on this corpus.
- **"Native frames reduce Git CPU":** refuted; git-side CPU arm-invariant.
- **"Our v2.51 sibling build is slower than distro git" (build/version
  confound):** refuted; A_stock vs A_sys −3.0% n.s. Both text arms show the
  same bimodal scanner behavior.
- **GC-assist as the high-CPU mechanism:** refuted by gctrace.
- **GOMAXPROCS reduction as a cheap fix:** one of two diagnostic runs still
  entered the high state; not a fix.

## Remaining high-potential experiments

1. Attribute the text-path scanner high-CPU state (perf profile a run once
   wall lags; suspect wazero/go-re2 feed patterns or pipe scheduling). A fix
   there may recover most of the engine's advantage with zero deployment
   cost — it also benefits every non-engine platform.
2. If the engine advances: re-run the memory tournament at higher worker
   counts (w64) where per-helper RSS growth matters more, and a cold-cache
   Linux run on a pack larger than RAM before any storage claims.
3. Rebase the patch onto each new Git release and re-run t4218 + record
   gate (maintenance cost is real: 1,619-line patch against pinned v2.51.0).

## Risks

- **GPLv2:** the patched Git is a derivative work; distribution requires
  source compliance. Keep the patch out of the MIT-licensed betterleaks
  binary; ship as a separate optional artifact or build recipe.
- **Fallback:** decided only at preflight (ERRO kind 5 before HELO), before
  any record is emitted; runtime failures are terminal, never mixed-engine.
  STRICT mode exists for benchmarking/CI.
- **Cancellation:** helper termination is signal-based with bounded grace;
  the race-built end-to-end run and lifecycle tests pass, but a CPU-bound
  helper in xdiff still takes up to the kill grace to die.
- **Memory:** persistent mode's per-helper RSS (1.36 GiB obs.) scales with
  workers; recycle-1 bounds it. PSS, not summed RSS, is the deployment
  metric.
- **Unsupported features:** merge diffs, copy detection, `--log-opts`,
  staged scans stay on the text path by design; textconv preflight and
  SHA-256 repos are covered by t4218.

## Storage/SPDK justification

**Not justified.** Every run in this series recorded zero block-input
operations and ≤121 major faults; object I/O is immaterial on this warm
corpus, and the measured bottleneck is scanner-side CPU, not storage. Any
mmap/io_uring/xNVMe/SPDK work would need a cold-cache, larger-than-RAM
corpus to even become measurable — and would then still be capped by the
~1.1 kcpu-s git-side share.

## Reproduction

Harness, raw JSONL (44 rows), provenance (binary hashes, corpus digests,
host state), predeclared design, and quarantine notes:
`/local/home/ahrav/scratch/gitperf/{results.jsonl,DESIGN.md,PROVENANCE.txt,
TOURNAMENT.md,measure_scan.py,run_arm.sh,run_series.sh,final_analysis.py,
series.err,gctrace-*.log}`. Engine opt-in code: `sources/git_engine_scan.go`
on this branch. Findings reports and record-digest runs:
`/local/home/ahrav/scratch/gitperf/findings-*.json`,
`{ref,custom}-digest-run.json`.
