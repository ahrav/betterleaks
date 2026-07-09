#!/usr/bin/env bash
set -euo pipefail

path="${1:-.}"

BETTERLEAKS_MANDATORY_ATOM_PATH="$path" \
  go test ./detect -run '^TestMandatoryAtomOpportunityForPath$' -count=1 -v
