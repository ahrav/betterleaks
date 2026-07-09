#!/usr/bin/env bash
set -euo pipefail

path="${1:-.}"

BETTERLEAKS_RULE_GATE_STATS_PATH="$path" \
  go test ./detect -run '^TestRuleGateStatsForPath$' -count=1 -v
