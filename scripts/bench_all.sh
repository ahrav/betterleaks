#!/usr/bin/env bash
# bench_all.sh - run benchcpu2.sh for one binary across the three lab corpora
# and append rows to the shared results TSV.
#
# USAGE
#   BIN=./betterleaks-wazero scripts/bench_all.sh <label-prefix> [runs]
set -euo pipefail

BIN="${BIN:-./betterleaks-wazero}"
label="$1"
runs="${2:-3}"
out="${OUT:-docs/ahoc-lab-results.tsv}"

declare -A corpora=(
    [git]=/local/home/ahrav/scratch/corpora/git
    [cpython]=/local/home/ahrav/scratch/corpora/cpython
    [gitlab-foss]=/local/home/ahrav/scratch/gitlab-foss
)

for name in git cpython gitlab-foss; do
    row="$(BIN="$BIN" EXTRA_ARGS="${EXTRA_ARGS:-}" bash scripts/benchcpu2.sh "${label}/${name}" "${corpora[$name]}" "$runs")"
    echo "$row" | tee -a "$out"
done
