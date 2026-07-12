#!/usr/bin/env python3
"""Summarize randomized chunking runs with paired bootstrap intervals."""

from __future__ import annotations

import argparse
import json
import math
import random
import statistics
from collections import defaultdict
from pathlib import Path


def percentile(sorted_values: list[float], probability: float) -> float:
    index = probability * (len(sorted_values) - 1)
    lower = math.floor(index)
    upper = math.ceil(index)
    if lower == upper:
        return sorted_values[lower]
    weight = index - lower
    return sorted_values[lower] * (1 - weight) + sorted_values[upper] * weight


def paired_interval(
    baseline: dict[int, dict],
    candidate: dict[int, dict],
    field: str,
    resamples: int,
    seed: int,
) -> tuple[float, float, float]:
    blocks = sorted(set(baseline) & set(candidate))
    log_ratios = [math.log(candidate[block][field] / baseline[block][field]) for block in blocks]
    estimate = math.exp(statistics.mean(log_ratios))
    randomizer = random.Random(seed)
    bootstraps = []
    for _ in range(resamples):
        sample = [log_ratios[randomizer.randrange(len(log_ratios))] for _ in log_ratios]
        bootstraps.append(math.exp(statistics.mean(sample)))
    bootstraps.sort()
    return estimate, percentile(bootstraps, 0.025), percentile(bootstraps, 0.975)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("result", type=Path)
    parser.add_argument("--resamples", type=int, default=100_000)
    parser.add_argument("--seed", type=int, default=7122201)
    args = parser.parse_args()

    rows = [json.loads(line) for line in args.result.read_text().splitlines() if line.strip()]
    if not rows or any(not row["valid"] for row in rows):
        raise SystemExit("result is empty or contains an invalid row")
    by_candidate: dict[str, list[dict]] = defaultdict(list)
    by_candidate_block: dict[str, dict[int, dict]] = defaultdict(dict)
    for row in rows:
        by_candidate[row["candidate"]].append(row)
        by_candidate_block[row["candidate"]][row["block"]] = row
    baseline = by_candidate_block["baseline"]

    summary = {}
    for index, candidate in enumerate(sorted(by_candidate)):
        candidate_rows = by_candidate[candidate]
        walls = [row["wall_ns"] / 1e9 for row in candidate_rows]
        cpus = [row["user_seconds"] + row["system_seconds"] for row in candidate_rows]
        entry = {
            "runs": len(candidate_rows),
            "wall_median_seconds": statistics.median(walls),
            "wall_min_seconds": min(walls),
            "wall_max_seconds": max(walls),
            "wall_cv": statistics.stdev(walls) / statistics.mean(walls) if len(walls) > 1 else 0,
            "cpu_median_seconds": statistics.median(cpus),
            "max_rss_median_kib": statistics.median(row["max_rss_kib"] for row in candidate_rows),
            "finding_count": candidate_rows[0]["finding_count"],
            "findings_digest": candidate_rows[0]["findings_digest"],
        }
        if candidate != "baseline":
            wall_ratio = paired_interval(
                baseline,
                by_candidate_block[candidate],
                "wall_ns",
                args.resamples,
                args.seed + index,
            )
            cpu_baseline = {
                block: {"cpu": row["user_seconds"] + row["system_seconds"]}
                for block, row in baseline.items()
            }
            cpu_candidate = {
                block: {"cpu": row["user_seconds"] + row["system_seconds"]}
                for block, row in by_candidate_block[candidate].items()
            }
            cpu_ratio = paired_interval(
                cpu_baseline,
                cpu_candidate,
                "cpu",
                args.resamples,
                args.seed + 10_000 + index,
            )
            entry["paired_wall_ratio"] = {
                "estimate": wall_ratio[0],
                "lower_95": wall_ratio[1],
                "upper_95": wall_ratio[2],
            }
            entry["paired_cpu_ratio"] = {
                "estimate": cpu_ratio[0],
                "lower_95": cpu_ratio[1],
                "upper_95": cpu_ratio[2],
            }
        summary[candidate] = entry

    print(json.dumps(summary, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
