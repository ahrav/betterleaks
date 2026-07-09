# Regex Scan-Time Exhaustion Backlog

Status: active second round. The first research round kept the narrow
semi-generic prefix/operator rejection gate and rejected broad claims as
under-measured. This backlog defines what still has to be proven before saying
we exhausted regex-cost avenues.

## Decision Standard

An avenue is exhausted only when one of these is true:

- Kept: the implementation preserves exact findings, passes strict report
  equivalence, and has targeted plus representative performance evidence.
- Rejected: the avenue has a concrete semantic risk, a measured slowdown, or no
  measurable selectivity under representative counters.
- Deferred: the avenue needs a separate design because the semantic blast radius
  exceeds this regex-gate patch.

All kept detector-side changes must preserve the invariant:

```text
full regex match => gate accepts => full regex still produces the finding
```

## Avenues

| ID | Avenue | Hypothesis | Proof Needed | Measurement Needed | Status |
|---|---|---|---|---|---|
| A1 | Gate selectivity counters | Existing gates save work only if they reject enough candidate fragments after keyword prefiltering. | Counters must not change findings and should be disabled by default or low overhead. | Gate attempts, byte rejects, regex-gate rejects/accepts, full regex calls, bytes to full regex, findings. | Kept |
| A2 | Per-fragment operator-byte flag | Repeated `strings.ContainsAny` per gated rule may be avoidable by computing the operator-byte fact once per decode pass. | Preserve the current `requiredAny` proof boundary; cache only the exact generated operator set. | Compare per-rule byte scan vs hoisted byte fact on no-operator, punctuation-heavy, and positive fixtures. | Kept |
| A3 | Parser-backed mandatory atom gates | `regexp/syntax` may find required literals longer or more selective than existing keywords. | Conservative language-inclusion proof for ASCII/case behavior; unsupported syntax fails open. | Counters showing atom gates reject after keyword prefiltering and beat their overhead. | Kept narrowly |
| A4 | Candidate-window scanning | Scanning bounded windows near keywords could reduce bytes passed to full regex. | Offset, decoded-segment, capture, context, filter, validation, required-rule, duplicate, and order equivalence. | Window size sensitivity plus strict report equivalence on positive/decoded/required-rule corpora. | Deferred |
| A5 | Regex batching / prefilter sets | A shared literal or regex-set prefilter could avoid per-rule regex invocation. | Must not replace full regex spans/captures; only rejection or candidate routing. | Candidate reduction and overhead versus existing Aho-Corasick keyword prefilter. | Rejected for this patch |
| A6 | Cold compile behavior | Gate and full-regex lazy compilation may dominate short scans. | Compile failure must fail open; eager compile cannot drop rules silently. | Detector construction, first fragment, and short repo scans with and without gates. | Measured |
| A7 | Strict report comparator | Count equality can hide semantic drift. | Compare all JSON-visible semantic fields except explicitly volatile fields. | Use in every macro benchmark before timing claims are interpreted. | Kept |
| A8 | Positive-path capture work | Zero-capture skip is kept; further capture extraction shortcuts may exist. | Report fields, named captures, secret groups, entropy, and validation inputs unchanged. | Positive benchmarks split by zero-capture and capture-heavy rules. | Kept zero-capture only |
| A9 | Minimum-length or character-class prechecks | Some patterns cannot match short fragments or fragments without required char classes. | Conservative lower-bound or char-set proof over whole regex, alternation-aware. | Reject rate after keyword prefilter and overhead versus direct regex. | Rejected |
| A10 | Rule inventory and custom-rule safety | Default-rule proof is not enough if custom rules can match generator-like text. | Gate attachment remains conservative for arbitrary configs. | Inventory of installed gates, skipped gates, and fail-open gates. | Kept conservative |

## Current Evidence Boundary

- Targeted synthetic miss benchmark: operator-byte gate was about `1.94x` faster
  than prefix-regex gate only on the keyword-rich no-operator fixture.
- Macro `git` scans across several repos were mostly neutral; one repo improved
  materially, and several were within noise or git-bound.
- Current expanded detector benchmark: current gates are `2.044943x` faster than
  prefix-regex-only on no-operator keyword misses, `1.034608x` faster on
  punctuation-heavy keyword misses, and `1.004187x` faster on the positive
  fixture.
- Directory hyperfine: aho-corasick `-4.34%`, go-gitpack `-8.25%` noisy,
  gitleaks `-0.92%`, prometheus `-0.38%`, self `+0.07%`, gitlab-foss `+2.46%`
  noisy wall; user CPU fell in every directory sample.
- Git-history hyperfine: aho-corasick `-21.05%`; go-gitpack `+1.92%`,
  gitleaks `+1.32%`, prometheus `+0.13%`, gitlab-foss `-n 100` `+0.44%`.
- Strict report comparison was equivalent on self, aho-corasick, go-gitpack,
  gitleaks, prometheus, and gitlab-foss.

## Completion Criteria

- A1 and A7 are implemented.
- A2 through A6 and A8 through A10 have a kept/rejected/deferred decision with
  correctness and performance evidence.
- Cross-repo benchmark reporting distinguishes detector CPU wins from git I/O
  or git-diff bottlenecks.
- Final writeup should state both new functionality and actual savings by
  workload, including the small git-history regressions.
