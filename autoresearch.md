# Autoresearch: Deep research: Reduce overall scanner scan time by optimizing regex/detector regex work while preserving exact findings and semantics; focus on additional techniques beyond the existing semi-generic prefix-stripped rejection gate.

## Objective
Reduce overall scanner scan time by optimizing regex/detector regex work while preserving exact findings and semantics; focus on additional techniques beyond the existing semi-generic prefix-stripped rejection gate.

## Metrics
- Primary: quality_gap (gaps, lower is better)
- Secondary: none yet

## How to Run
`./autoresearch.sh` prints `METRIC name=value` lines.

## Files in Scope
- autoresearch.research/regex-scan-time

## Off Limits
- Do not change reported findings, captures, filters, validation, required-rule
  behavior, or JSON-visible report semantics.
- Do not broaden regex gates without an implication proof and differential
  report evidence.

## Constraints
- Decision contract: quality_gap is treated as a quality-bearing score; faster runs should not be promoted when component evidence shows quality or correctness erosion.
- Keep research notes under autoresearch.research/regex-scan-time.
- Use source-backed evidence before implementing recommendations.

## Decision Rules
- Keep when the primary metric improves or a baseline is needed and checks pass.
- Discard when the metric is equal or worse, unless the run only establishes the baseline.
- Log crashes and failed checks with a concrete rollback reason.
- Put next-step guidance in ASI so another Codex session can continue.

## Stop Conditions
- Stop when the target metric reaches the agreed threshold.
- For qualitative loops, stop when `quality_gap=0`, checks pass, and no high-impact open finding remains.
- Stop when maxIterations is reached or the user interrupts.

## Research Notes
- Source-backed facts, contradictions, and open questions go here or in linked scratchpad files.
- For deep research loops, link the scratchpad folder and summarize the current synthesis.

## What's Been Tried
- Run 1 recorded the initial research quality gap: `quality_gap=6`.
- Run 2 recorded the closed research checklist: `quality_gap=0`.
- Implemented a guarded operator-byte rejection gate for recognized
  semi-generic generated-shape rules.
- Focused benchmark now compares the operator-byte gate against prefix-regex
  gates only in the same binary: `655656345` ns/op vs `1274015995` ns/op
  (`1.943115x`) on the synthetic no-operator miss fixture.
- Kept broad mandatory atom extraction, candidate-window scanning, and
  required-rule shortcuts deferred pending stronger measurement and semantic
  proof.
- Added opt-in gate counters and measured real file-tree selectivity. Current
  default config installs 148 prefix gates, 136 generated operator-byte gates,
  and 23 mandatory-atom gates.
- Kept per-decode-pass caching for the generated operator-byte predicate:
  `required_any_cache_speedup_ratio` was `1.026421x` on no-operator misses,
  `1.004635x` on punctuation-heavy misses, and `1.001213x` on the positive
  fixture.
- Kept narrow parser-backed mandatory atom gates for ASCII required literals not
  already covered by keywords. Inventory found 23 qualifying default rules;
  sampled real repos had zero simulated false rejects.
- Rejected minimum-length gates for this patch: sampled repos rejected 0, 4, 0,
  11, and 76 candidate checks respectively, including only 76 out of 454k
  candidate checks on gitlab-foss.
- Added `scripts/compare_reports_strict.py` and used it to verify strict
  baseline/current report equivalence across self, aho-corasick, go-gitpack,
  gitleaks, prometheus, and gitlab-foss.
- Expanded benchmark matrix:
  - no-operator keyword miss: `625876394` ns/op current vs `1279881497`
    prefix-regex-only (`2.044943x`).
  - punctuation-heavy keyword miss: `1499068018` ns/op current vs `1550948284`
    prefix-regex-only (`1.034608x`).
  - positive fixture: `197846` ns/op current vs `198674` prefix-regex-only
    (`1.004187x`).
- Cross-repo hyperfine is mixed at wall clock. Directory scans ranged from
  `-8.25%` to `+2.46%` wall, with user CPU lower in every sample. Git-history
  scans showed a material aho-corasick win (`-21.05%`) and small neutral/noisy
  regressions on go-gitpack (`+1.92%`), gitleaks (`+1.32%`), prometheus
  (`+0.13%`), and gitlab-foss `-n 100` (`+0.44%`).

## Resume This Session

Use these commands to pick the loop back up without rediscovering state:

```bash
node /local/home/ahrav/.codex/plugins/cache/TheGreenCedar/codex-autoresearch/2.5.1/scripts/autoresearch.mjs state --cwd /local/home/ahrav/scratch/bl-regex-scan-time
node /local/home/ahrav/.codex/plugins/cache/TheGreenCedar/codex-autoresearch/2.5.1/scripts/autoresearch.mjs doctor --cwd /local/home/ahrav/scratch/bl-regex-scan-time --check-benchmark
node /local/home/ahrav/.codex/plugins/cache/TheGreenCedar/codex-autoresearch/2.5.1/scripts/autoresearch.mjs next --cwd /local/home/ahrav/scratch/bl-regex-scan-time
node /local/home/ahrav/.codex/plugins/cache/TheGreenCedar/codex-autoresearch/2.5.1/scripts/autoresearch.mjs log --cwd /local/home/ahrav/scratch/bl-regex-scan-time --from-last --status keep --description "Describe the kept change"
node /local/home/ahrav/.codex/plugins/cache/TheGreenCedar/codex-autoresearch/2.5.1/scripts/autoresearch.mjs export --cwd /local/home/ahrav/scratch/bl-regex-scan-time
```

## Run Ledger

<!-- AUTORESEARCH_RUN_LEDGER:START -->
- Run 1 measure: Baseline quality_gap measurement; metric=6; best=unknown.
- Run 2 measure: Measured closed regex-scan-time research gaps after guarded operator-byte gate implementation, with go test, focused benchmark, and normalized report-equivalence evidence recorded outside Autoresearch.; metric=0; best=unknown.
- Run 3 measure: Measured closed exhaustion checklist after counters, operator-byte cache, mandatory atom gate, strict comparator, benchmarks, and cross-repo report equality.; metric=0; best=unknown.
<!-- AUTORESEARCH_RUN_LEDGER:END -->
