#!/usr/bin/env bash
set -euo pipefail

tmp="${TMPDIR:-/tmp}/betterleaks-regex-scan-bench.$$"
trap 'rm -f "$tmp"' EXIT

go test ./detect -run '^$' -bench '^BenchmarkDetectFragmentSemiGeneric' -benchmem -count=5 | tee "$tmp"

awk '
  function record(bench, mode, value) {
    sum[bench, mode] += value
    n[bench, mode]++
  }
  function emit(bench, prefix) {
    cached = sum[bench, "operator_byte_gate"] / n[bench, "operator_byte_gate"]
    no_atom = sum[bench, "operator_byte_gate_no_atom"] / n[bench, "operator_byte_gate_no_atom"]
    per_rule = sum[bench, "operator_byte_gate_per_rule_scan"] / n[bench, "operator_byte_gate_per_rule_scan"]
    prefix_only = sum[bench, "prefix_regex_gate_only"] / n[bench, "prefix_regex_gate_only"]
    printf "METRIC %s_ns_per_op=%.0f\n", prefix, cached
    printf "METRIC %s_no_atom_ns_per_op=%.0f\n", prefix, no_atom
    printf "METRIC %s_mandatory_atom_speedup_ratio=%.6f\n", prefix, no_atom / cached
    printf "METRIC %s_per_rule_required_any_ns_per_op=%.0f\n", prefix, per_rule
    printf "METRIC %s_required_any_cache_speedup_ratio=%.6f\n", prefix, per_rule / cached
    printf "METRIC %s_prefix_gate_only_ns_per_op=%.0f\n", prefix, prefix_only
    printf "METRIC %s_speedup_ratio=%.6f\n", prefix, prefix_only / cached
  }
  $1 ~ /^BenchmarkDetectFragmentSemiGeneric/ {
    split($1, parts, "/")
    bench = parts[1]
    mode = parts[2]
    sub(/-[0-9]+$/, "", mode)
    record(bench, mode, $3)
  }
  END {
    no_operator = "BenchmarkDetectFragmentSemiGenericKeywordNoOperator"
    punctuation = "BenchmarkDetectFragmentSemiGenericKeywordPunctuationMiss"
    positive = "BenchmarkDetectFragmentSemiGenericPositive"
    cold = "BenchmarkDetectFragmentSemiGenericColdNoOperator"
    if (n[no_operator, "operator_byte_gate"] == 0 ||
        n[no_operator, "operator_byte_gate_no_atom"] == 0 ||
        n[no_operator, "operator_byte_gate_per_rule_scan"] == 0 ||
        n[no_operator, "prefix_regex_gate_only"] == 0 ||
        n[punctuation, "operator_byte_gate"] == 0 ||
        n[punctuation, "operator_byte_gate_no_atom"] == 0 ||
        n[punctuation, "operator_byte_gate_per_rule_scan"] == 0 ||
        n[punctuation, "prefix_regex_gate_only"] == 0 ||
        n[positive, "operator_byte_gate"] == 0 ||
        n[positive, "operator_byte_gate_no_atom"] == 0 ||
        n[positive, "operator_byte_gate_per_rule_scan"] == 0 ||
        n[positive, "prefix_regex_gate_only"] == 0 ||
        n[cold, "operator_byte_gate"] == 0 ||
        n[cold, "operator_byte_gate_no_atom"] == 0 ||
        n[cold, "operator_byte_gate_per_rule_scan"] == 0 ||
        n[cold, "prefix_regex_gate_only"] == 0) {
      exit 1
    }
    emit(no_operator, "no_operator")
    emit(punctuation, "punctuation_miss")
    emit(positive, "positive")
    emit(cold, "cold_no_operator")

    cached = sum[no_operator, "operator_byte_gate"] / n[no_operator, "operator_byte_gate"]
    prefix_only = sum[no_operator, "prefix_regex_gate_only"] / n[no_operator, "prefix_regex_gate_only"]
    printf "METRIC ns_per_op=%.0f\n", cached
    printf "METRIC prefix_gate_only_ns_per_op=%.0f\n", prefix_only
    printf "METRIC speedup_ratio=%.6f\n", prefix_only / cached
  }
' "$tmp"
