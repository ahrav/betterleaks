#!/usr/bin/env python3
"""Strictly compare two betterleaks JSON reports, ignoring only Fingerprint."""

from __future__ import annotations

from collections import Counter
import json
import sys
from typing import Any


IGNORED_FIELDS = {"Fingerprint"}


def normalize(value: Any) -> Any:
    if isinstance(value, dict):
        return {
            key: normalize(value[key])
            for key in sorted(value)
            if key not in IGNORED_FIELDS
        }
    if isinstance(value, list):
        return [normalize(item) for item in value]
    return value


def canonical_finding(finding: dict[str, Any]) -> str:
    return json.dumps(normalize(finding), sort_keys=True, separators=(",", ":"))


def finding_label(encoded: str) -> str:
    finding = json.loads(encoded)
    return (
        f"[{finding.get('RuleID', '')}] "
        f"{finding.get('File', '')}:{finding.get('StartLine', '?')} "
        f"{finding.get('Match', '')[:80]!r}"
    )


def load_report(path: str) -> list[dict[str, Any]]:
    with open(path, encoding="utf-8") as f:
        report = json.load(f)
    if report is None:
        return []
    if not isinstance(report, list):
        raise TypeError(f"{path}: expected top-level JSON array")
    for i, finding in enumerate(report):
        if not isinstance(finding, dict):
            raise TypeError(f"{path}: finding {i} is not an object")
    return report


def main() -> int:
    if len(sys.argv) != 3:
        print(f"Usage: {sys.argv[0]} <before.json> <after.json>", file=sys.stderr)
        return 2

    before = load_report(sys.argv[1])
    after = load_report(sys.argv[2])
    before_counts = Counter(canonical_finding(f) for f in before)
    after_counts = Counter(canonical_finding(f) for f in after)

    only_before = before_counts - after_counts
    only_after = after_counts - before_counts

    print(f"Before findings: {len(before)}")
    print(f"After findings:  {len(after)}")
    print(f"Only before:     {sum(only_before.values())}")
    print(f"Only after:      {sum(only_after.values())}")

    if only_before:
        print("\nFindings only before:")
        for encoded, count in only_before.most_common(20):
            print(f"  x{count} {finding_label(encoded)}")
    if only_after:
        print("\nFindings only after:")
        for encoded, count in only_after.most_common(20):
            print(f"  x{count} {finding_label(encoded)}")

    if only_before or only_after:
        print("\nRESULT: Reports differ")
        return 1

    print("\nRESULT: Reports are equivalent (excluding Fingerprint)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
