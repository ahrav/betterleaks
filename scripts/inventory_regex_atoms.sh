#!/usr/bin/env bash
set -euo pipefail

BETTERLEAKS_MANDATORY_ATOM_INVENTORY=1 \
  go test ./detect -run '^TestMandatoryAtomInventory$' -count=1 -v
