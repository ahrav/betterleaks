# Research Brief: Reduce overall scanner scan time by optimizing regex/detector regex work while preserving exact findings and semantics; focus on additional techniques beyond the existing semi-generic prefix-stripped rejection gate.

## Request
Reduce overall scanner scan time by optimizing regex/detector regex work while preserving exact findings and semantics; focus on additional techniques beyond the existing semi-generic prefix-stripped rejection gate.

## Decision To Support
- Identify source-backed changes worth testing through an autoresearch loop.

## Success Criteria
- The project essence is accurate.
- Sources and direct evidence are logged.
- High-impact findings are converted into quality gaps.
- Each implemented or rejected gap has evidence.

## Constraints
- Findings and report semantics must be byte-for-byte equivalent except for
  explicitly volatile fingerprint fields.
- Rejection gates may skip a full regex only when the full regex cannot match
  the same fragment.
- The full rule regex remains the only source of matches, spans, captures,
  filters, validation, required-rule behavior, and report fields.
- New performance claims must distinguish targeted detector benchmarks from
  representative end-to-end scan time.

## Known Unknowns
- Real-corpus selectivity for operator-byte gates is not measured yet.
- Cold detector and first-use gate compilation cost are not measured yet.
- Broad mandatory atom extraction and candidate-window scanning need separate
  semantic proofs before implementation.
