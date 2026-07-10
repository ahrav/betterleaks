#!/usr/bin/env python3
"""Compare betterleaks' short-lived Git batches with persistent diff-tree.

The benchmark deliberately measures Git production and pipe draining only, so
detector and parser costs do not obscure process/cache lifetime. The original
``ephemeral`` and ``persistent-static`` diagnostics drain to ``/dev/null``;
``ephemeral-drain`` and ``persistent-dynamic`` both drain stdout through Python
in large chunks for a fair pipe/protocol comparison. Use ``--verify`` to check
the semantic streams before using timings from a new repository or Git version.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import resource
import secrets
import subprocess
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from queue import Empty, Queue


def git_env() -> dict[str, str]:
    """Match betterleaks' isolated Git configuration and delta cache."""
    env = os.environ.copy()
    env.update(
        {
            "GIT_CONFIG_GLOBAL": os.devnull,
            "GIT_CONFIG_NOSYSTEM": "1",
            "GIT_CONFIG_SYSTEM": os.devnull,
            "GIT_NO_REPLACE_OBJECTS": "1",
            "GIT_TERMINAL_PROMPT": "0",
            "GIT_CONFIG_COUNT": "1",
            "GIT_CONFIG_KEY_0": "core.deltaBaseCacheLimit",
            "GIT_CONFIG_VALUE_0": "128m",
        }
    )
    env.setdefault("MALLOC_ARENA_MAX", "2")
    return env


def commits(repo: Path, limit: int) -> list[str]:
    command = ["git", "-C", str(repo), "rev-list", "--all"]
    if limit:
        command.append(f"--max-count={limit}")
    output = subprocess.check_output(command, env=git_env(), text=True)
    return output.splitlines()


def batches(values: list[str], size: int) -> list[list[str]]:
    return [values[offset : offset + size] for offset in range(0, len(values), size)]


def production_batches(values: list[str], workers: int) -> list[list[str]]:
    """Reproduce sources.buildBatches, including its 16-commit locality runs."""
    batch_size = max(len(values) // (workers * 8), 64)
    batch_count = math.ceil(len(values) / batch_size)
    result: list[list[str]] = [[] for _ in range(batch_count)]
    for run_index, offset in enumerate(range(0, len(values), 16)):
        result[run_index % batch_count].extend(values[offset : offset + 16])
    return [batch for batch in result if batch]


def git_log_command(repo: Path) -> list[str]:
    return [
        "git",
        "-C",
        str(repo),
        "log",
        "-p",
        "-U0",
        "--no-walk",
        "--stdin",
        "--diff-filter=tuxdb",
    ]


def diff_tree_command(repo: Path) -> list[str]:
    return [
        "git",
        "-C",
        str(repo),
        "diff-tree",
        "--stdin",
        "--root",
        "-p",
        "-U0",
        "-M",
        "--diff-filter=tuxdb",
        "--pretty=medium",
    ]


def encoded(values: list[str]) -> bytes:
    return ("\n".join(values) + "\n").encode()


def run_command(command: list[str], values: list[str], *, capture: bool = False) -> bytes:
    result = subprocess.run(
        command,
        input=encoded(values),
        stdout=subprocess.PIPE if capture else subprocess.DEVNULL,
        stderr=subprocess.PIPE,
        env=git_env(),
        check=False,
    )
    if result.returncode:
        raise RuntimeError(
            f"command failed ({result.returncode}): {' '.join(command)}\n"
            + result.stderr.decode(errors="replace")
        )
    return result.stdout if capture else b""


def hash_command(command: list[str], values: list[str], digest: object) -> int:
    """Stream stdout into an existing hashlib digest without retaining it."""
    process = subprocess.Popen(
        command,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=git_env(),
    )
    assert process.stdin is not None
    assert process.stdout is not None
    assert process.stderr is not None

    def write_input() -> None:
        process.stdin.write(encoded(values))
        process.stdin.close()

    byte_count = 0
    with ThreadPoolExecutor(max_workers=1) as pool:
        writer = pool.submit(write_input)
        while chunk := process.stdout.read(1024 * 1024):
            digest.update(chunk)  # type: ignore[attr-defined]
            byte_count += len(chunk)
        writer.result()
    stderr = process.stderr.read()
    returncode = process.wait()
    if returncode:
        raise RuntimeError(
            f"command failed ({returncode}): {' '.join(command)}\n"
            + stderr.decode(errors="replace")
        )
    return byte_count


def run_ephemeral(repo: Path, work: list[list[str]], workers: int) -> None:
    command = git_log_command(repo)
    with ThreadPoolExecutor(max_workers=workers) as pool:
        list(pool.map(lambda batch: run_command(command, batch), work))


def drain_command(command: list[str], values: list[str]) -> None:
    """Write stdin and drain stdout concurrently without parsing lines."""
    process = subprocess.Popen(
        command,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=git_env(),
    )
    assert process.stdin is not None
    assert process.stdout is not None
    assert process.stderr is not None

    def write_input() -> None:
        process.stdin.write(encoded(values))
        process.stdin.close()

    with ThreadPoolExecutor(max_workers=1) as pool:
        writer = pool.submit(write_input)
        while os.read(process.stdout.fileno(), 1 << 20):
            pass
        writer.result()
    stderr = process.stderr.read()
    returncode = process.wait()
    if returncode:
        raise RuntimeError(
            f"command failed ({returncode}): {' '.join(command)}\n"
            + stderr.decode(errors="replace")
        )


def run_ephemeral_drain(repo: Path, work: list[list[str]], workers: int) -> None:
    command = git_log_command(repo)
    with ThreadPoolExecutor(max_workers=workers) as pool:
        list(pool.map(lambda batch: drain_command(command, batch), work))


def run_persistent_static(repo: Path, work: list[list[str]], workers: int) -> None:
    # Static round-robin assignment isolates process/cache lifetime. A scanner
    # implementation should retain dynamic scheduling with explicit sentinels.
    assignments: list[list[str]] = [[] for _ in range(min(workers, len(work)))]
    for index, batch in enumerate(work):
        assignments[index % len(assignments)].extend(batch)
    command = diff_tree_command(repo)
    with ThreadPoolExecutor(max_workers=len(assignments)) as pool:
        list(pool.map(lambda assignment: run_command(command, assignment), assignments))


def run_persistent_dynamic(repo: Path, work: list[list[str]], workers: int) -> None:
    """Keep one diff-tree per worker while retaining the production queue."""
    pending: Queue[list[str]] = Queue()
    for batch in work:
        pending.put(batch)

    command = diff_tree_command(repo)

    def worker() -> None:
        process = subprocess.Popen(
            command,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=git_env(),
        )
        assert process.stdin is not None
        assert process.stdout is not None
        assert process.stderr is not None

        buffered = bytearray()

        def read_until(marker: bytes) -> None:
            while True:
                search_from = 0
                while True:
                    index = buffered.find(marker, search_from)
                    if index < 0:
                        break
                    # The invalid request is echoed as a complete top-level
                    # line. A marker-like added line has a leading '+', and a
                    # commit-message line is indented, so neither can close a
                    # batch even before considering the random nonce.
                    if index == 0 or buffered[index - 1] == ord("\n"):
                        del buffered[: index + len(marker)]
                        return
                    search_from = index + 1
                # Keep only the suffix that could contain a split marker. The
                # patch body is intentionally discarded without per-line work.
                if len(buffered) > len(marker):
                    del buffered[: len(buffered) - len(marker)]
                chunk = os.read(process.stdout.fileno(), 1 << 20)
                if not chunk:
                    raise RuntimeError("persistent diff-tree closed before its batch sentinel")
                buffered.extend(chunk)

        def write_request(batch: list[str], marker: bytes) -> None:
            process.stdin.write(encoded(batch))
            process.stdin.write(marker)
            process.stdin.flush()

        with ThreadPoolExecutor(max_workers=2) as io_pool:
            stderr_future = io_pool.submit(process.stderr.read)
            batch_serial = 0
            worker_nonce = secrets.token_hex(16)
            while True:
                try:
                    batch = pending.get_nowait()
                except Empty:
                    break
                marker = f"betterleaks-batch-v1 {worker_nonce} {batch_serial}\n".encode()
                batch_serial += 1
                # A full Git stdout pipe can otherwise deadlock a large batch:
                # Git waits for us to drain while we wait for stdin.write.
                writer = io_pool.submit(write_request, batch, marker)
                read_until(marker)
                writer.result()
                pending.task_done()
            process.stdin.close()
            trailing_other_bytes = len(buffered)
            while chunk := os.read(process.stdout.fileno(), 1 << 20):
                trailing_other_bytes += len(chunk)
            returncode = process.wait()
            stderr = stderr_future.result()
        if returncode or trailing_other_bytes:
            raise RuntimeError(
                f"persistent diff-tree failed ({returncode}); trailing bytes={trailing_other_bytes}\n"
                + stderr.decode(errors="replace")
            )

    with ThreadPoolExecutor(max_workers=min(workers, len(work))) as pool:
        list(pool.map(lambda _: worker(), range(min(workers, len(work)))))


def child_usage() -> tuple[float, float, int, int, int]:
    usage = resource.getrusage(resource.RUSAGE_CHILDREN)
    return (
        usage.ru_utime,
        usage.ru_stime,
        usage.ru_inblock,
        usage.ru_majflt,
        usage.ru_minflt,
    )


def subtract(after: tuple[float, float, int, int, int], before: tuple[float, float, int, int, int]) -> tuple[float, float, int, int, int]:
    return tuple(right - left for right, left in zip(after, before, strict=True))  # type: ignore[return-value]


def verify(repo: Path, values: list[str], work: list[list[str]]) -> None:
    baseline_digest = hashlib.sha256()
    baseline_bytes = 0
    command = git_log_command(repo)
    per_batch_identical = True
    for batch in work:
        baseline_bytes += hash_command(command, batch, baseline_digest)
        log_digest = hashlib.sha256()
        tree_digest = hashlib.sha256()
        log_bytes = hash_command(command, batch, log_digest)
        tree_bytes = hash_command(diff_tree_command(repo), batch, tree_digest)
        per_batch_identical = per_batch_identical and (
            log_bytes == tree_bytes and log_digest.digest() == tree_digest.digest()
        )
    ordered_values = [value for batch in work for value in batch]
    candidate_digest = hashlib.sha256()
    candidate_bytes = hash_command(diff_tree_command(repo), ordered_values, candidate_digest)
    baseline_hash = baseline_digest.hexdigest()
    candidate_hash = candidate_digest.hexdigest()
    byte_identical = baseline_bytes == candidate_bytes and baseline_hash == candidate_hash
    separator_delta = candidate_bytes - baseline_bytes
    expected_separator_delta = max(len(work) - 1, 0)
    result = {
        "batches": len(work),
        "commits": len(values),
        "baseline_bytes": baseline_bytes,
        "candidate_bytes": candidate_bytes,
        "baseline_sha256": baseline_hash,
        "candidate_sha256": candidate_hash,
        "byte_identical": byte_identical,
        "per_batch_identical": per_batch_identical,
        "separator_delta_bytes": separator_delta,
        "expected_separator_delta_bytes": expected_separator_delta,
    }
    print(json.dumps(result, sort_keys=True))
    if not per_batch_identical or separator_delta != expected_separator_delta:
        raise RuntimeError("git-log and diff-tree differ beyond expected batch-boundary separators")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("repo", type=Path)
    parser.add_argument(
        "--arm",
        choices=["ephemeral", "ephemeral-drain", "persistent-dynamic", "persistent-static"],
        default="ephemeral",
    )
    parser.add_argument("--commits", type=int, default=4096, help="0 means every commit reachable from --all")
    parser.add_argument(
        "--batch-size",
        type=int,
        default=0,
        help="contiguous diagnostic batch size; 0 reproduces production buildBatches",
    )
    parser.add_argument("--workers", type=int, default=1)
    parser.add_argument("--repetitions", type=int, default=1)
    parser.add_argument("--verify", action="store_true")
    args = parser.parse_args()

    if args.batch_size < 0 or args.workers < 1 or args.repetitions < 1:
        parser.error("batch size cannot be negative; workers and repetitions must be positive")
    if not (args.repo / ".git").is_dir():
        parser.error(f"{args.repo} is not a non-bare Git worktree")

    values = commits(args.repo, args.commits)
    if not values:
        parser.error("revision walk produced no commits")
    work = (
        batches(values, args.batch_size)
        if args.batch_size
        else production_batches(values, args.workers)
    )
    if args.verify:
        verify(args.repo, values, work)
        return

    runners = {
        "ephemeral": run_ephemeral,
        "ephemeral-drain": run_ephemeral_drain,
        "persistent-dynamic": run_persistent_dynamic,
        "persistent-static": run_persistent_static,
    }
    runner = runners[args.arm]
    for repetition in range(args.repetitions):
        before = child_usage()
        started = time.perf_counter()
        runner(args.repo, work, args.workers)
        wall = time.perf_counter() - started
        user, system, inblock, major_faults, minor_faults = subtract(child_usage(), before)
        print(
            json.dumps(
                {
                    "arm": args.arm,
                    "batch_size": args.batch_size or "production",
                    "batches": len(work),
                    "commits": len(values),
                    "git_processes": len(work) if args.arm.startswith("ephemeral") else min(args.workers, len(work)),
                    "inblock": inblock,
                    "major_faults": major_faults,
                    "minor_faults": minor_faults,
                    "repetition": repetition + 1,
                    "system_s": round(system, 6),
                    "user_s": round(user, 6),
                    "wall_s": round(wall, 6),
                    "workers": args.workers,
                },
                sort_keys=True,
            )
        )


if __name__ == "__main__":
    try:
        main()
    except (OSError, RuntimeError, subprocess.SubprocessError) as error:
        print(f"error: {error}", file=sys.stderr)
        raise SystemExit(1) from error
