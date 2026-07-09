# Research Sources: Reduce overall scanner scan time by optimizing regex/detector regex work while preserving exact findings and semantics; focus on additional techniques beyond the existing semi-generic prefix-stripped rejection gate.

| Source | Date Checked | Claim Supported | Confidence |
| --- | --- | --- | --- |
| `cmd/generate/config/utils/generate.go` | 2026-07-09 | `GenerateSemiGenericRegex` always places a generated operator between identifiers and the captured secret for standalone generated semi-generic rules. | High |
| `detect/rule_gate.go` | 2026-07-09 | Prefix-stripped gates are rejection-only; the added operator-byte gate fails open on gate compile error, disables byte rejection for top-level alternation, and only applies to recognized generated operator/secret shapes. | High |
| `cmd/generate/config/rules/atlassian.go` | 2026-07-09 | Default config includes a merged regex where one branch is semi-generic and another `ATATT3...` unique-token branch can match without an assignment operator byte. | High |
| `detect/rule_gate_test.go` | 2026-07-09 | Tests cover generator-shape recognition, operator-byte rejection, top-level alternation disabling, full-match implication samples, default-config equivalence, and the Atlassian operator-free merged-rule regression. | High |
| `detect/detect.go` | 2026-07-09 | The detector checks gates before `FindAllStringIndex`, then still uses the original full regex to produce findings; zero-capture rules skip the second submatch regex scan. | High |
| `scripts/bench_regex_scan.sh` | 2026-07-09 | Focused detector benchmark compares current operator-byte gates with prefix-regex gates only; final result was `655656345` ns/op vs `1274015995` ns/op (`1.943115x`) on the synthetic keyword-rich no-operator miss fixture. | Medium |
| `/tmp/bl-regex-hyperfine.json` | 2026-07-09 | End-to-end self-scan macro timing was neutral/noisy: baseline `0.423485s +/- 0.013134`, current `0.418606s +/- 0.016902`; reports still matched. | Medium |
| `/tmp/bl-regex-baseline-report.json` vs `/tmp/bl-regex-current-report.json` using `scripts/compare_reports.py` | 2026-07-09 | Baseline and current self-scan JSON reports were equivalent after ignoring `Fingerprint`. | High |
| `.claude/research-state/2026-07-09-reduce-overall-scan-time/phase5-final.md` | 2026-07-09 | Adversarial synthesis keeps the narrow operator-byte gate, downgrades broad mandatory atoms and candidate-window scanning, and requires measurement before broad scan-time claims. | High |
