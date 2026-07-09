# Research Synthesis: Reduce overall scanner scan time by optimizing regex/detector regex work while preserving exact findings and semantics; focus on additional techniques beyond the existing semi-generic prefix-stripped rejection gate.

## Project Essence
Betterleaks scans source fragments with default and custom secret-detection
regex rules. The regex layer is on the hot path, but findings depend on exact
match spans, capture groups, filters, validation, required-rule recursion,
suppression, and JSON-visible report fields. The safe optimization shape is a
rejection gate: skip a rule only when a cheap predicate proves the full regex
cannot match; otherwise run the original regex unchanged.

## High-Impact Findings
- Keep the prefix-stripped semi-generic regex gate. The optional
  `[\w.-]{0,50}?` prefix prevents RE2 from using strong literal-prefix
  acceleration, while stripping it is sound because a full match implies a
  suffix-position match in the same string.
- Add only narrow generated-shape operator-byte rejection. Standalone
  `GenerateSemiGenericRegex` rules require one of `=>:|?,` before the secret,
  so a fragment without those bytes cannot match that generated shape. The live
  gate is guarded by the generated operator/secret substrings and the generated
  suffix.
- Do not apply operator-byte rejection to top-level merged alternatives. The
  Atlassian default rule merges a semi-generic branch with an operator-free
  `ATATT3...` branch, so byte rejection there would drop real findings.
- Keep zero-capture positive-path cleanup. If a regex has no capture groups,
  `FindStringSubmatch` cannot change the emitted secret and is avoidable.
- Defer broad mandatory atom extraction and candidate-window scanning. Both can
  be made correct only with more syntax proof, offset/report proof, and
  selectivity counters.
- Treat the current detector benchmark as targeted evidence only. The final
  benchmark result is `655656345` ns/op for current operator-byte gates versus
  `1274015995` ns/op for prefix-regex gates only on a synthetic no-operator
  keyword-miss fixture; broad scan-time claims need representative macro
  measurements.

## Quality-Gap Translation
- The current round closes the research hygiene gaps, implements the narrow
  accepted gate, rejects/defer high-risk broad techniques with evidence, and
  records correctness checks plus targeted benchmark evidence.

## Confidence And Gaps
- Confidence is high that full-regex semantics are preserved for the current
  patch because gates are rejection-only, compile failure fails open, merged
  top-level alternatives lose the byte requirement, and report comparison found
  equivalent JSON-visible findings except fingerprints.
- Confidence is medium that the operator-byte gate materially helps broad scans.
  It nearly halves the targeted no-operator miss benchmark versus prefix-regex
  gates only in the same binary, but macro self-scan evidence was noisy and
  near neutral.
- Remaining gaps are measurement, not immediate source changes: add counters,
  punctuation-heavy miss benchmarks, positive benchmarks, decoded benchmarks,
  cold benchmarks, and benchstat discipline before claiming production scan-time
  gains.
