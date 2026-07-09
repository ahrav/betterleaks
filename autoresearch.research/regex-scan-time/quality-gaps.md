# Quality Gaps: Reduce overall scanner scan time by optimizing regex/detector regex work while preserving exact findings and semantics; focus on additional techniques beyond the existing semi-generic prefix-stripped rejection gate.

## Round 1: Narrow Gate Research

- [x] Project essence is accurate and source-backed.
- [x] Sources are logged with dates, claims, and confidence.
- [x] Synthesis separates high-impact changes from small QoL fixes.
- [x] Each high-impact recommendation is implemented or rejected with evidence.
- [x] Correctness checks pass after kept changes.
- [x] Final handoff includes dashboard or state evidence.

## Round 2: Exhaust Remaining Regex-Cost Avenues

- [x] Exhaustion backlog recorded with per-avenue proof and measurement criteria.
- [x] Gate selectivity counters added and measured on synthetic and real scan paths.
- [x] Per-fragment operator-byte flag evaluated against per-rule `ContainsAny`.
- [x] Parser-backed mandatory atom extraction evaluated and kept/rejected/deferred.
- [x] Candidate-window scanning evaluated and kept/rejected/deferred.
- [x] Regex batching or shared prefiltering evaluated and kept/rejected/deferred.
- [x] Cold compile and first-fragment behavior measured.
- [x] Strict report comparator or equivalent semantic equality suite applied.
- [x] Positive-path capture/candidate optimizations evaluated.
- [x] Minimum-length and character-class prechecks evaluated.
- [x] Cross-repo hyperfine suite scripted or documented with repeatable commands.
- [x] Final savings report separates detector microbenchmarks from end-to-end git scans.
