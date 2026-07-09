#!/usr/bin/env bash
set -euo pipefail

/local/home/ahrav/.local/share/mise/installs/node/24.14.0/bin/node /local/home/ahrav/.codex/plugins/cache/TheGreenCedar/codex-autoresearch/2.5.1/scripts/autoresearch.mjs quality-gap --cwd . --research-slug regex-scan-time
