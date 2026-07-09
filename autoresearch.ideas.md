# Autoresearch Ideas: Reduce overall scanner scan time by optimizing regex/detector regex work while preserving exact findings and semantics; focus on additional techniques beyond the existing semi-generic prefix-stripped rejection gate.

- Per-decode-pass operator-byte cache for the exact generated semi-generic
  operator character set.
- Conservative `regexp/syntax` mandatory-atom extraction, starting with ASCII
  literal concatenations and failing open for alternation/casefold complexity.
- Candidate-window scanning only after strict decoded-offset and required-rule
  report equivalence exists.
- Regex-set or literal-set prefiltering as a rejection layer, not as a match
  producer.
- Cold-path benchmark covering detector construction and first gate/full-regex
  compilation.
- Minimum-length and required-character-set gates for fragments that survive
  keyword prefiltering.
