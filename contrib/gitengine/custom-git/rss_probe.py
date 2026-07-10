#!/usr/bin/env python3
"""Stream custom-engine batches and report helper RSS at batch boundaries."""

import argparse
import os
import struct
import subprocess
import time


def frame(tag, batch_id, payload=b""):
    body = tag + struct.pack(">Q", batch_id) + payload
    return struct.pack(">I", len(body)) + body


def read_frame(stream):
    prefix = stream.read(4)
    if len(prefix) != 4:
        raise RuntimeError("unexpected helper EOF")
    length = struct.unpack(">I", prefix)[0]
    body = stream.read(length)
    if len(body) != length:
        raise RuntimeError("truncated helper frame")
    return body[:4], struct.unpack(">Q", body[4:12])[0], body[12:]


def rss_kib(pid):
    output = subprocess.check_output(["ps", "-o", "rss=", "-p", str(pid)], text=True)
    return int(output.strip())


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--git", required=True)
    parser.add_argument("--repo", required=True)
    parser.add_argument("--commits", type=int, default=20_000)
    parser.add_argument("--batch-size", type=int, default=2_434)
    args = parser.parse_args()

    commit_output = subprocess.check_output(
        ["git", "-C", args.repo, "rev-list", "--all"],
        env={**os.environ, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null"},
    )
    commits = commit_output.splitlines()[: args.commits]
    proc = subprocess.Popen(
        [args.git, "-C", args.repo, "betterleaks--diff-engine", "--protocol=1"],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    tag, batch_id, payload = read_frame(proc.stdout)
    if tag != b"HELO" or batch_id != 0:
        raise RuntimeError("bad handshake")
    oid_len = struct.unpack(">H", payload[2:4])[0]
    started = time.monotonic()
    for batch_id, offset in enumerate(range(0, len(commits), args.batch_size), 1):
        batch = commits[offset : offset + args.batch_size]
        request = bytearray(struct.pack(">I", len(batch)))
        for encoded in batch:
            oid = bytes.fromhex(encoded.decode())
            if len(oid) != oid_len:
                raise RuntimeError("object ID width mismatch")
            request.extend(struct.pack(">I", len(oid)))
            request.extend(oid)
        proc.stdin.write(frame(b"BGIN", batch_id, request))
        proc.stdin.flush()
        frames = 0
        while True:
            tag, got_batch, payload = read_frame(proc.stdout)
            if got_batch != batch_id:
                raise RuntimeError("batch ID mismatch")
            frames += 1
            if tag == b"ERRO":
                raise RuntimeError(f"helper error: {payload!r}")
            if tag == b"BEND":
                break
        print(
            f"batch={batch_id} commits={len(batch)} frames={frames} "
            f"rss_kib={rss_kib(proc.pid)} elapsed_s={time.monotonic() - started:.3f}",
            flush=True,
        )
    proc.stdin.write(frame(b"QUIT", 0))
    proc.stdin.flush()
    proc.stdin.close()
    status = proc.wait()
    stderr = proc.stderr.read().decode(errors="replace")
    if status or stderr:
        raise RuntimeError(f"helper exit {status}: {stderr}")


if __name__ == "__main__":
    main()
