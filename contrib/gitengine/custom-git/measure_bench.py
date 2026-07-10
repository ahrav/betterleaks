#!/usr/bin/env python3
"""Run one benchmark command while sampling descendant helper RSS."""

import argparse
import json
import subprocess
import time


def process_table():
    output = subprocess.check_output(
        ["ps", "-axo", "pid=,ppid=,rss=,vsz=,pagein=,command="], text=True
    )
    rows = []
    for line in output.splitlines():
        fields = line.strip().split(None, 5)
        if len(fields) == 6:
            try:
                rows.append((
                    int(fields[0]), int(fields[1]), int(fields[2]),
                    int(fields[3]), int(fields[4]), fields[5],
                ))
            except ValueError:
                continue
    return rows


def is_engine_helper(command):
    """Select only the engine processes below the benchmark wrapper."""
    if any(name in command for name in (
        "betterleaks--diff-engine",
        "betterleaks-libgit2-engine",
        "betterleaks-gix-helper",
    )):
        return True
    # The stock reference opens one `git log -p` and one raw `diff-tree`
    # process per worker. Restrict matching to this wrapper's descendants so
    # unrelated Git processes on the host cannot enter the measurement.
    return (
        (command.startswith("git -C ") or " git -C " in command)
        and (
            (" log -p " in command and " --no-walk=unsorted " in command)
            or (" diff-tree " in command and " --raw " in command)
        )
    )


def helper_stats(root_pid):
    rows = process_table()
    descendants = {root_pid}
    changed = True
    while changed:
        changed = False
        for pid, parent, _, _, _, _ in rows:
            if parent in descendants and pid not in descendants:
                descendants.add(pid)
                changed = True
    helpers = [(pid, rss, vsz, pageins) for pid, _, rss, vsz, pageins, command in rows
               if pid in descendants and is_engine_helper(command)]
    return helpers


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--interval", type=float, default=0.25)
    parser.add_argument("--append-jsonl")
    parser.add_argument("--label", default="")
    parser.add_argument("--validity", choices=("valid", "quarantined"), default="valid")
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if args.command and args.command[0] == "--":
        args.command = args.command[1:]
    if not args.command:
        parser.error("benchmark command is required after --")

    proc = subprocess.Popen(
        args.command, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True
    )
    peak_aggregate = 0
    peak_individual = 0
    peak_aggregate_vsz = 0
    peak_individual_vsz = 0
    peak_helpers = 0
    pageins_by_pid = {}
    samples = 0
    started = time.monotonic()
    while proc.poll() is None:
        helpers = helper_stats(proc.pid)
        rss = [item[1] for item in helpers]
        vsz = [item[2] for item in helpers]
        for pid, _, _, pageins in helpers:
            pageins_by_pid[pid] = max(pageins_by_pid.get(pid, 0), pageins)
        peak_aggregate = max(peak_aggregate, sum(rss))
        peak_individual = max(peak_individual, max(rss, default=0))
        peak_aggregate_vsz = max(peak_aggregate_vsz, sum(vsz))
        peak_individual_vsz = max(peak_individual_vsz, max(vsz, default=0))
        peak_helpers = max(peak_helpers, len(rss))
        samples += 1
        time.sleep(args.interval)
    stdout, stderr = proc.communicate()
    result = {
        "status": proc.returncode,
        "wall_seconds": time.monotonic() - started,
        "samples": samples,
        "peak_helpers": peak_helpers,
        "peak_aggregate_rss_kib": peak_aggregate,
        "peak_individual_rss_kib": peak_individual,
        "peak_aggregate_vsz_kib": peak_aggregate_vsz,
        "peak_individual_vsz_kib": peak_individual_vsz,
        "helper_pageins": sum(pageins_by_pid.values()),
        "rss_scope": "engine_helpers_only",
        "benchmark": json.loads(stdout) if stdout.strip() else None,
        "stderr": stderr,
        "command": args.command,
        "label": args.label,
        "validity": args.validity,
    }
    if args.append_jsonl:
        with open(args.append_jsonl, "a", encoding="utf-8") as stream:
            stream.write(json.dumps(result, sort_keys=True) + "\n")
    print(json.dumps(result, indent=2, sort_keys=True))
    if proc.returncode:
        raise SystemExit(proc.returncode)


if __name__ == "__main__":
    main()
