# Production A/B/C experiment design — predeclared

Date: 2026-07-11. Host: 64-core Graviton (aarch64), 123.5 GiB RAM, Amazon Linux 2023,
kernel 6.12.92. Corpus: /local/home/ahrav/scratch/gitlab-foss frozen snapshot
(HEAD 754df18208dd903e1b05c7a4343da272079bea3d,
ref digest deed23c8c84f6c1ae6037989040f5b9b7f3ebbe7a395f896ec86ee565436783c,
rev digest e2e8f26e5fed457385c0b782346d05daee3bd31dbf4ea4d0eba13aeffb4e6c59,
197,178 commits — five more than the 2026-07-10 macOS snapshot; this is a
*different frozen snapshot* and all expectations are re-derived on it).

## Binaries (pinned)

- Stock Git sibling: git v2.51.0 built from c44beea485, flags
  `NO_CURL=1 NO_GETTEXT=1 NO_TCLTK=1 NO_PERL=1 NO_PYTHON=1 NO_EXPAT=1`, -O2 default.
  sha256 45283e3c6cea6d9af55132bbfe3103dd35b930b3de3b42f6bb45bcaa6a404081
- Patched Git sibling: same source revision + contrib/gitengine/custom-git/
  betterleaks-diff-engine.patch, same flags.
  sha256 dcb6b9f9c08c6c6897665517a8ebb78f78997e7b6d4cf68a173d04e61ca9e8f2
- System git (deployment baseline): /usr/bin/git 2.50.1 (distro build).
- betterleaks-ab: branch perf/06-engine-production-ab (worktree gitperf-ab),
  engine opt-in commit; go1.26.4 linux/arm64.
  sha256 recorded at build in RESULTS notes.

## Arms

| Arm | Mechanism | Env |
|---|---|---|
| A_stock | production text path (git log --no-walk --stdin -p -U0 per batch, fast parser), stock sibling | BETTERLEAKS_GIT_BIN=stock-git |
| C1 | native-frame engine, helper recycled after every batch (ephemeral, no cross-batch retention) | BETTERLEAKS_GIT_BIN=stock-git BETTERLEAKS_GIT_ENGINE_BIN=patched-git BETTERLEAKS_GIT_ENGINE_RECYCLE=1 BETTERLEAKS_GIT_ENGINE_STRICT=1 |
| Cp | native-frame engine, persistent helper per worker | BETTERLEAKS_GIT_BIN=stock-git BETTERLEAKS_GIT_ENGINE_BIN=patched-git BETTERLEAKS_GIT_ENGINE_STRICT=1 |
| A_sys | deployment baseline: text path with system git 2.50.1 | (no BETTERLEAKS_GIT_BIN) |

Arm B (persistent stock text) is replaced by C1: `git diff-tree --stdin` cannot
apply mailmap, and this corpus has mailmap-remapped authors, so a persistent-text
arm cannot pass the findings-equivalence gate. The A/C1/Cp triple still separates
the two mechanisms: native-frames effect = C1 − A_stock (equal process lifetime,
one helper per batch), persistence effect = Cp − C1 (equal mechanism, different
lifetime). A_sys − A_stock isolates the Git version/build difference for the
deployment comparison.

## Fixed factors

- Workers: `--git-workers 32` in every arm (prior on-box data: w32 vs w64 within
  4%; 64 persistent helpers would risk memory-censoring the Cp arm).
- Scheduler/batching: identical code (shared buildBatches, shared queue).
- Rules/config: default embedded betterleaks config; no repo-local config file
  exists in the working tree.
- Command: `betterleaks-ab git /local/home/ahrav/scratch/gitlab-foss --exit-code 0`
  (no report path in timed runs; findings count captured from the summary line).
- Git env: gitConfigIsolationEnv (config isolation, deltaBaseCacheLimit=128m,
  MALLOC_ARENA_MAX=2, no zlib preload) applies to all subprocesses in all arms.
- Detector Sema: default 40.

## Gates before timing (per arm)

1. Record-level: custom engine must reproduce the stock-reference canonical
   multiset digest on the full corpus (bench harness, hard expectations).
2. Findings-level: one scan per arm with --report-path; all pairs must be
   equivalent under scripts/compare_reports.py and have identical counts.
3. Zero helper/protocol errors; STRICT mode so no silent fallback.
4. t4218 protocol tests and Go engine/sources suites pass (incl. race).

## Series

- Pilot: 3 runs/arm, rotated order, to estimate execution-level CV.
- Minimum worthwhile effect: 5% end-to-end wall.
- Confirmatory n per arm: max(8, ceil(15.7 * (CV/0.05)^2)) (two-sample normal
  approximation, alpha=0.05, power=0.80). Horizon fixed before the series.
- Order: Latin-square rotation per block (A_stock,C1,Cp,A_sys / C1,Cp,A_sys,A_stock / ...).
  Exact executed order recorded in the JSONL.
- Censoring: any run exceeding 300 s wall (>3x the ~88 s expected baseline) is
  killed and recorded as censored, not timed. Memory floor: MemAvailable < 6 GiB
  aborts and censors the run.
- Warm-cache series: pack is 2.0 GiB on a 123 GiB host; page cache stays warm.
  Every run is preceded by the same corpus digest check touching the same pack.
  No cold-storage claims will be made from this series.

## Metrics per run (measure_scan.py)

wall; rusage user/sys CPU of the waited tree; sampled CPU split by process
class (scanner vs git log vs engine helper vs cat-file); peak aggregate and
individual RSS per class; peak aggregate PSS per class (physical, shared-page
corrected); minor/major faults; block I/O; helper count peak; MemAvailable floor;
findings count from the summary line; helper_starts (engine arms, from scan log).
