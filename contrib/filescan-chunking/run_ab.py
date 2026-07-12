#!/usr/bin/env python3
"""Run a randomized, fixed-corpus, correctness-gated scanner binary A/B."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import random
import signal
import subprocess
import time
from pathlib import Path


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def findings_identity(path: Path) -> tuple[int, str]:
    findings = json.loads(path.read_text()) or []
    canonical = json.dumps(
        sorted(findings, key=lambda finding: json.dumps(finding, sort_keys=True)),
        sort_keys=True,
        separators=(",", ":"),
    ).encode()
    return len(findings), hashlib.sha256(canonical).hexdigest()


def run_child(command: list[str], stdout_path: Path, stderr_path: Path, timeout: int):
    with stdout_path.open("wb") as stdout, stderr_path.open("wb") as stderr:
        process = subprocess.Popen(
            command,
            stdout=stdout,
            stderr=stderr,
            start_new_session=True,
        )
        started = time.monotonic_ns()
        deadline = time.monotonic() + timeout
        while True:
            pid, status, usage = os.wait4(process.pid, os.WNOHANG)
            if pid == process.pid:
                process.returncode = os.waitstatus_to_exitcode(status)
                return process.returncode, time.monotonic_ns() - started, usage, False
            if time.monotonic() >= deadline:
                os.killpg(process.pid, signal.SIGKILL)
                _, status, usage = os.wait4(process.pid, 0)
                process.returncode = os.waitstatus_to_exitcode(status)
                return process.returncode, time.monotonic_ns() - started, usage, True
            time.sleep(0.01)


def scan_command(binary: Path, report: Path, scanner_args: list[str], root: Path) -> list[str]:
    return [
        str(binary),
        "dir",
        "--no-banner",
        "--log-level",
        "error",
        "--exit-code",
        "0",
        "--report-format",
        "json",
        "--report-path",
        str(report),
        *scanner_args,
        str(root),
    ]


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--baseline-binary", type=Path, required=True)
    parser.add_argument("--candidate-binary", type=Path, required=True)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--blocks", type=int, required=True)
    parser.add_argument("--seed", type=int, default=7122026)
    parser.add_argument("--timeout", type=int, default=120)
    parser.add_argument("--scanner-arg", action="append", default=[])
    args = parser.parse_args()

    binaries = {
        "baseline": args.baseline_binary.resolve(),
        "candidate": args.candidate_binary.resolve(),
    }
    binary_hashes = {name: sha256_file(path) for name, path in binaries.items()}
    args.output.mkdir(parents=True, exist_ok=True)
    runs_dir = args.output / "runs"
    runs_dir.mkdir(exist_ok=True)

    randomizer = random.Random(args.seed)
    orders = [
        ["baseline", "candidate"] if block % 2 == 0 else ["candidate", "baseline"]
        for block in range(args.blocks)
    ]
    randomizer.shuffle(orders)
    schedule = []
    for block, order in enumerate(orders):
        schedule.append({"block": block, "order": order})
    (args.output / "plan.json").write_text(
        json.dumps(
            {
                "schema": 1,
                "blocks": args.blocks,
                "seed": args.seed,
                "binaries": {name: str(path) for name, path in binaries.items()},
                "binary_sha256": binary_hashes,
                "root": str(args.root.resolve()),
                "scanner_args": args.scanner_arg,
                "schedule": schedule,
                "timeout_seconds": args.timeout,
            },
            indent=2,
            sort_keys=True,
        )
        + "\n"
    )

    oracle_report = runs_dir / "oracle-baseline.findings.json"
    oracle_returncode, _, _, oracle_timed_out = run_child(
        scan_command(binaries["baseline"], oracle_report, args.scanner_arg, args.root),
        runs_dir / "oracle-baseline.stdout",
        runs_dir / "oracle-baseline.stderr",
        args.timeout,
    )
    if oracle_timed_out or oracle_returncode != 0 or not oracle_report.exists():
        return 1
    oracle = findings_identity(oracle_report)
    (args.output / "oracle.json").write_text(
        json.dumps({"finding_count": oracle[0], "findings_digest": oracle[1]}, indent=2, sort_keys=True)
        + "\n"
    )

    with (args.output / "results.jsonl").open("a", buffering=1) as results:
        for scheduled in schedule:
            block = scheduled["block"]
            for position, arm in enumerate(scheduled["order"]):
                stem = f"b{block:02d}-p{position:02d}-{arm}"
                report = runs_dir / f"{stem}.findings.json"
                command = scan_command(binaries[arm], report, args.scanner_arg, args.root)
                returncode, wall_ns, usage, timed_out = run_child(
                    command,
                    runs_dir / f"{stem}.stdout",
                    runs_dir / f"{stem}.stderr",
                    args.timeout,
                )
                identity = findings_identity(report) if returncode == 0 and report.exists() else None
                valid = not timed_out and returncode == 0 and identity == oracle
                row = {
                    "block": block,
                    "position": position,
                    "candidate": arm,
                    "binary_sha256": binary_hashes[arm],
                    "command": command,
                    "returncode": returncode,
                    "timed_out": timed_out,
                    "valid": valid,
                    "wall_ns": wall_ns,
                    "user_seconds": usage.ru_utime,
                    "system_seconds": usage.ru_stime,
                    "max_rss_kib": usage.ru_maxrss,
                    "minor_faults": usage.ru_minflt,
                    "major_faults": usage.ru_majflt,
                    "input_blocks": usage.ru_inblock,
                    "output_blocks": usage.ru_oublock,
                    "voluntary_context_switches": usage.ru_nvcsw,
                    "involuntary_context_switches": usage.ru_nivcsw,
                    "finding_count": identity[0] if identity else None,
                    "findings_digest": identity[1] if identity else None,
                }
                results.write(json.dumps(row, sort_keys=True) + "\n")
                os.fsync(results.fileno())
                if not valid:
                    return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
