# Research Plan: Reduce overall scanner scan time by optimizing regex/detector regex work while preserving exact findings and semantics; focus on additional techniques beyond the existing semi-generic prefix-stripped rejection gate.

## Workstreams
- Current implementation and architecture evidence: complete for the current
  narrow gate patch.
- High-impact improvement candidates: operator-byte gate accepted narrowly;
  broad mandatory atoms, candidate windows, and required-rule shortcuts deferred.
- Risks, constraints, and validation strategy: captured in Phase 5 synthesis and
  the Phase 6 implementation mapping.
- Follow-up measurement: add counters and a broader benchmark matrix before
  making broad scan-time claims.

## Sequencing
- Gather evidence first. Done.
- Synthesize findings into `synthesis.md`. Done.
- Convert actionable findings into `quality-gaps.md`. Done.
- Iterate with the Codex Autoresearch skill until `quality_gap=0`. Current
  round is ready for a final quality-gap measurement.
