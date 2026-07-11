#!/usr/bin/env python3
"""Sample one benchmark command's helper resources through Linux procfs."""

import argparse
import json
import os
import subprocess
import time


def process_table():
    rows = []
    page_kib = os.sysconf("SC_PAGE_SIZE") // 1024
    for entry in os.listdir("/proc"):
        if not entry.isdigit():
            continue
        pid = int(entry)
        try:
            with open(f"/proc/{pid}/stat", encoding="ascii") as stream:
                stat = stream.read()
            close = stat.rfind(")")
            if close < 0:
                continue
            fields = stat[close + 2:].split()
            with open(f"/proc/{pid}/cmdline", "rb") as stream:
                argv = [
                    item.decode(errors="replace")
                    for item in stream.read().split(b"\0")
                    if item
                ]
            io_stats = {}
            try:
                with open(f"/proc/{pid}/io", encoding="ascii") as stream:
                    for line in stream:
                        key, value = line.split(":", 1)
                        io_stats[key] = int(value)
            except (FileNotFoundError, PermissionError, ProcessLookupError):
                pass
            rows.append({
                "pid": pid,
                "ppid": int(fields[1]),
                "starttime": int(fields[19]),
                "rss_kib": int(fields[21]) * page_kib,
                "vsz_kib": int(fields[20]) // 1024,
                "minor_faults": int(fields[7]),
                "major_faults": int(fields[9]),
                "user_ticks": int(fields[11]),
                "system_ticks": int(fields[12]),
                "read_syscalls": io_stats.get("syscr", 0),
                "write_syscalls": io_stats.get("syscw", 0),
                "storage_read_bytes": io_stats.get("read_bytes", 0),
                "storage_write_bytes": io_stats.get("write_bytes", 0),
                "argv": argv,
                "command": " ".join(argv),
            })
        except (FileNotFoundError, PermissionError, ProcessLookupError, ValueError):
            continue
    return rows


def is_engine_helper(argv):
    """Select only the engine processes below the benchmark wrapper."""
    if not argv:
        return False
    executable = os.path.basename(argv[0])
    if executable in (
        "betterleaks--diff-engine",
        "betterleaks-libgit2-engine",
        "betterleaks-gix-helper",
    ):
        return True
    if "betterleaks--diff-engine" in argv:
        return True
    # The stock reference currently opens three `git log` processes per batch:
    # patch, raw metadata, and commit metadata. Keep the older diff-tree match
    # for archived harnesses. Restrict matching to this wrapper's descendants.
    return executable == "git" and (
        ("log" in argv and "--no-walk=unsorted" in argv)
        or ("diff-tree" in argv and "--raw" in argv)
    )


def descendant_rows(rows, root_pid):
    descendants = {root_pid}
    changed = True
    while changed:
        changed = False
        for row in rows:
            if row["ppid"] in descendants and row["pid"] not in descendants:
                descendants.add(row["pid"])
                changed = True
    return [row for row in rows if row["pid"] in descendants]


COUNTER_FIELDS = (
    "minor_faults",
    "major_faults",
    "user_ticks",
    "system_ticks",
    "read_syscalls",
    "write_syscalls",
    "storage_read_bytes",
    "storage_write_bytes",
)


def retain_process_counters(observed, rows):
    for row in rows:
        key = (row["pid"], row["starttime"])
        current = observed.setdefault(key, {field: 0 for field in COUNTER_FIELDS})
        current["command"] = row["command"]
        current["helper"] = current.get("helper", False) or is_engine_helper(
            row["argv"]
        )
        for field in COUNTER_FIELDS:
            current[field] = max(current[field], row[field])


def sum_counters(observed, helper=None):
    selected = observed.values()
    if helper is not None:
        selected = [item for item in selected if item.get("helper", False) == helper]
    return {field: sum(item[field] for item in selected) for field in COUNTER_FIELDS}


def counters_for_pid(observed, pid):
    """Return the latest sampled counters for one process identity."""
    matches = [
        (starttime, item)
        for (observed_pid, starttime), item in observed.items()
        if observed_pid == pid
    ]
    if not matches:
        return {field: 0 for field in COUNTER_FIELDS}
    return max(matches, key=lambda match: match[0])[1]


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
    if not os.path.isdir("/proc/self"):
        parser.error("resource sampling requires Linux /proc")

    started = time.monotonic()
    proc = subprocess.Popen(
        args.command, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True
    )
    peak_aggregate = 0
    peak_individual = 0
    peak_aggregate_vsz = 0
    peak_individual_vsz = 0
    peak_helpers = 0
    observed = {}
    samples = 0
    while True:
        rows = descendant_rows(process_table(), proc.pid)
        retain_process_counters(observed, rows)
        helpers = [row for row in rows if is_engine_helper(row["argv"])]
        rss = [item["rss_kib"] for item in helpers]
        vsz = [item["vsz_kib"] for item in helpers]
        peak_aggregate = max(peak_aggregate, sum(rss))
        peak_individual = max(peak_individual, max(rss, default=0))
        peak_aggregate_vsz = max(peak_aggregate_vsz, sum(vsz))
        peak_individual_vsz = max(peak_individual_vsz, max(vsz, default=0))
        peak_helpers = max(peak_helpers, len(rss))
        samples += 1
        try:
            proc.wait(timeout=args.interval)
            break
        except subprocess.TimeoutExpired:
            pass
    stdout, stderr = proc.communicate()
    helper_counters = sum_counters(observed, helper=True)
    tree_counters = sum_counters(observed)
    root_counters = counters_for_pid(observed, proc.pid)
    ticks = os.sysconf("SC_CLK_TCK")
    result = {
        "status": proc.returncode,
        "wall_seconds": time.monotonic() - started,
        "samples": samples,
        "peak_helpers": peak_helpers,
        "peak_aggregate_rss_kib": peak_aggregate,
        "peak_individual_rss_kib": peak_individual,
        "peak_aggregate_vsz_kib": peak_aggregate_vsz,
        "peak_individual_vsz_kib": peak_individual_vsz,
        "helper_pageins": helper_counters["major_faults"],
        "helper_minor_faults": helper_counters["minor_faults"],
        "helper_major_faults": helper_counters["major_faults"],
        "helper_user_cpu_seconds": helper_counters["user_ticks"] / ticks,
        "helper_system_cpu_seconds": helper_counters["system_ticks"] / ticks,
        "helper_read_syscalls": helper_counters["read_syscalls"],
        "helper_write_syscalls": helper_counters["write_syscalls"],
        "helper_storage_read_bytes": helper_counters["storage_read_bytes"],
        "helper_storage_write_bytes": helper_counters["storage_write_bytes"],
        "process_tree_user_cpu_seconds": tree_counters["user_ticks"] / ticks,
        "process_tree_system_cpu_seconds": tree_counters["system_ticks"] / ticks,
        "benchmark_root_read_syscalls": root_counters["read_syscalls"],
        "benchmark_root_write_syscalls": root_counters["write_syscalls"],
        "rss_scope": "engine_helpers_only",
        "cpu_scope": "sampled_proc_descendants_including_exited_pids_seen_before_exit",
        "io_scope": "sampled_benchmark_root_procfs_io_may_include_reaped_children_and_omit_activity_after_final_sample",
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
