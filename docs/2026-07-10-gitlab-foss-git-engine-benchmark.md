# GitLab FOSS Git-engine benchmark and decision

Date: 2026-07-10

## Decision

The earlier betterleaks-repository model was a mechanics check, not a valid
main-use-case benchmark. The primary workload is now a full, unfiltered clone
of GitLab FOSS.

The ten previously listed items are **not ten sequential implementation
steps**. They collapse into two gated tournaments. The engine tournament has
now produced a practical winner:

1. **Engine result: advance the scanner-native Git builtin on ordinary
   files.** It is the only native arm that matches the complete 197,173-commit
   GitLab oracle and has a rotated three-replicate same-effective-profile
   measurement. Default custom Git had an observed 111.432-second wall median
   versus 125.337 seconds for the stock typed reference, but used 19.340 GiB
   aggregate helper RSS versus 9.347 GiB. Recycling after every batch with
   `64m/512m` used 6.770 GiB and had a 121.471-second wall median. Three runs
   and visible drift do not establish statistical significance; they expose
   the speed/memory policy choice for scanner-level integration.

   Libgit2 reached exact 595-commit self-history parity only after two public-
   API compatibility adapters, then its corrected GitLab run was stopped after
   an observed 1,098-second lower bound without final counts or a digest. Gix
   also matches self-history, but its most Git-faithful tested standalone rename
   matcher still misses six of the first 100 GitLab commits. A narrow stock-Git
   fallback makes that slice exact and localizes the gap; it is a hybrid control,
   not a standalone gix win.

   Corrected stock persistent `git diff-tree --stdin` remains the low-cost text
   control: it beat an equivalently drained ephemeral run, so it was not
   rejected. It is not one of the three native-record implementations.
2. **Conditional next tournament: storage representation and backend.** Hold
   that engine fixed; compare packed Git objects with a pre-inflated immutable
   ODB on normal files. Only if cold-storage time remains material, compare
   pread/mmap, io_uring, io_uring command passthrough, and SPDK through the same
   xNVMe interface.

SPDK ublk is an optional control for unmodified Git. SPDK Blobstore is a
possible backend for a winning direct ODB. A full userspace POSIX filesystem is
not planned unless the direct ODB wins and compatibility with unmodified Git
becomes a demonstrated requirement.

## Representative workload

```text
Repository:  gitlab-org/gitlab-foss
Clone:       full `git clone --no-checkout`; no shallow history or blob filter
HEAD:        682c0cd4780176231f313a28758e74b89be59a1a
Refs:        6,252
Commits:     197,173 reachable from --all
Objects:     3,436,603, all packed
Pack:        1,955,974,039 bytes
Ref digest:  d3a5a08289c2471fe300abcb50c58308fda1e43c78d2fd77fa667b608d3711d9
Rev digest:  16fd50d990ed2df94e6384a542c1aa2bfb2054fb83ca0aced917115a8f441eeb
Host:        Apple arm64, 10 logical CPUs, 32 GiB RAM
Git:         2.51.0
```

The ref digest is SHA-256 over byte-sorted
`for-each-ref --format='%(refname)%00%(objectname)'` output. The revision digest
is SHA-256 over `rev-list --all` in its emitted order. Candidate runs must
recheck both values so an advancing remote ref or reordered commit input does
not silently change the workload.

The default scan produced about 4.18 GB of scanner input and 4,712 findings.
This is qualitatively different from the approximately 46 MB betterleaks
self-history trace.

## Full-history measurements

| Measurement | Wall | User CPU | System CPU | Block inputs |
| --- | ---: | ---: | ---: | ---: |
| Betterleaks, default config | 189.02 s | 1,175.25 s | 119.40 s | 0 |
| Source/parser/scheduler with the same global prefilter and zero rules | 128.73 s | 747.25 s | 73.16 s | 0 |
| Git production only, production batching, output drained | 88.36 s | 637.59 s | 37.70 s | 0 |

The source-only config was derived from `betterleaks config show`: it retains
the default global prefilter and removes rule tables. It therefore preserves
path skipping much more closely than the earlier empty-config measurement.
The source-only and full runs scanned 4.14 GB and 4.18 GB respectively, so this
is still a stage-envelope measurement rather than a perfectly subtractive CPU
decomposition.

Conditional stage bounds are now large enough to justify embedded-engine
work:

- Git production alone is 46.7% of observed end-to-end wall. If all of it were
  critical-path work, a 2x Git engine could remove at most 23.4% end to end.
- The broader source/parser/scheduler envelope is 68.1% of end-to-end wall. A
  2x native-record source could remove at most 34.1%; an infinite replacement
  cannot exceed 68.1% without also changing detection.

These are conditional Amdahl bounds, not expected wins. Production overlaps
Git, parsing, archive handling, and detection.

### Corrected native-engine full-history result

The final warm-cache comparison hard-gated every row on the frozen output
identity: 197,173 commits, 23,792,827 records, 5,417,932,516 canonical bytes,
and digest
`15bf91a9c237467054c9e2f3e11f405109256231e5c7928d13621b2cdb7d6e72`.
Reference and custom Git used the same effective isolated profile and 128 MiB
delta-base cache. The order rotated across three rounds. Parent-environment
absence of `GIT_DIFF_OPTS` and inherited `GIT_CONFIG_*` injection was
reconstructed immediately after the runs rather than captured at launch; the
harness now removes those inputs mechanically and tests hostile values.

| Configuration | Wall median, range | Scan median, range | Aggregate helper RSS median, range |
| --- | ---: | ---: | ---: |
| Stock typed reference | 125.337 s, 124.349–145.309 | 122.940 s, 122.360–142.608 | 9.347 GiB, 8.757–9.656 |
| Custom Git, default pack settings | 111.432 s, 108.131–112.648 | 108.768 s, 105.348–109.657 | 19.340 GiB, 19.253–19.365 |
| Custom Git, `64m/512m`, recycle every batch | 121.471 s, 116.070–147.430 | 118.313 s, 113.637–145.156 | 6.770 GiB, 6.725–6.842 |

Default custom Git's observed wall range did not overlap the reference range
and its median was 11.1% lower, at 106.9% higher median helper RSS. The bounded
arm's wall range overlapped both alternatives; relative to reference its median
was 3.1% lower and helper RSS was 27.6% lower. Relative to custom default it was
9.0% slower and used 65.0% less helper RSS. These are observed three-run
effects, not confidence intervals or a release performance promise.

Every row reported zero sampled helper page-ins. RSS covers matching engine
helpers only and excludes the Go benchmark coordinator/root process. This is a
CPU, mmap-residency, and process-lifetime result on a warm macOS host, not
evidence about cold storage or SPDK. Earlier custom timing/helper-RSS artifacts used
Git's 96 MiB default delta cache against a 128 MiB reference and are marked
`profile_confounded_delta_cache_96m_vs_128m`; their output identity remains
valid but they do not enter this table.

## Persistent stock Git result

All Git-only arms used the production `buildBatches` shape: 81 batches made of
16-commit locality runs, shared across ten workers.

These stock-control trials use the current legacy command/config behavior and
predate the fully pinned Betterleaks scan profile. They isolate process/cache
lifetime, but candidate acceptance will rerun the controls under the pinned
profile and exact record oracle.

The original `/dev/null` diagnostics and the fair pipe-drained comparison are
shown separately. Discarding stdout in the kernel is useful for a Git-production
floor, but cannot be compared directly with a protocol that copies and searches
4.18 GB in userspace.

| `/dev/null` diagnostic | Processes | Wall | User CPU | System CPU | Minor faults |
| --- | ---: | ---: | ---: | ---: | ---: |
| Current short-lived `git log` batches | 81 | 88.36 s | 637.59 s | 37.70 s | 9.40 M |
| Persistent `diff-tree`, static assignment | 10 | 95.43 s | 641.96 s | 25.11 s | 3.70 M |

| 1 MiB chunk-drained arm | Trial | Processes | Wall | User CPU | System CPU | Minor faults | Versus paired ephemeral |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Short-lived `git log` batches | 1 | 81 | 113.12 s | 639.23 s | 45.84 s | 9.27 M | baseline |
| Persistent dynamic `diff-tree` | 1 | 10 | 104.30 s | 642.67 s | 34.33 s | 4.04 M | 7.8% faster |
| Short-lived `git log` batches | 2 | 81 | 150.40 s | 629.45 s | 44.60 s | 9.36 M | baseline |
| Persistent dynamic `diff-tree` | 2 | 10 | 88.63 s | 623.30 s | 30.29 s | 3.63 M | 41.1% faster |
| Short-lived `git log` batches | 3, run first | 81 | 128.11 s | 633.16 s | 42.47 s | 9.48 M | baseline |
| Persistent dynamic `diff-tree` | 3, run second | 10 | 129.60 s | 631.57 s | 35.77 s | 4.03 M | 1.2% slower |

The static arm proves persistence reduces process-related costs: minor faults
fell 60.7% and system CPU fell 33.4%. It nevertheless lost wall time because
static ownership reintroduced stragglers that the shared production queue was
designed to correct.

The first dynamic arm was invalid. It used a valid empty-tree request as a
boundary; Git printed that request but did not flush it while stdin remained
open. Adding `stdbuf -oL` changed the protocol to per-line flushing and produced
the obsolete 128.66-second result.

The corrected protocol sends a non-OID request marker. Git's invalid-input
branch echoes the complete line and explicitly flushes stdout. A second bug
appeared only on the full repository: serially writing a large request before
draining output deadlocked when both pipes filled. The final harness writes
stdin and drains stdout concurrently and retains that failure as a regression
case.

With equivalent pipe draining, persistence won the two pairs in which it ran
first (7.8% and 41.1%) and lost the reversed-order pair by 1.2%. Wall time is
therefore strongly order/cache sensitive and no central estimate is promoted.
The lower process count consistently reduced system CPU and minor faults, so
the mechanism remains worth a controlled alternating series. The stock arm
still materializes roughly 4 GB of patch text and retains the Go parser, so the
native-record tournament tests a different ceiling.

## Correctness result

On 64 recent commits, four individual 16-commit `git log` and `diff-tree`
batches were byte-identical. Concatenating the four short-lived `git log`
streams omitted one blank line at each former process boundary, while the
persistent stream retained it: 43,760,004 versus 43,760,007 bytes. The fast
parser ignores these top-level blank lines, but the eventual engine gate must
compare native records and final findings, not require textual byte identity
across subprocess boundaries.

### First native-record full-history diagnostic

The first custom-Git builtin run and its identical-driver stock reference both
covered the frozen ref/revision digests and emitted the same record counts:

| Engine | Scan wall | Commits | Files | Hunks | Canonical bytes | Digest prefix |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| Custom Git builtin | 148.45 s | 197,173 | 2,791,965 | 20,803,689 | 5,417,901,723 | `9731411e` |
| Stock Git typed reference | 174.58 s | 197,173 | 2,791,965 | 20,803,689 | 5,417,932,516 | `15bf91a9` |

The 15.0% wall difference is **diagnostic only**. The custom output is 30,793
canonical bytes shorter and its multiset digest differs, despite identical
record counts. A streaming per-commit differential reported 1,691 affected
commits, all in commit-message projection rather than file/hunk data. That
exact first-pass count is a contemporaneous observation because its transcript
was not retained. Two
distinct assumptions failed: pretty-medium expands body tabs to eight spaces,
and the Go reference trims Unicode White_Space (including U+0085), while the
first C normalizer retained tabs and trimmed only bytewise `isspace`. The
candidate cannot advance until those cases are fixed and the full digest
matches. These runs were also ordered custom then reference and are not an
interleaved performance series.

The corrected implementation sets Git's medium-format tab expansion to eight
columns and mirrors the Go parser's Unicode White_Space and four-space body
indent handling. A second production-shaped streaming comparison then passed
all 197,173 commits with zero mismatches. This promotes custom Git through the
full semantic gate; it does not retroactively validate the first run's timing,
which used different output and ran before the memory/recycling experiments.

Peak memory also needs a separate gate. Late in the custom run, ten persistent
helpers occupied roughly 17.5 GiB combined. Stock reference patch processes
also had large per-batch peaks, but a sampled aggregate was about 5.1 GiB.
The production-shaped batches contain roughly 2,400 commits, so follow-up must
separate unavoidable single-batch working set from state retained across the
roughly eight sequential batches handled by each persistent worker.

The first gix+safe-libgit2-diff hybrid full gate also failed to generalize its
exact 595-commit result. It matched all 197,173 commits and 2,791,965 files but
emitted 20,765,560 hunks, 38,129 fewer than the reference, while producing
53,256,094 more canonical bytes. Its digest differed and helper RSS reached
roughly 18-20 GiB combined. The 1,113-second wall time overlapped other
correctness scans and is invalid for speed; the semantic and memory failures
stand independently. Follow-up is restricted to small failing GitLab slices
until the mismatch mechanism is known.

The original default matcher's first 100-commit slice contained 15 mismatching
commits. The earliest,
`b55e25fd...`, showed the mechanism: Git matched dozens of edited renames,
while gix reported the corresponding destination files as additions. That
preserved the file count but replaced rename-relative hunks with whole-file
addition hunks. A 40% versus 50% threshold change had no effect, consistent
with a different similarity metric and assignment strategy rather than a
simple threshold crossover.

The exploration continued rather than stopping at that first matcher. An
artificial descending-source tie-break reduced the slice to two mismatches.
The most Git-faithful tested standalone design—encounter order, four candidates
per destination, and stable score/basename sorting—had six mismatches. Git's
full 0..60000 score resolution left the same six, ruling out score truncation.
The opt-in `BETTERLEAKS_GIX_AMBIGUOUS_RENAME_FALLBACK=1` path invokes stock Git
only for competing score/name ties and matched the three targeted commits and
all 100 commits. This localizes the remaining gap to candidate admission/source
ordering around exact and basename pre-matches; it is not a standalone gix win,
and no full fallback performance run was made.

The corrected libgit2 GitLab gate produced a separate negative result. It was
user-stopped after an observed 1,098-second (>18-minute) lower bound, with ten
helpers at observed helper-RSS lower bounds of 7,131,568 KiB aggregate and
763,440 KiB per helper. The run emitted no benchmark object, counts, or digest,
so it supports no full-history correctness conclusion. Timing is quarantined;
a brief documentation-search overlap at startup reinforces that restriction.
The scalability loss is enough to park libgit2 from contention as the production winner
without pretending the incomplete run passed semantics.

## What happens next

### Engine tournament outcome

0. **Stock persistent Git — text control.** Keep the corrected dynamic worker
   as the lower-effort baseline. `diff-tree` does not expose `--use-mailmap`,
   while the pinned `git log` profile does, so this is not yet a semantic
   fallback on repositories with mailmap mappings. Test a small post-parse
   mailmap adapter or preflight restriction before considering adoption.
1. **Custom Git builtin — advance to integration.** The framed native-record
   engine, full GitLab semantic gate, and corrected warm-cache measurement are
   complete. Put it behind an explicit preflight/feature boundary and run
   scanner-level findings parity plus repeated end-to-end scans. Choose default
   persistence or recycle-after-one from an explicit helper-memory budget.
2. **Libgit2 helper — park.** Retain the self-history adapters and partial-run
   evidence, but do not spend another full GitLab run without a new hypothesis
   capable of changing the >18-minute scalability result.
3. **Gix helper — retain as a localization/hybrid control.** A standalone gix
   arm advances only if candidate admission/order becomes Git-exact without
   stock fallback. Otherwise the fallback's complexity and process crossover
   need an explicit product reason before any full run.

The portable current path remains the oracle and fallback. The custom builtin
does not become a release default until findings parity, packaging/GPL source
compliance, maintained Git-version rebasing, and repeated end-to-end benefit
are demonstrated.

### Storage tournament: conditional, not sequential adoption

After the custom engine passes that integration gate:

1. Compare its existing packed-object reader against an immutable pre-inflated
   OID/segment store on ordinary files.
2. Profile warm and cold repositories on Linux with a pack larger than RAM.
3. If storage is at least 10% of source wall, hold the segment format fixed and
   compare pread/mmap, io_uring, io_uring_cmd, and SPDK through xNVMe.
4. Run SPDK ublk plus ext4/XFS as the unmodified-Git control.

The current macOS runs recorded zero block-input operations because the 1.96 GB
(1.82 GiB) pack was resident in a 32 GiB host. They do not reject SPDK for cold
larger-than-RAM Linux deployments; they show that SPDK cannot be evaluated
honestly on this warm host.

## Reproduction

The Git-production harness is
[`contrib/fastgit/benchmark_persistent_git.py`](../contrib/fastgit/benchmark_persistent_git.py).
Raw results are
[`contrib/fastgit/gitlab_foss_results_20260710.jsonl`](../contrib/fastgit/gitlab_foss_results_20260710.jsonl).

Corrected custom-Git rows and content-addressed closeout provenance are
[`contrib/gitengine/custom-git/results/gitlab-full-warm-corrected-profile-2026-07-10.jsonl`](../contrib/gitengine/custom-git/results/gitlab-full-warm-corrected-profile-2026-07-10.jsonl)
and
[`contrib/gitengine/custom-git/results/gitlab-full-warm-corrected-profile-2026-07-10.provenance.json`](../contrib/gitengine/custom-git/results/gitlab-full-warm-corrected-profile-2026-07-10.provenance.json).
Gix localization evidence is in
[`contrib/gitengine/gix-helper/README.md`](../contrib/gitengine/gix-helper/README.md).
The quarantined libgit2 partial row is
[`contrib/gitengine/libgit2-helper/results/gitlab-foss-full-corrected-20260710.jsonl`](../contrib/gitengine/libgit2-helper/results/gitlab-foss-full-corrected-20260710.jsonl)
with a sibling provenance JSON.

```sh
git clone --no-checkout https://gitlab.com/gitlab-org/gitlab-foss.git \
  /Users/ahrav/Projects/benchmarks/gitlab-foss

python3 -B contrib/fastgit/benchmark_persistent_git.py \
  /Users/ahrav/Projects/benchmarks/gitlab-foss \
  --arm=ephemeral-drain --commits=0 --workers=10

python3 -B contrib/fastgit/benchmark_persistent_git.py \
  /Users/ahrav/Projects/benchmarks/gitlab-foss \
  --arm=persistent-static --commits=0 --workers=10

python3 -B contrib/fastgit/benchmark_persistent_git.py \
  /Users/ahrav/Projects/benchmarks/gitlab-foss \
  --arm=persistent-dynamic --commits=0 --workers=10
```

The original stage-envelope and stock-persistence figures are single
frozen-trace trials. The corrected custom/reference table has three rotated
observations per arm, still too few for a final release claim. Candidate
acceptance requires scanner-level repeated runs sized from observed
execution-level variance.

Native candidates use the common digesting driver. It mirrors the production
16-commit locality runs and shared batch queue, reuses each helper across
sequential batches, validates helper counters, and reports an
order-independent digest over every typed record:

```sh
go run ./contrib/gitengine/bench \
  -engine reference \
  -repo /Users/ahrav/Projects/benchmarks/gitlab-foss \
  -workers 10 \
  -expected-ref-digest d3a5a08289c2471fe300abcb50c58308fda1e43c78d2fd77fa667b608d3711d9 \
  -expected-revision-digest 16fd50d990ed2df94e6384a542c1aa2bfb2054fb83ca0aced917115a8f441eeb
```

Use `-engine custom-git`, `libgit2`, or `gix` plus the corresponding
`-helper` executable for a candidate. A timing is not eligible for comparison
unless its record counts, canonical byte count, and multiset digest match the
reference.
