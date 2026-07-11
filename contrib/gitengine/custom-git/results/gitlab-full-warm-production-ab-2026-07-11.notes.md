# Production A/B/C tournament log

(Chronological; every entry: hypothesis → run → result → verdict.)

## Gates (all passed before timing)

- t4218 protocol suite: 8/8 pass (patched build, full `make all`).
- Go suites: internal/gitengine + sources pass, incl. race subset; race-built
  full engine scan of self-repo clean.
- Record digest: custom engine reproduced the stock-reference canonical
  multiset digest on the full 197,178-commit on-box snapshot
  (49139d35…, 24,253,390 records, 5,495,475,641 canonical bytes). exit=0.
- Findings digest: A_stock, C1, Cp, A_sys each produced 4,709 findings;
  scripts/compare_reports.py: all pairs equivalent (order-only diffs).
- Every timed run re-checks ref/rev snapshot digests before starting and
  embeds "leaks found: 4709" in its stderr tail (verified across rows).

## Pilot (blocks 0-2 + partial 3; n=3 per arm, C1 block1-pos0 quarantined)

| arm | wall median | wall range | rusage CPU med | note |
|---|---:|---:|---:|---|
| A_stock (text, v2.51 sibling) | 114.3 | 104.0-115.9 | 4,217 | all runs in "high-CPU mode" |
| C1 (engine, recycle-1) | 84.7 | 82.4-87.1 | 2,607 | all low mode |
| Cp (engine, persistent) | 85.7 | 84.5-91.7 | 2,528 | all low mode |
| A_sys (text, system 2.50.1) | 86.9 | 81.2-125.2 | 2,702 | bimodal: 2 low / 2 high |

### Finding 1 — git-side CPU is arm-invariant (~1.1 kcpu-s)

rusage(u+s) − sampled scanner CPU ≈ git-side CPU:
A_stock 4217−3141≈1076; Cp 2528−1430≈1098; A_sys(low) 2470−1375≈1095.
The native-frame engine does NOT materially reduce git-side CPU on this
corpus. Hypothesis that native frames avoid expensive patch-text formatting
in git: REJECTED as a git-side effect (formatting is cheap next to
inflate+diff).

### Finding 2 — the whole difference is scanner-side, and text is bimodal

Scanner (Go) sampled CPU: engine arms 1,388-2,005 s (6/6 runs); text arms
low mode 1,375-1,611 s, high mode 2,621-3,714 s. In low mode the text arm's
wall (81-87 s) equals the engine arms' wall. Suspected GC-assist mode when
parsing 4.2 GB of patch text; native records allocate far less. NOT yet
attributed (gctrace diagnostic scheduled post-series).

### Finding 3 — persistence is worth ~nothing here

Cp vs C1: +1.1% median wall (Welch t=0.78, CI −4.6..+9.8 s). Helper starts:
257 (C1) vs 32 (Cp). Process persistence/cache retention: NO measurable win
at this batch size on this warm corpus. (Contradicts the macOS 3-run series
where recycle-1 was 9% slower than persistent.)

### Sizing

Pooled low-mode CV ≈ 4-6%; but text arms are bimodal so variance is
mode-driven, not noise-driven. Predeclared horizon: 8 confirmatory blocks
(3-10), Latin-square rotation, no early stop. The bimodality will be
reported as mode frequencies rather than washed into a mean.

## Confirmatory series

Started 03:35 UTC, blocks 3-10 (8 blocks × 4 arms). Analysis after
completion; C1 gets n=10 admitted (one pilot row quarantined), others n=11.

## Quarantine log
EXCLUSION: arm=C1 label=block1-pos0 (original series) — contaminated by accidental concurrent duplicate scan 03:05:58-03:07:30 UTC; exclude from analysis; replacement C1 run to be appended
PREDECLARED 03:35 UTC: confirmatory horizon = 8 blocks (3..10), offset 3. No early stop. A_sys block1-pos2 (125.159s) retained (no documented contamination mechanism); C1 block1-pos0 remains quarantined (documented duplicate-scan overlap).
