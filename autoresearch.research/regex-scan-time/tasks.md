# Research Tasks: Reduce overall scanner scan time by optimizing regex/detector regex work while preserving exact findings and semantics; focus on additional techniques beyond the existing semi-generic prefix-stripped rejection gate.

## queued
- Add per-rule gate counters before claiming broad production scan-time gains.
- Add punctuation-heavy, positive, decoded, and cold benchmark variants.
- Recompute installed gate coverage after the top-level-alternation guard if a
  future report needs exact coverage counts.

## in_progress
- None.

## done
- Scratchpad initialized.
- Phase 1 through Phase 5 deeper-research outputs persisted under
  `.claude/research-state/2026-07-09-reduce-overall-scan-time/`.
- Implemented narrow generated-shape operator-byte rejection with fail-open
  compilation and merged-alternation guard.
- Implemented zero-capture `FindStringSubmatch` skip.
- Added focused regression tests and synthetic detector benchmark.
- Verified `go test ./...`, focused benchmark, and normalized baseline/current
  report equivalence.

## blockers
- None.
