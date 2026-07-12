# Filesystem scanner tournament harness

This directory contains the reproducible benchmark harness for comparing
filesystem scanner designs. It runs real `betterleaks dir` processes in a
predeclared, randomized, balanced, fixed-horizon experiment. Raw runs remain
the primary artifact; the analyzer never hides invalid attempts.

The harness is Linux-only. The manifest command and corpus format are
portable. The Python tools use only the standard library; the Linux Go tools
also use the repository's pinned `golang.org/x/sys` dependency. All benchmark
paths and commands are explicit in the recorded plan and result rows.

## Components

- `manifest`: creates an immutable corpus manifest, verifies it, and freezes a
  scanner-output oracle.
- `runner`: validates the plan and artifacts, checks the corpus before and
  after every execution, runs balanced blocks, samples the process tree from
  procfs, and writes durable JSONL.
- `cachewarm`: reads selected regular files and uses bounded `mincore` probes
  to require a predeclared resident-page ratio before a warm run.
- `corpusgen`: creates the deterministic sparse/unusual correctness corpus;
  truncation and helper-crash cases remain focused test injections.
- `analyze.py`: computes paired log-ratio confidence intervals, pilot
  variability and run sizing, stratified bootstrap intervals, and the
  predeclared winner rules.
- `schema`: versioned structural JSON Schemas for the manifest, plan, scanner
  metrics, and raw result row. The runner remains authoritative for semantic
  and cross-field checks such as schedule digests and artifact coverage.
- `examples/host-local-plan.json`: a small uncontrolled-cache calibration for
  the GitLab FOSS checkout available on the development host. It is a harness
  smoke study, not pilot or confirmatory evidence.

## Build

From the repository root:

```sh
GO=${GO:-go}
export CGO_ENABLED=${CGO_ENABLED:-1}
RUN=/tmp/betterleaks-filescan
mkdir -p "$RUN/bin" "$RUN/home" "$RUN/manifests" "$RUN/results" "$RUN/work"
GOCACHE=/tmp/betterleaks-filescan-gocache "$GO" build -trimpath -o "$RUN/bin/betterleaks" .
GOCACHE=/tmp/betterleaks-filescan-gocache "$GO" build -tags=filescan_uring -trimpath -o "$RUN/bin/betterleaks-uring" .
GOCACHE=/tmp/betterleaks-filescan-gocache "$GO" build -trimpath -o "$RUN/bin/filescan-manifest" ./contrib/filescan/manifest
GOCACHE=/tmp/betterleaks-filescan-gocache "$GO" build -trimpath -o "$RUN/bin/filescan-runner" ./contrib/filescan/runner
GOCACHE=/tmp/betterleaks-filescan-gocache "$GO" build -trimpath -o "$RUN/bin/filescan-cachewarm" ./contrib/filescan/cachewarm
GOCACHE=/tmp/betterleaks-filescan-gocache "$GO" build -trimpath -o "$RUN/bin/filescan-corpusgen" ./contrib/filescan/corpusgen
```

Record `"$GO" version`, `CGO_ENABLED`, embedded `go version -m` metadata, and
dynamic-library versions with the evidence package; the 2026-07-11 local study
used Go 1.25.11 and `CGO_ENABLED=1`. Build every candidate from the same source
revision and toolchain. The `filescan_uring` build tag enables the raw io_uring candidate;
the untagged binary deliberately retains the unsupported stub. If
candidates are separate binaries, either give each immutable build its own
plan or use one immutable dispatch binary whose candidate argument selects the
backend. Never replace a binary after the plan has been frozen. Set
`binary_sha256` in every pilot and confirmation plan. Each such stratum must
also pin its manifest in `manifest_sha256` and the exact seeded schedule in
`schedule_sha256`. Generate the latter from a complete draft with
`filescan-runner -plan draft.json -print-schedule-digests`. List every explicit
config file, invoked hook executable, and other mutable auxiliary input in
`artifacts` with its SHA-256. Calibration may intentionally omit these pins for
broad exploratory sweeps; those rows remain ineligible for claims.

## Freeze and verify a corpus

The local GitLab checkout can exercise the workflow without downloading or
generating a large corpus:

```sh
RUN=/tmp/betterleaks-filescan
CORPUS=/local/home/ahrav/scratch/gitlab-foss
REVISION=754df18208dd903e1b05c7a4343da272079bea3d
"$RUN/bin/filescan-manifest" create \
  -root "$CORPUS" \
  -out "$RUN/manifests/gitlab-foss-local.json" \
  -id gitlab-foss-local-754df182 \
  -class source-tree-many-small \
  -role calibration \
  -origin https://gitlab.com/gitlab-org/gitlab-foss.git \
  -revision "$REVISION"
"$RUN/bin/filescan-manifest" verify \
  -manifest "$RUN/manifests/gitlab-foss-local.json" \
  -root "$CORPUS" -mode portable
"$RUN/bin/filescan-manifest" verify \
  -manifest "$RUN/manifests/gitlab-foss-local.json" \
  -root "$CORPUS" -mode live
```

Manifest output must be outside the corpus. Creation writes an atomic JSON
manifest and a content-addressed canonical entry-record sidecar. Replacing a
manifest never overwrites a sidecar referenced by an earlier snapshot. The
portable content
digest covers relative path, type, mode, logical size, symlink target, content
digest, and stable error category. It can identify a reconstructed copy at a
different path. The live guard additionally covers host metadata such as
device, inode, physical blocks, ownership, link count, mtime, and ctime. It is
cheap enough to run immediately before and after every timed execution and is
intentionally tied to the exact local tree. The sidecar digest protects the
frozen per-entry evidence.

Run a portable verification after acquisition or reconstruction. Do not use a
corpus if its manifest reports errors unless the correctness design explicitly
expects them; the plan must opt in with `allow_manifest_errors`.

For published evidence, record one exact origin and revision or immutable
object list for every corpus. Use independent calibration and confirmation
objects. Preserve the manifest, sidecar, acquisition instructions, binary,
plan, raw JSONL, and analysis together.

## Scanner metrics and correctness oracle

The runner always creates an inherited descriptor 3 and sets
`BETTERLEAKS_FILESCAN_METRICS_FD=3`. A scanner that supports the protocol may
write exactly one UTF-8 JSON object conforming to
`schema/scanner-metrics.schema.json`, then close the descriptor. It must not
write metrics to stdout or include post-exit work.

Each targets, fragments, findings, errors, and fallbacks identity contains a
record count, the number of bytes in the canonical records, and a SHA-256
digest. Canonicalization must be insensitive to incidental goroutine completion
order and must preserve every semantic field needed to detect selection,
framing, finding, error, and fallback differences. Backend timing and resource
fields are measurements, not substitutes for the five correctness identities.

The payload also records files and bytes actually served by each backend,
aggregate fallback reasons and error kinds, and peak queue, submitted,
completed-but-unconsumed, reorder, mapped, pinned, and in-flight state. These
diagnostics explain an arm; they never relax an oracle comparison.

Once a trusted baseline emits the protocol, freeze its output before measuring
other candidates:

```sh
RUN=/tmp/betterleaks-filescan
CORPUS=/local/home/ahrav/scratch/gitlab-foss
BETTERLEAKS_FILESCAN_METRICS_FD=3 \
  "$RUN/bin/betterleaks" --no-banner --exit-code=0 --log-level=error \
  --config=/local/home/ahrav/scratch/betterleaks-fork/betterleaks/config/betterleaks.toml \
  dir \
  --filescan-backend=baseline --filescan-namespace=parallel \
  --filescan-active-files=128 --filescan-detector-workers=64 \
  --filescan-walkers=64 --filescan-max-inflight-mib=64 --filescan-inline-detector \
  "$CORPUS" 3>"$RUN/manifests/gitlab-foss-oracle.json"
"$RUN/bin/filescan-manifest" oracle \
  -manifest "$RUN/manifests/gitlab-foss-local.json" \
  -metrics "$RUN/manifests/gitlab-foss-oracle.json"
```

The oracle command performs a full portable, live, and sidecar verification
before it updates the manifest.

If a scanner build does not emit this optional descriptor, keep
`require_scanner_metrics` false only for harness smoke and
timing calibration. Set it to true for correctness-gated pilot and
confirmation work. With it enabled, a missing, malformed, oversized, or
mismatched object is a candidate-owned invalid run.

## Run the local smoke calibration

The example records the exact config digest at the time it was written and
compares the current baseline arm with a 512 KiB sequential-advice buffered
arm. Its cache state is intentionally uncontrolled, so it can validate the
harness but cannot estimate pilot variance or support a performance claim.
Copy it, adjust candidates for the intended smoke run, and update paths and
SHA-256 values if the checkout changed. A real pilot must add a pinned cache
conditioning helper, residency verification, a correctness stratum, and the
exact binary, manifest, and sidecar hashes.

```sh
RUN=/tmp/betterleaks-filescan
cp contrib/filescan/examples/host-local-plan.json "$RUN/host-local-plan.json"
sha256sum "$RUN/bin/betterleaks" \
  /local/home/ahrav/scratch/betterleaks-fork/betterleaks/config/betterleaks.toml \
  "$RUN/manifests/gitlab-foss-local.json"
"$RUN/bin/filescan-runner" \
  -plan "$RUN/host-local-plan.json" \
  -results "$RUN/results/gitlab-local-smoke.jsonl" \
  >"$RUN/results/gitlab-local-smoke.summary.json"
python3 contrib/filescan/analyze.py \
  --plan "$RUN/host-local-plan.json" \
  --results "$RUN/results/gitlab-local-smoke.jsonl" \
  --output "$RUN/results/gitlab-local-smoke.analysis.json"
```

The results file is created exclusively; reruns require a new filename. Every
row contains the exact argv, effective environment digest and explicit
overrides, wall time, wait4 CPU/RSS/fault counters,
sampled procfs process-tree RSS/VSZ/fault/CPU/I/O counters, hooks, live-guard
results, optional scanner metrics, captured output, and a validity ledger. The
Hardened pilot and confirmation rows additionally contain the complete pinned
artifact identity set; legacy calibration rows predate that requirement. The
`host` record resolves the corpus root's `st_dev` major:minor and samples that
device's `/proc/diskstats` sectors, I/O time, weighted I/O time, and peak
in-flight count. It also records before/after and interval observations from
`/proc/vmstat`, `/proc/meminfo`, and `/proc/pressure/io`, with independent
availability, delta-availability, and sample-error ledgers.

Diskstats is scoped to the resolved corpus device but can still include other
traffic to that device. VM statistics, memory/cache gauges, and pressure are
explicitly host-global and not process-attributed. Their deltas are context for
detecting noisy runs; they are never interpreted as scanner-attributable
page-cache harm. Only a candidate's explicitly defined scanner metric may feed
the predeclared page-cache winner rule.

Command assembly is deliberate: `common_args` are persistent Betterleaks flags,
the first `scanner_args` element is the subcommand (`dir`), candidate `args` are
inserted next as subcommand-local experiment flags, and the remaining
`scanner_args` are subcommand arguments, including local flags and corpus
paths. Candidate arguments accept only the runner's canonical `--filescan-*`
allowlist; valued options use `--option=value`, and every option name is unique
within that arm. `common_args` and `scanner_args` may not set them, so later
arguments cannot silently override the frozen candidate configuration.

## Experimental contract

Each block contains every candidate once. The runner uses a Williams balanced
Latin schedule: candidate positions and first-order carryover are balanced over
a complete cycle, and cycles are deterministically randomized from the plan
seed. An odd candidate count uses the rows and their reversals. The fixed
horizon must be a whole cycle for pilot and confirmation. Calibration may use
any positive block count; it takes a deterministic randomized prefix of a
Williams cycle so broad sweeps do not incur a full cycle per configuration.

Infrastructure-invalid blocks are written in full or in their observed partial
form and then repeated with the same candidate sequence, up to the predeclared
cap. They are never silently discarded. Candidate-owned failures are not
replaced, and a candidate with any such failure is disqualified by the
analyzer. A timeout kills the candidate process group. Results are flushed and
`fsync`ed after every block.

Use these phases:

1. `calibration`: sweep candidate parameters only on held-out objects and keep
   all negative results.
2. `pilot`: run 6--8 balanced blocks, estimate paired log-ratio variance and
   baseline coefficient of variation, and use the reported inflated sample
   size to freeze a confirmation horizon.
3. `confirmation`: run the frozen horizon without early stopping. Only this
   phase is eligible to advance a candidate.

Every plan stratum also declares `evidence_role`. A `performance` stratum must
use a `calibration` manifest in calibration/pilot plans and a held-out
`confirmation` manifest in confirmation plans. A `correctness` stratum must use
a `correctness` manifest. Correctness strata run in every block and can
disqualify a candidate, but the analyzer excludes their timing from winner
estimates.

`allowed_exit_codes` defaults to `[0]`. Performance strata are permanently
restricted to `[0]`. A correctness stratum may predeclare additional nonzero
codes for expected permission/error gates; an allowed, ordinary nonzero exit is
recorded as informational and still requires all five oracle identities to
match. Timeouts, signals, crashes, wait failures, and undeclared codes remain
errors regardless of this list.

Before execution, the runner hashes the binary, artifacts, manifests, and
sidecars and derives the seed-fixed schedule. Before making any claim, the
analyzer checks each row's recorded identities and observed-order digest
against those plan pins, then checks the full fixed horizon, candidate
membership, position balance, and first-order carryover balance. Missing,
inconsistent, or mismatched provenance pins make the whole study
claim-ineligible. It then uses two-sided Student-t intervals for
per-stratum paired log
ratios and an equal-weight, within-stratum bootstrap for each workload class.
The familywise alpha is divided across the predeclared winner claims and
nonbaseline candidates. A performance winner must improve end-to-end wall time
by at least the minimum worthwhile effect, have an upper ratio bound below 1,
and have no stratum whose upper bound exceeds the equivalence guard. A resource
winner must satisfy the wall-time equivalence guard and have an upper bound
beyond the predeclared CPU, RSS, or page-cache improvement threshold. Raw read
throughput alone is never a winner rule.

Cache state is part of each stratum. Warm runs require a documented warm-up
hook and a verification policy. Controlled-cold runs require a host-approved,
predeclared hook; do not claim cold-cache evidence merely because the process
was restarted. The runner performs the before live guard first and cache
conditioning last, immediately before the timed process; its after guard still
rejects hook or candidate mutations. Scanner metrics must record backend queue depth, in-flight,
mapped, pinned, completed-but-unconsumed, reorder, page-cache, error, and
fallback behavior when those concepts apply.

## Corpus matrix and evidence boundary

Performance confirmation needs at least these independently acquired classes:

- two structurally independent large monorepos;
- a package cache, dependency tree, or extracted layers with millions of
  1--32 KiB files;
- immutable sequential WARC, logs, dumps, or artifacts totaling at least twice
  host RAM;
- a mixed enterprise-style source/generated/binary/archive/document tree;
- archive-heavy releases, package archives, and nested supported archives.

Keep a seventh sparse/unusual corpus as a correctness and operational gate,
including sparse files, long lines, missing final newlines, Unicode paths,
symlinks, permissions failures, and truncation races. It does not replace the
six performance corpora.

The development host is Linux/aarch64 with 64 logical CPUs, about 123.5 GiB
RAM, and XFS on an EBS-backed NVMe block device. The available GitLab FOSS checkout at revision
`754df18208dd903e1b05c7a4343da272079bea3d` contains 70,322 non-`.git` regular
files totaling about 553 MB logical. It is useful only as a many-small-file,
natural-warm local smoke workload. This environment does not provide a
controlled page-cache reset, has an 8 MiB locked-memory limit, and does not
contain a real immutable sequential corpus of at least about 247 GiB. Claims
about controlled-cold, above-RAM, direct I/O, mmap, or io_uring behavior require
external confirmation on dedicated hosts and the full corpus matrix.

## Validate the harness

These checks use only tiny temporary corpora:

```sh
GO=${GO:-go}
GOCACHE=/tmp/betterleaks-filescan-gocache "$GO" test ./contrib/filescan/...
GOCACHE=/tmp/betterleaks-filescan-gocache "$GO" test -race ./contrib/filescan/...
GOCACHE=/tmp/betterleaks-filescan-gocache "$GO" vet ./contrib/filescan/...
python3 -m unittest discover -s contrib/filescan -p '*_test.py'
python3 -m py_compile contrib/filescan/analyze.py
```
