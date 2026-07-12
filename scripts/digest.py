#!/usr/bin/env python3
"""Compute a stable, order-independent digest of a betterleaks JSON report.

Emits "<count>\t<sha256>" so keep/discard decisions can compare findings
byte-identically across experiments. Fingerprint is excluded (it is derived and
path-dependent); every other identity field is included, matching the
COMPARE_FIELDS set used by compare_reports.py.
"""

import hashlib
import json
import sys

COMPARE_FIELDS = [
    "RuleID", "Description", "File",
    "StartLine", "EndLine", "StartColumn", "EndColumn",
    "Match", "Secret", "Entropy",
    "Commit", "Author", "Email", "Date", "Message",
    "SymlinkFile",
]


def normalize(f):
    d = {k: f.get(k, "") for k in COMPARE_FIELDS}
    d["Entropy"] = f.get("Entropy", 0)
    d["Tags"] = sorted(f.get("Tags") or [])
    d["Attributes"] = sorted((f.get("Attributes") or {}).items())
    return json.dumps(d, sort_keys=True, ensure_ascii=False)


def main():
    with open(sys.argv[1]) as fh:
        findings = json.load(fh)
    lines = sorted(normalize(f) for f in findings)
    h = hashlib.sha256()
    for ln in lines:
        h.update(ln.encode("utf-8"))
        h.update(b"\n")
    print(f"{len(findings)}\t{h.hexdigest()}")


if __name__ == "__main__":
    main()
