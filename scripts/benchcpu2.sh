#!/usr/bin/env bash
# benchcpu2.sh - CPU-primary measurement harness (session 3).
#
# The box is shared with another autoresearch session, so wall-clock is
# unreliable. Primary metric: CPU-seconds (user+sys of the whole process
# tree, via getrusage children); cpu_min across runs is the cleanest
# estimate of true work. Co-primary: whole-tree peak RSS via a transient
# systemd user-scope cgroup (memory.peak), which includes git subprocess
# workers AND charged page cache for mmap'd packs.
#
# USAGE
#   BIN=./betterleaks EXTRA_ARGS="--scan-mode=pack" benchcpu2.sh <label> <corpus> [runs]
#
# Emits one TSV row:
#   label  runs  cpu_mean  cpu_std  cpu_min  wall_mean  wall_min  tree_peak_rss_mb  findings  digest  load1
set -euo pipefail

BIN="${BIN:-./betterleaks}"
EXTRA_ARGS="${EXTRA_ARGS:-}"
label="$1"
corpus="$2"
runs="${3:-3}"

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT
report="${workdir}/report.json"

# Load gate: CPU-seconds tolerate contention, but heavy oversubscription
# still skews sys-time. Wait for load < LOAD_MAX (default 12, up to 30 min).
LOAD_MAX="${LOAD_MAX:-12}"
for _ in $(seq 1 360); do
    load=$(awk '{print $1}' /proc/loadavg)
    ok=$(awk "BEGIN{print (${load} < ${LOAD_MAX}) ? 1 : 0}")
    [[ "$ok" == "1" ]] && break
    sleep 5
done

read -r cpu_mean cpu_std cpu_min wall_mean wall_min peak_mb < <(
    RUNS="$runs" B="$BIN" C="$corpus" E="$EXTRA_ARGS" R="$report" python3 - <<'PY'
import os, resource, subprocess, sys, time, math, glob, random

runs   = int(os.environ["RUNS"])
cmd_base = [os.environ["B"], "--no-banner", "-l", "error", "--exit-code", "0",
            "git", os.environ["C"]] + os.environ["E"].split() + ["-f", "json", "-r", os.environ["R"]]

def one():
    unit = f"bl3-{os.getpid()}-{random.randint(0,999999)}"
    cmd = ["systemd-run", "--user", "--scope", f"--unit={unit}",
           "-p", "MemoryAccounting=yes", "-q", "--"] + cmd_base
    r0 = resource.getrusage(resource.RUSAGE_CHILDREN); t0 = time.time()
    p = subprocess.Popen(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    peak = 0
    cg = None
    while p.poll() is None:
        if cg is None:
            hits = glob.glob(f"/sys/fs/cgroup/user.slice/*/user@*.service/*/{unit}.scope") \
                 or glob.glob(f"/sys/fs/cgroup/user.slice/**/{unit}.scope", recursive=True)
            cg = hits[0] if hits else None
        if cg:
            try:
                with open(cg + "/memory.peak") as f:
                    peak = max(peak, int(f.read()))
            except OSError:
                pass
        time.sleep(0.05)
    wall = time.time() - t0
    r1 = resource.getrusage(resource.RUSAGE_CHILDREN)
    cpu = (r1.ru_utime - r0.ru_utime) + (r1.ru_stime - r0.ru_stime)
    return wall, cpu, peak

one()  # warmup (also populates page cache)
walls, cpus, peak = [], [], 0
for _ in range(runs):
    w, c, pk = one(); walls.append(w); cpus.append(c); peak = max(peak, pk)
n = len(cpus)
cm = sum(cpus)/n
cs = math.sqrt(sum((x-cm)**2 for x in cpus)/n) if n > 1 else 0.0
wm = sum(walls)/n
print(f"{cm:.2f} {cs:.2f} {min(cpus):.2f} {wm:.3f} {min(walls):.3f} {peak/1048576:.0f}")
PY
)

read -r findings digest < <(python3 scripts/digest.py "${report}")
load1=$(awk '{print $1}' /proc/loadavg)

printf "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n" \
    "$label" "$runs" "$cpu_mean" "$cpu_std" "$cpu_min" "$wall_mean" "$wall_min" "$peak_mb" "$findings" "$digest" "$load1"
