#!/usr/bin/env python3
"""Randomized repo fuzzer: build random histories, compare vanilla vs patched
git output byte-for-byte across diff commands. Any mismatch is a patch bug."""
import os, random, subprocess, sys, shutil, hashlib

A = os.environ.get("WORKDIR", "/tmp/fastgit-fuzz")
VAN = os.environ["VAN"]
PAT = os.environ["PAT"]
ENV = dict(os.environ,
           GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_SYSTEM="/dev/null",
           GIT_CONFIG_NOSYSTEM="1",
           GIT_AUTHOR_NAME="f", GIT_AUTHOR_EMAIL="f@f",
           GIT_COMMITTER_NAME="f", GIT_COMMITTER_EMAIL="f@f",
           GIT_AUTHOR_DATE="2001-02-03T04:05:06Z",
           GIT_COMMITTER_DATE="2001-02-03T04:05:06Z")

def sh(cmd, cwd=None, binary=False):
    r = subprocess.run(cmd, cwd=cwd, env=ENV, capture_output=True)
    return r.returncode, r.stdout, r.stderr

ALPHABETS = [
    b"abcdefghij\n",                      # newline-rich text
    b"\x00\x01\xfe\xff\n ",               # binary-ish with newlines
    b"a\r\n\t ",                          # CRLF + whitespace
    bytes(range(256)) + b"\n" * 32,       # full byte range
    b"x\n",                               # degenerate 2-symbol (collision-heavy classes)
]

def rand_content(rng, alphabet, maxlen=20000):
    n = rng.randrange(0, maxlen)
    return bytes(rng.choice(alphabet) for _ in range(n))

def mutate(rng, data):
    ops = rng.randrange(1, 6)
    d = bytearray(data)
    for _ in range(ops):
        if not d:
            d = bytearray(rand_content(rng, rng.choice(ALPHABETS), 2000)); continue
        op = rng.randrange(4)
        pos = rng.randrange(len(d))
        if op == 0:   # insert random chunk
            d[pos:pos] = rand_content(rng, rng.choice(ALPHABETS), 500)
        elif op == 1: # delete span
            d[pos:pos + rng.randrange(1, 500)] = b""
        elif op == 2: # duplicate a span elsewhere (move-detection fodder)
            s = d[pos:pos + rng.randrange(1, 1000)]
            q = rng.randrange(len(d) + 1)
            d[q:q] = s
        else:         # flip bytes
            for _ in range(rng.randrange(1, 20)):
                d[rng.randrange(len(d))] = rng.randrange(256)
    return bytes(d)

CMDS = [
    ["log", "-p", "-U0", "--full-history", "--all", "--diff-filter=tuxdb"],
    ["log", "-p", "--all", "--diff-algorithm=histogram"],
    ["log", "-p", "--all", "--diff-algorithm=patience"],
    ["log", "-p", "--all", "--diff-algorithm=minimal"],
    ["log", "-p", "--all", "-w"],
    ["log", "-p", "--all", "-b"],
    ["log", "-p", "--all", "--ignore-cr-at-eol"],
    ["log", "-p", "--all", "--color=always", "--color-moved=zebra"],
    ["log", "-p", "--all", "--color=always", "--color-moved=blocks",
     "--color-moved-ws=ignore-space-change"],
    ["log", "-p", "--all", "-M", "-C", "--text"],
    ["log", "--stat", "--all"],
    ["log", "-p", "--all", "-W"],
    ["log", "-p", "--all", "--word-diff=porcelain"],
]

def one_round(seed):
    rng = random.Random(seed)
    repo = f"{A}/fuzzrepo-{seed}"
    shutil.rmtree(repo, ignore_errors=True)
    os.makedirs(repo)
    sh(["git", "init", "-q", "-b", "main"], cwd=repo)
    files = {}
    nfiles = rng.randrange(1, 8)
    for i in range(nfiles):
        files[f"f{i}.txt"] = rand_content(rng, rng.choice(ALPHABETS))
    ncommits = rng.randrange(2, 12)
    for c in range(ncommits):
        for name in list(files):
            r = rng.random()
            if r < 0.55:
                files[name] = mutate(rng, files[name])
            elif r < 0.60:
                del files[name]
                sh(["git", "rm", "-q", "-f", "--ignore-unmatch", name], cwd=repo)
                p = os.path.join(repo, name)
                if os.path.exists(p): os.unlink(p)
        if rng.random() < 0.3:
            files[f"new{c}.txt"] = rand_content(rng, rng.choice(ALPHABETS))
        for name, data in files.items():
            with open(os.path.join(repo, name), "wb") as fh:
                fh.write(data)
        sh(["git", "add", "-A"], cwd=repo)
        sh(["git", "commit", "-q", "--allow-empty", "-m", f"c{c}"], cwd=repo)
        # occasional branch + merge
        if c == ncommits // 2 and rng.random() < 0.5:
            sh(["git", "checkout", "-q", "-b", f"side{c}"], cwd=repo)
            k = rng.choice(list(files)) if files else None
            if k:
                files[k] = mutate(rng, files[k])
                with open(os.path.join(repo, k), "wb") as fh: fh.write(files[k])
            sh(["git", "add", "-A"], cwd=repo)
            sh(["git", "commit", "-q", "--allow-empty", "-m", "side"], cwd=repo)
            sh(["git", "checkout", "-q", "main"], cwd=repo)
            sh(["git", "merge", "-q", "--no-edit", "-X", "ours", f"side{c}"], cwd=repo)

    bad = []
    for cmd in CMDS:
        rv, ov, ev = sh([VAN, "-C", repo] + cmd)
        rp, op_, ep = sh([PAT, "-C", repo] + cmd)
        if rv != rp or ov != op_ or ev != ep:
            bad.append((cmd, rv, rp, hashlib.sha256(ov).hexdigest()[:12],
                        hashlib.sha256(op_).hexdigest()[:12]))
    if bad:
        print(f"SEED {seed}: {len(bad)} MISMATCHES (repo kept at {repo})")
        for b in bad: print("   ", b)
        return False
    shutil.rmtree(repo, ignore_errors=True)
    return True

def main():
    os.makedirs(A, exist_ok=True)
    start = int(sys.argv[1]); count = int(sys.argv[2])
    fails = 0
    for seed in range(start, start + count):
        if not one_round(seed):
            fails += 1
        if (seed - start + 1) % 25 == 0:
            print(f"progress {seed - start + 1}/{count} fails={fails}", flush=True)
    print(f"FUZZ-DONE range={start}..{start+count-1} fails={fails}")
    return 1 if fails else 0

if __name__ == "__main__":
    sys.exit(main())
