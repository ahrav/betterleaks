# Userspace Git engine and SPDK performance ceiling

Date: 2026-07-10

> **Decision update:** The local betterleaks-repository model below is retained
> as a simulation-methodology artifact, not as main-use-case evidence. It has
> been superseded by the full-history
> [GitLab FOSS benchmark](2026-07-10-gitlab-foss-git-engine-benchmark.md),
> which changes the source-stage ceiling. It invalidates the first stock
> `diff-tree --stdin` delimiter experiment and shows a corrected dynamic worker
> beating an equivalently pipe-drained ephemeral control by 7.8% in one trial.

## Executive conclusion

The overreaching idea is viable, but the useful boundary is narrower than a
general-purpose userspace filesystem:

```text
Git-aware revision/object/diff engine
  -> scanner-native file and hunk records
  -> optional content-only candidate cache
  -> betterleaks metadata filtering and findings
```

The storage layer should be replaceable underneath the object engine:

```text
ordinary files/pread or mmap
  -> xNVMe POSIX/io_uring backend
  -> xNVMe/SPDK or SPDK Blobstore backend
```

For the captured warm betterleaks workload, the complete source/parser/scheduler
stage is only 0.652 seconds of a 3.9575-second scan. Under a stage-preserving
model with no shared-core relief or source/detector fusion, replacing that stage
with an infinitely fast implementation has a conditional optimistic bound of
16.48% end to end, before accounting for its existing overlap with detection.
The finite-pipeline sensitivity model produces about 1.085x wall speedup
(roughly a 7.8% reduction) at a 2x engine and about 1.178x (roughly a 15.1%
reduction) at an effectively infinite engine under its fitted historical
balance. These are model outputs, not predictions. Both remain below the
conditional bound. A fused design that also removes detector work is
deliberately outside it.

SPDK by itself is much smaller. The current checkout has one 4.71 MiB pack and the
warm producer recorded no block-input operations. Even assuming storage is 30%
of source-stage time and importing the report's configuration-specific 1.096x
SPDK/io_uring ratio at queue depth 128, the conditional end-to-end bound is
about 0.43%. Warm, dependent Git-object reads should be modeled nearer queue
depth one, where the corresponding bound is about 0.1%.

The best first prototypes are therefore:

1. Stock `git diff-tree --stdin` as a persistent streaming engine per logical
   worker. This preserves Git's exact diff semantics and decoded-object cache
   lifetime without a Git fork.
2. Only if native records remain worthwhile, a small scanner-native Git
   builtin/daemon that removes text materialization as well.
3. A coarse-batch libgit2 shim on the normal filesystem. Its custom ODB API is
   the cleanest later insertion point for a non-filesystem object store.
4. A gix Rust helper/static library, measured independently because its diff
   and rename semantics are not identical to Git.
5. A go-git development mmap prototype as the cheapest Go-only object-access baseline,
   not as the assumed finalist.
6. Only after an embedded engine shows cold object reads are material: place
   the same immutable object layout behind xNVMe and then SPDK.

One adjacent result has a larger ceiling than the storage work: 47.9% of added
hunk bytes in the captured history are exact repeats. A content-only detector
candidate cache could attack the dominant detector stage rather than remaining
under the 16.48% source-stage ceiling.

## Current architecture and contract

The single-worker semantic form is `git log -p -U0 --full-history --all
--diff-filter=tuxdb`. The current default `ParallelGit` path instead runs
`git rev-list --all`, forms commit batches, and repeatedly launches `git log
--no-walk --stdin` processes. It reads each stdout pipe through the default fast
parser's 64 KiB buffered reader and emits batches of 64 scanner-native file
records. User-provided `--log-opts` remain on the compatibility parser.

Relevant code:

- [`sources/git.go`](../sources/git.go): constructs the Git process and stdout
  pipe, selects the default fast path, and projects native files into scanner
  fragments.
- [`sources/gitlog_fastparse.go`](../sources/gitlog_fastparse.go): owns the
  scanner-minimal record contract and parser.
- [`sources/parallel_git.go`](../sources/parallel_git.go): partitions commits
  across ten producers on the measurement host.

An embedded replacement must preserve:

- revision coverage under `--all --full-history`, including root and merge
  commits;
- commit SHA, message, author, and author date;
- destination path plus delete and binary state;
- hunk new-line position and exact added bytes;
- archive blob lookup for the destination object;
- `.gitattributes`, binary classification, rename/copy, diff algorithm, and
  hunk-boundary behavior relevant to the scanner contract;
- cancellation, bounded ownership, and repack/snapshot consistency.

It does **not** need to construct a general `gitdiff.File` graph or serialize a
textual patch if it emits the native contract directly.

## Grounded measurements

| Quantity | Measurement |
| --- | ---: |
| Final end-to-end mean | 3.9575 s |
| Source/parser/scheduler only, 10 workers | 0.652 s mean; 0.51-0.81 s |
| Source/parser/scheduler only, 1 worker | 0.845 s mean; 0.68-1.03 s |
| Git objects | one 4.71 MiB pack; 3,687 packed plus 164 loose objects |
| Decoded object bytes | 41,568,300 bytes versus 5,059,704 bytes on disk; 8.22x |
| Captured patch stream | 46.41 MB (44.26 MiB) |
| Added-text payload | 23.96 MB (22.85 MiB) |
| File-diff jobs | 3,461 |
| Parser replay | 34.09 ms |
| Parser routines in final CPU profile | 0.70% cumulative |
| External detector code in final CPU profile | 72.82% |
| Nonempty added-hunk payloads | 12,727 |
| Unique exact added-hunk payloads | 5,130 |
| Duplicate added bytes | 11.47 MB (10.94 MiB), 47.9% |

The object-to-patch expansion is about 10x. A separate `git cat-file
--batch-all-objects` measurement found that a flat decoded-object store would
be 8.22x the current on-disk object bytes in this checkout (8.72x for blobs,
4.84x for trees, and 1.54x for commits). That is a measured warning for this
small repository, not a general expansion factor. Across file-diff jobs, both
source and detector work are extremely skewed:

| Distribution | p50 | p90 | p99 | max | top-five share |
| --- | ---: | ---: | ---: | ---: | ---: |
| Patch-section bytes | 854 | 5,171 | 33,976 | 8,565,296 | 74.3% |
| Added bytes | 291 | 3,570 | 26,122 | 7,728,941 | 73.3% |

That skew is why the simulation supports empirical per-file weights instead of
depending only on a lognormal guess.

## Simulation

### Numeric question

As the source/object/diff stage becomes 1x-20x faster, how much does total scan
wall time improve under finite producer and detector concurrency, backpressure,
and skewed file sizes? Where does the curve flatten? Separately, what is the
conditional Amdahl bound for accelerating only the storage fraction of the source
stage?

### Model

The runnable model is
[`contrib/fastgit/simulate_git_engine_ceiling.py`](../contrib/fastgit/simulate_git_engine_ceiling.py).
The exact historical per-file trace used for the reported curve is committed as
[`contrib/fastgit/betterleaks_file_work_20260710.txt`](../contrib/fastgit/betterleaks_file_work_20260710.txt),
including hashes of the source patch and data rows.

It models 3,461 finite file jobs flowing through:

```text
10 producer slots
  -> bounded admission/backpressure
  -> 40 detector slots
  -> pipeline drain
```

Producer service is weighted by a file's patch-section bytes. Detector service
is weighted by its added bytes. Each isolated finite stage is calibrated to its
observed/inferred completion time before the stages are composed, avoiding the
mistake of multiplying stage elapsed time by slot count and then adding a second
straggler penalty.

The 3.36-second detector-stage input is inferred by calibrating the composed
empirical trace to the 3.9575-second full scan; it is not an independent
detector-only measurement. The defensible uncertainty interval is approximately
3.305 seconds (fully serialized residual) to 3.958 seconds (source fully hidden),
and that interval should be swept before using the model for another workload.

The model deliberately excludes:

- shared-core contention and the CPU released by removing Git processes;
- detailed pack cache hits, delta-chain dependencies, and I/O queue depth;
- per-rule detector cost and metadata-dependent filtering;
- actual 16-commit run assignment/process affinity and each worker's buffered
  64-file handoff; the model list-schedules per-file work instead;
- commit-run locality and the current repeated Git-process cache resets;
- cold-cache and larger-than-memory behavior;
- implementation/FFI overhead of any candidate engine.

Those are prototype parameters, not facts the simulation can discover.

### Run and regenerate empirical weights

The checked-in empirical trace is the default:

```sh
python3 contrib/fastgit/simulate_git_engine_ceiling.py
```

Trace provenance is intentionally explicit about what was and was not
retained. The archived patch SHA-256 is
`f3309c9ae0c9c807626d39d276f3f6068282d3a86707570a7f8bfd4fbeea9575`;
its first emitted commit is `9d3938f6d5b36e3a2c3424e4cf84af590856722b`, and the row hash is shown below.
The exact `--all` ref snapshot and capture-time Git version were not recorded,
so these weights are immutable inputs but not a fully regenerable repository
snapshot. The 0.652-second producer anchor came from samples
0.59/0.51/0.57/0.69/0.74/0.81 seconds; the raw trials behind the 3.9575-second
full-scan mean were not retained in this artifact. Re-capture all anchors for a
current performance claim.

Use `--synthetic` for the distribution-sensitivity fallback. To regenerate a
new trace for a new repository state, first remeasure the source-only and full
scan anchors; the 2026-07-10 timing constants must not be silently applied to a
different history.

```sh
git log -p -U0 --full-history --all --diff-filter=tuxdb \
  > /tmp/betterleaks-self.patch

LC_ALL=C awk '
function flush() {
  if (infile) {
    print total, added
    total=0; added=0; infile=0
  }
}
/^diff --git / { flush(); infile=1 }
infile {
  n=length($0)+1
  total+=n
  if (substr($0,1,1)=="+" && substr($0,1,3)!="+++") added+=n-1
}
END { flush() }
' /tmp/betterleaks-self.patch > /tmp/betterleaks-file-work.txt

shasum -a 256 /tmp/betterleaks-file-work.txt
# Expected for the archived 2026-07-10 rows:
# 25a1ee97655ef110fc7c33356a6ba887850673f8bf1ad137948915c52919bbd1
```

### Results

Conditional optimistic bounds, treating all 0.652 seconds as critical-path
work and assuming the candidate does not also relieve detector CPU contention:

| Engine speedup | Maximum E2E reduction |
| ---: | ---: |
| 1.25x | 3.30% |
| 1.36x | 4.36% |
| 1.5x | 5.49% |
| 2x | 8.24% |
| 2.58x | 10.09% |
| 3x | 10.98% |
| 4x | 12.36% |
| 6x | 13.73% |
| infinite | 16.48% |

The 1.36x and 2.58x points are useful sensitivity points from a published gix
clone/index experiment, not predictions for historical diff generation.

Empirical finite-pipeline wall-speedup:

| Workload balance | 1.25x engine | 2x | 4x | 10x | effectively infinite |
| --- | ---: | ---: | ---: | ---: | ---: |
| Fitted historical balance | 1.033x | 1.085x | 1.131x | 1.159x | 1.178x |
| Source and detector balanced | 1.111x | 1.333x | 1.599x | 1.810x | 1.970x |
| Source 2x detector | 1.154x | 1.500x | 1.999x | 2.497x | 2.952x |
| Source 4x detector | 1.190x | 1.666x | 2.499x | 3.569x | 4.910x |

The model reproduces the measured current wall time at about 3.96 seconds. The
important crossover is workload balance: spectacular source-engine work is
nearly irrelevant when detection dominates, but becomes close to linear on a
source-bound or cold/large repository.

For the adjacent fused-cache idea, an intentionally optimistic sensitivity
treats the repeated-byte fraction as an equal removable fraction of the whole
detector stage:

| Detector work reused | Current engine | 2x engine | 4x engine | effectively infinite engine |
| ---: | ---: | ---: | ---: | ---: |
| 0% | 1.000x | 1.085x | 1.131x | 1.178x |
| 25% | 1.265x | 1.407x | 1.487x | 1.570x |
| 47.9% | 1.671x | 1.932x | 2.089x | 2.260x |

The 47.9% row is deliberately an upper-envelope experiment, not a forecast.
It assumes detector work is byte-linear, every repeated byte is reusable, and
candidate-graph lookup plus occurrence-specific replay are free. Its purpose
is to show why a correctly separated detector-candidate cache deserves a real
prototype even though pure source-stage work has a modest ceiling.

Conditional SPDK-only bounds inside the current source stage:

| Assumed storage share of source | 1.02x storage | 1.096x storage |
| ---: | ---: | ---: |
| 10% | 0.032% E2E | 0.144% E2E |
| 30% | 0.097% E2E | 0.432% E2E |
| 60% | 0.193% E2E | 0.863% E2E |

The 1.02x and 1.096x factors are calculated from Table 12 on page 24 of the
[SPDK 24.05 report](https://review.spdk.io/download/performance-reports/SPDK_nvme_bdev_perf_report_2405.pdf):
4 KiB random reads, one NVMe SSD, one CPU core, and one fio job. At QD1 the
table reports 13,572 versus 13,336 IOPS; at QD128 it reports 1,171,181 versus
1,068,420 IOPS for SPDK versus polling io_uring. They are configuration-specific
sensitivity inputs, not general storage predictions. Git's dependent delta
reads do not automatically create QD128.

### Validation

The script checks:

- one job completes in producer time plus detector time;
- a producer-only workload scales exactly with engine speedup;
- a detector-only workload is invariant to engine speedup;
- a uniform finite pipeline lies between the slower-stage bound and serialized
  sum;
- empirical stages are independently normalized before composition.

The Amdahl calculation is retained alongside the simulation so a
plausible-looking stochastic result cannot exceed the stage-preserving bound
without explicitly claiming shared-resource relief or cross-stage fusion.

## What is available off the shelf

### Git/object/diff engines

| Candidate | What it supplies | Integration | Main risk | Rank |
| --- | --- | --- | --- | ---: |
| Stock `git diff-tree --stdin` worker | Canonical refs, packs, deltas, attributes, xdiff, binary and rename behavior; persistent process/cache lifetime | Stream commit batches and invalid-input flushed boundary sentinels | Still emits text; pipes must be drained concurrently | 1 |
| Persistent custom Git builtin/daemon | Same canonical engine plus scanner-native records | Length-prefixed native records from a long-lived process | Git internal API and maintained patch | 2 |
| libgit2 | Linkable C Git core, revwalk, ODB, tree/blob diff callbacks, custom ODB | One coarse C call per batch via shim/git2go | Exact Git parity and cgo deployment | 3 |
| gix/gitoxide | Rust object/pack/MIDX/commit-graph and tree/blob diff components | Rust helper or staticlib with coarse batch ABI | Rename/diff behavior differs; parity unproven | 4 |
| go-git v6 | Pure-Go object model, pack decode, tree diff, experimental mmap scanner | Direct Go prototype | Alpha; MIDX gaps; whole-blob string diff path | 5 |
| JGit | Mature Java object and diff engine, DFS object database | JVM sidecar/JNI/native image | Runtime and integration weight; MIDX/SHA-256 gaps | reference |
| Dulwich | Readable Python object/diff implementation | Python sidecar or algorithm source | Orchestration remains Python; unlikely speed winner | reference |

#### Stock persistent `git diff-tree --stdin`

This is the smallest useful prototype and was not in the initial shortlist.
`git diff-tree --stdin` accepts revisions incrementally and emits output before
EOF, so each existing logical worker can keep one stock Git process alive
while retaining the shared dynamic batch queue. On Git 2.51, this full-history
comparison over the current roughly 46 MB stream was byte-identical:

```sh
git rev-list --all |
  git diff-tree --stdin --root -p -U0 -M --diff-filter=tuxdb --pretty=medium

git rev-list --all |
  git log --no-walk --stdin -p -U0 -M --diff-filter=tuxdb --pretty=medium
```

`-M` is essential: the comparable `git log` path enables rename detection,
while the first `diff-tree` trial without it did not. The original plan to use
an identical-tree pair as a boundary was wrong: Git prints that valid request
but does not explicitly flush the empty diff while stdin remains open. The
corrected worker sends a high-entropy line that cannot begin with a valid OID;
`diff-tree`'s invalid-input branch echoes the complete line and flushes stdout.

Request writes and stdout draining must be concurrent. A full GitLab run of a
serial write-then-read implementation deadlocked when Git's stdout pipe and
the request's stdin pipe both filled. With concurrent 1 MiB draining, ten
persistent workers took 104.30 seconds versus 113.12 seconds for equivalently
drained short-lived workers. Differential tests still cover empty commits,
merges, mode-only changes, the final batch, marker-like content, and blocked
pipes. This measures persistent process/cache lifetime with no Git patch and no
new diff implementation.

#### Persistent Git native-record daemon

This preserves the strongest semantic oracle: Git itself. The existing
parallel implementation creates multiple Git processes over successive commit
runs, so each process loses its decoded-delta cache. Existing locality
experiments showed an 8-18% Git-CPU signal, distinct from text-pipe savings.

Git's [diff API](https://git-scm.com/docs/api-diff) already supports tree pairs
and output callbacks, but the fine-grained symbol emitter is internal. A small
Git patch would frame scanner-native records, keep one engine per logical
worker alive, and service successive commit batches.

This should follow the stock `diff-tree --stdin` arm. It further separates:

- process startup and cache-reset savings;
- text formatting and patch-pipe traffic (about 4.18 GB on GitLab FOSS);
- parser work;
- potential direct archive-blob access;

without simultaneously changing the diff oracle.

#### libgit2

[libgit2](https://github.com/libgit2/libgit2) is a production-used, linkable C
implementation with revision walking and [tree/blob diff APIs](https://libgit2.org/docs/reference/main/diff/index.html).
Its [`git_odb_backend`](https://libgit2.org/docs/reference/main/sys/odb_backend/git_odb_backend.html)
provides `read`, `read_header`, `readstream`, `exists`, prefix lookup, iteration,
and pack-ingestion hooks. That is the most mature seam for a later immutable
SPDK-backed decoded-object store.

The Go boundary should be coarse: pass a batch of commit OIDs to C, accumulate
native records in a C arena, and copy or transfer one batch back. Per-line cgo
callbacks repeat the FFI shape that already lost in the parser tournament.

#### gix/gitoxide

[gix](https://docs.rs/gix/latest/gix/) exposes commit-graph walks, MIDX and pack
access, tree diffs, blob conversion/binary handling, and line diffs. Its object
[`Find`](https://docs.rs/gix-object/latest/gix_object/trait.Find.html) trait is a
promising narrow storage boundary. Its high-level repository is more
filesystem-oriented, so a custom store likely composes lower-level crates.

The main caveat is explicit: gix's rename tracker documents behavior that
differs from Git. A gix arm must pass the existing generated-stream, flag,
date-format, corpus, and findings-digest differential suite before performance
matters. Published gitoxide clone results are evidence of engine potential, not
evidence for `git log -p` parity or speed.

#### go-git v6

The development snapshot of [go-git](https://github.com/go-git/go-git/tree/d60654d4f9983abdaf8cc9f7df4d98e5c3626ae4)
offers the cheapest in-language prototype and includes an experimental mmap
pack scanner. This is unreleased v6/alpha work, not a stable v6 claim. It is
useful for measuring mmap object access without SPDK. However, that snapshot's
compatibility matrix marks MIDX and cruft packs unsupported, and its patch path
reads complete blobs into Go strings and uses diff-match-patch rather than Git
xdiff. Split its object-read and blob-diff timers before rejecting the whole
engine.

### Userspace storage and filesystem options

| Candidate | Unmodified Git | Direct storage path | Assessment |
| --- | ---: | ---: | --- |
| Flat immutable ODB on ordinary files | No; embedded engine | No | Required representation baseline |
| xNVMe backend | No; embedded engine | Selectable POSIX/io_uring/SPDK | Best controlled backend tournament |
| SPDK ublk plus ext4/XFS | Yes | SPDK below kernel filesystem; buffered Git still copies | Cheapest unmodified-Git SPDK test |
| SPDK Blobstore immutable ODB | No | Yes | Good eventual substrate, not a filesystem |
| DAOS DFS/dfuse/libioil | Mostly | Userspace object store with compatibility/interception | Proof architecture exists; operationally heavy |
| PMDK/DAX filesystem | Yes | Load/store access through DAX, not SPDK DMA | Archived toolkit; historical floor where hardware exists |
| Linux FUSE-over-io_uring | Yes, through a FUSE implementation | Ring-based FUSE transport | Interface still developing; not all requests use the ring |
| Kioxia UFROP | RocksDB only | io_uring/libnvme NVMe character-device path | Tiny modern code template, not a Git filesystem |
| Historical SPDK BlobFS | No | Yes | Removed/insufficient semantics; do not adopt |
| SPDK fsdev/FUSE dispatcher | Potentially | Partial | Deprecated/replacement underway |
| NVFUSE/Assise/OpenMPDK uNVMe | Varies | Yes | Research/source mining only; dormant or narrow |

#### Do not build a POSIX filesystem first

[SPDK Blobstore](https://spdk.io/doc/blob.html) is deliberately non-POSIX and
stores large opaque blobs. Historical BlobFS had a flat namespace, append-only
writes, non-atomic rename, O(n) lookup, and a debug-oriented FUSE adapter; the
component was later removed. Current fsdev/FUSE APIs are themselves in a
deprecation transition. A Git-compatible filesystem would add directories,
permissions, links, mmap, atomic rename, locks, durability, recovery, and cache
coherence that a read-only scanner object store does not need.

A narrow immutable layout is more credible:

```text
snapshot/root generation
  -> OID index segments
  -> immutable object payload segments
```

The normal repository remains canonical. Import reachable decoded objects,
seal segments, and atomically publish one generation. This avoids implementing
Git's write/repack semantics in the first prototype.

#### xNVMe before hard-wiring SPDK

[xNVMe](https://xnvme.io/) offers one API over POSIX, libaio/io_uring,
io_uring command passthrough, and SPDK. Keeping the object layout and Git engine
fixed while changing only the backend gives the essential attribution:

```text
flat ODB + pread
flat ODB + io_uring
flat ODB + io_uring_cmd
flat ODB + SPDK
```

If the ordinary-file pre-inflated ODB loses, SPDK is unlikely to rescue it.

#### SPDK ublk

SPDK's [ublk target](https://spdk.io/doc/ublk.html) can expose an SPDK bdev as a
normal Linux block device, allowing ext4/XFS and unmodified Git. Linux ublk's
zero-copy mode, however, requires cooperating direct-I/O clients and registered
buffers; ordinary buffered Git does not qualify. This arm is still valuable
because it is complete and falsifiable, but its expected gain is low.

#### DAOS as architectural proof

[DAOS DFS/dfuse/libioil](https://docs.daos.io/master/user/filesystem/) shows
that a userspace object store, compatibility filesystem, and interception layer
can coexist. It is too heavy as the first betterleaks prototype, but it prevents
us from incorrectly declaring the architecture impossible.

Other projects are more useful as source material than dependencies:

- [Linux FUSE-over-io_uring](https://www.kernel.org/doc/html/latest/filesystems/fuse/fuse-io-uring.html)
  is a current transport direction, but its documentation still describes an
  interface under development and not all request types use the ring.
- [Kioxia UFROP](https://github.com/kioxia-jp/ufrop) is a compact current
  RocksDB filesystem plugin using io_uring/libnvme against an NVMe character
  device. Its narrow implementation is a useful backend template, not a
  general filesystem to adopt.
- [NVFUSE](https://github.com/nvfuse/nvfuse),
  [Assise](https://github.com/ut-osa/assise), and
  [OpenMPDK uNVMe](https://github.com/OpenMPDK/uNVMe) demonstrate embedded or
  preload-based userspace filesystem techniques, but their public artifacts
  are dormant, research-oriented, or tied to old SPDK generations.
- [PMDK/DAX](https://pmem.io/pmdk/) can provide a historical load/store
  comparison on suitable persistent-memory hardware, but the main repository
  is now archived and it tests a different mechanism from NVMe DMA/kernel
  bypass.

## Minimal-port opportunities

- Use a Git builtin/daemon rather than trying to link Git's internal
  `libgit.a` as a stable ABI.
- Use libgit2 through a small batch C shim rather than porting its ODB, revwalk,
  and diff layers into Go.
- Use gix as a Rust helper/staticlib. Do not port the complete Rust engine.
- Evaluate `imara-diff` behind a coarse blob-pair/batch boundary if gix object
  access wins but full gix semantics do not.
- Use go-git v6 to establish the lowest-friction mmap/object baseline.
- Mine JGit/Dulwich for corner cases and object-store architecture; do not port
  their whole runtimes as a first performance experiment.

## Recommended tournament

### Phase 0: preserve the current oracle

Record scanner-native output and findings digests on the existing synthetic,
fuzz, flag/date-format, and multi-repository corpus. Add counters for ref walk,
OID lookup, compressed bytes, decoded bytes, delta depth, tree diff, blob diff,
output bytes, cache hits, and process/cache resets.

### Phase 1: attack source compute and cache lifetime

1. Stock Git text stream.
2. Existing optimized fastgit text stream with short-lived batch processes.
3. Persistent stock `git diff-tree --stdin` per worker.
4. Persistent Git native-record daemon.
5. libgit2 batch shim on normal files/mmap.
6. gix Rust batch helper.
7. go-git v6 mmap baseline.

Measure warm and natural-cold runs on betterleaks plus a repository whose packs
exceed available page cache. An engine is not accepted until scanner-native
records and findings digests match.

### Phase 2: separate representation from storage

1. Build a read-only pre-inflated OID-to-segment store on ordinary files.
2. Run the winning Git engine against it.
3. Measure storage amplification, decoded-object cache behavior, and cold
   performance.
4. Add a fully materialized native-record cache keyed by a snapshot of scanned
   refs and relevant Git configuration.

### Phase 3: backend tournament

Keep the engine and segment format fixed, then compare pread, io_uring,
io_uring_cmd, and SPDK through xNVMe. Separately run unmodified Git on SPDK ublk
plus ext4/XFS.

Advance to Blobstore only if cold traces show a material storage share and
enough independent misses to generate useful queue depth.

### Phase 4: fused content cache

Cache a complete content-derived candidate graph, not final findings. A safe
key must include at least the rule/config graph digest, detector/code version,
regex engine, decode settings/depth, and raw-hunk hash. The value must retain
rule IDs, match spans, extraction/decode provenance, and any required-rule or
specificity relationships needed to reproduce candidate suppression. Then
apply path, commit, author, line, allowlist, fingerprint, arbitrary expression
filters, and all other metadata-dependent logic independently for each
occurrence.

The current detector interleaves some path and attribute checks with candidate
generation, so this seam does not exist cleanly yet. The cache experiment first
needs a differential refactor that separates content-derived candidate
discovery from occurrence-dependent evaluation. That refactor may do extra
first-occurrence regex work and must be benchmarked on both unique-heavy and
duplicate-heavy histories.

The 47.9% duplicate-byte figure is an admission signal, not a promised speedup.
The prototype must separate byte-proportional rule cost from fixed per-fragment
and metadata costs.

## Risk register

| Risk | Impact | Mitigation |
| --- | --- | --- |
| Git semantic drift | Missed or shifted scanner input | Differential native-record and findings corpus; keep Git daemon oracle |
| Root/merge/full-history mismatch | Coverage loss | Explicit graph fixtures and parity against current `git log` |
| Rename/binary/attributes mismatch | Changed file identity or content | Candidate-specific parity tests; treat gix/go-git defaults as untrusted |
| Repack/GC race | Missing or corrupt object view | Immutable snapshot generation; pin refs and object generation |
| Alternates/promisor/SHA-256/MIDX gaps | Unsupported real repositories | Capability probe and fallback to stock Git |
| Fine-grained FFI | Transition overhead and unsafe ownership | Coarse batch ABI with owned, length-prefixed records |
| DMA lifecycle/IOMMU error | Corruption or host-memory exposure | VFIO/IOMMU, registered arena validation, drain before teardown |
| Polling steals detector cores | Net slowdown despite faster I/O | Pin/sweep polling cores and report whole-scan CPU/wall time |
| Storage amplification | Decoded or materialized stores may greatly expand the working set | Measure imported decoded bytes separately; budget/evict and find the cold crossover |
| Candidate-cache semantic drift | Wrong suppression, decode, path, or expression behavior | Key by config/code/regex/decode identity plus raw content; cache candidate graphs only; replay all metadata-dependent logic |
| Cache invalidation error | Stale scan results | Key materialized source records by refs, config, engine version, attributes, and rule-set hash |
| Portability | Linux-only fast path fragments product | Portable stock-Git fallback; helper process boundary |
| Maintenance burden | Forked Git/SPDK drift | Keep patches narrow and backends replaceable |

## Evidence gaps

- No candidate has a public apples-to-apples benchmark for full-history
  `git log -p -U0` plus betterleaks' output contract.
- Clone/index benchmarks do not predict tree/blob diff and hunk generation.
- Warm M1 results do not predict cold Linux NVMe or larger-than-memory packs.
- CPU-profile percentages are not wall-time fractions, especially across child
  Git processes.
- Patch bytes proxy diff work poorly for adversarial Myers/Histogram cases.
- Added bytes proxy detector cost imperfectly because rule and metadata costs
  vary.
- SPDK benefit depends on queue depth, polling cores, NUMA, IOMMU, and cache
  state; the headline IOPS/core comparison is not an application speedup.

## Primary references

- [Git diff API](https://git-scm.com/docs/api-diff)
- [Git `diff-tree --stdin`](https://git-scm.com/docs/git-diff-tree)
- [Git pack format](https://git-scm.com/docs/gitformat-pack)
- [libgit2](https://github.com/libgit2/libgit2)
- [libgit2 custom ODB](https://libgit2.org/docs/reference/main/sys/odb_backend/git_odb_backend.html)
- [libgit2 diff API](https://libgit2.org/docs/reference/main/diff/index.html)
- [gix](https://docs.rs/gix/latest/gix/)
- [gix object `Find`](https://docs.rs/gix-object/latest/gix_object/trait.Find.html)
- [gitoxide pack-resolution experiment](https://github.com/Byron/gitoxide/discussions/579)
- [go-git](https://github.com/go-git/go-git)
- [SPDK Blobstore](https://spdk.io/doc/blob.html)
- [SPDK ublk](https://spdk.io/doc/ublk.html)
- [Linux ublk](https://docs.kernel.org/block/ublk.html)
- [SPDK deprecations](https://spdk.io/doc/deprecation.html)
- [SPDK 24.05 NVMe bdev performance report](https://review.spdk.io/download/performance-reports/SPDK_nvme_bdev_perf_report_2405.pdf)
- [xNVMe](https://xnvme.io/)
- [DAOS filesystem](https://docs.daos.io/master/user/filesystem/)
- [PMDK/DAX](https://pmem.io/pmdk/)
- [Linux FUSE-over-io_uring](https://www.kernel.org/doc/html/latest/filesystems/fuse/fuse-io-uring.html)
- [Kioxia UFROP](https://github.com/kioxia-jp/ufrop)
