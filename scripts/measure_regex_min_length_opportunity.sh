#!/usr/bin/env bash
set -euo pipefail

path="${1:-.}"

BETTERLEAKS_MIN_LENGTH_PATH="$path" \
  go test ./detect -run '^TestMinimumLengthOpportunityForPath$' -count=1 -v
