# Custom Git engine performance tournament — 2026-07-11

## Outcome

The next meaningful code improvement is a two-part path:

1. retain one 64 KiB buffered reader for the helper's entire protocol stream;
2. build the custom Git builtin with the existing `memchr` + XXH3 xdiff line-hash patch and the pinned xxHash v0.8.2 header.

At ten workers on the current GitLab corpus, three full warm runs reduced median
wall time from 109.434 s to 99.241 s (-9.31%), scan time from 106.734 s to
96.506 s (-9.58%), and exact aggregate helper CPU from 1007.566 s to 924.001 s
(-8.29%). Median aggregate helper RSS moved from 15.629 GiB to 15.682 GiB
(+0.34%).

Worker count is a separate latency/RSS control, not part of that code delta. On
this 64-core host the fastest tested full run used 48 workers: 32.473 s wall,
28.053 s scan, 1162.613 s helper CPU, and 55.221 GiB aggregate helper RSS.
Sixty-four workers regressed to 35.000 s and 67.932 GiB, establishing the tested
latency saturation point. No worker-count default was changed because the safe
choice depends on memory and CPU limits outside this host.

## Scope and measurement contract

The benchmark's `scan_ms` covers custom helper generation, binary protocol
decoding, canonical-record encoding, and digest consumption after workers are
ready. It excludes preflight, worker startup/HELO, and worker shutdown, and it
does not run the finding detector. The outer wall observation includes those
lifecycle phases, but this archived sampler started its clock just after
`Popen` and detected exit on a 100 ms interval; it can therefore omit launch
cost and include up to one interval of exit-detection delay. The live
`betterleaks git` source still uses the text `git log -p` path; the native
engine remains a benchmark/compare/test arm. These numbers establish canonical-
record pipeline performance, not production detector latency.

Every performance row below used:

- corpus `/local/home/ahrav/scratch/gitlab-foss` at `754df18208dd903e1b05c7a4343da272079bea3d`;
- 197,178 commits and 6,270 refs;
- one 1.82 GiB pack on a 123 GiB host;
- warm state, `MALLOC_ARENA_MAX=2`, no helper recycling, no pack-window override unless named;
- Git `c44beea485f0f2feaf460e2ac87fdd5608d63cf0` plus the custom builtin patch;
- a 100 ms `/proc` sampler for helper RSS, faults, I/O, and syscall counts;
- exact user/system CPU from every reaped helper `ProcessState`;
- ref digest `deed23c8c84f6c1ae6037989040f5b9b7f3ebbe7a395f896ec86ee565436783c`;
- revision digest `e2e8f26e5fed457385c0b782346d05daee3bd31dbf4ea4d0eba13aeffb4e6c59`.

The authoritative canonical result is 24,253,390 records, including 197,178
commit, 2,820,879 file, and 21,235,333 hunk records; 5,495,475,641 canonical
bytes; digest `49139d357cb54d84b515204f23ef221be21fe1210287101cda1002cda1393b93`.

## Honest baseline

| Configuration | Runs | Wall s, median (range) | Scan s, median (range) | Helper CPU s, median (range) | Helper RSS GiB, median (range) |
|---|---:|---:|---:|---:|---:|
| Stock typed reference | 1 | 121.424 | 119.350 | sampled only | 7.813 |
| Custom builtin, O2, unbuffered protocol, 10 workers | 3 | 109.434 (108.864–110.209) | 106.734 (106.200–107.386) | 1007.566 (1006.437–1031.185) | 15.629 (15.537–15.698) |
| Kept O2 XXH3 + 64 KiB protocol buffer, 10 workers | 3 | 99.241 (98.929–99.313) | 96.506 (96.289–96.606) | 924.001 (922.205–926.013) | 15.682 (15.678–15.783) |

The stock reference CPU number is not reported because its short-lived child
processes were sampled rather than reaped by the benchmark. The custom helper
CPU figures are exact. RSS is the largest sampled sum and can miss a peak
between samples. The raw artifact's `process_tree_*_syscalls` fields are not
used: Linux folds a waited child's I/O counters into its parent, so summing
both levels double-counted descendant I/O. The corrected harness reports the
sampled benchmark-root I/O counter instead. This does not affect wall, scan,
exact helper CPU, helper RSS, fault, or correctness fields.

## Profile before changes

The pre-change Go CPU profile on 1,000 commits attributed 41.96% flat CPU to
`internal/runtime/syscall.Syscall6`; `readBatch` was 35.71% cumulative. The
unbuffered reader issued `io.ReadFull` directly for each frame length and
payload. Heap allocation was also visible (`CanonicalRecord`, `ReadFrame`, and
decoder byte copies), but the CPU profile and direct read structure made
syscall amortization the first protocol candidate.

The pre-change helper gprof run on 5,000 commits attributed 29.41% to
`xdl_prepare_ctx`, 28.98% to `xdl_hash_record`, and 11.05% to `xdl_recs_cmp`.
`patch_delta` and object-header decoding were about 2% each; rename detection
and protocol payload serialization were below 1%. Final AArch64 disassembly
confirmed `xdl_hash_record` was a scalar byte loop with a carried DJB2
dependency (`ldrb`, multiply-by-33 via add/shift, xor, branch).

After the xdiff change, the same helper profile shape put `xdl_hash_record` at
9.38%; the long XXH3 path was only 0.18%. The post-change Go profile put
`readBatch` at 10.42% cumulative. This is why no handwritten SIMD, SWAR,
branchless hash, cache blocking, cache maintenance, or huge-page change was
advanced.

Hardware PMU collection was unavailable (`perf_event_paranoid=2`, no usable
`perf` binary), so no top-down slot, cache, branch, or TLB claim is made.

## Tournament log

Every successful row below matched the expected canonical digest and counts.
Sample rows use the same current 20,000-commit selection: 3,297,457 records,
715,342,799 bytes, digest
`20a098234cb93c51661edf1a8e917740c3516d2bd79cabab3278a37723e710aa`.

| Candidate | Hypothesis and isolated implementation | Result | Verdict |
|---|---|---|---|
| Persistent buffered protocol reader | Reuse one Go `bufio.Reader` across HELO and all batches instead of issuing two direct reads per frame. | Two full 10-worker runs were 106.524–107.524 s wall versus paired baselines 108.864–110.209 s; the post-change Go profile reduced `readBatch` from 35.71% to 10.42% cumulative. | Won. Prefetch-ownership regression test added. |
| 64 KiB reader | Increase the default 4 KiB buffer only. | Two clean 64 KiB 20k screens had 17.020–17.140 s scan and 109.935–110.153 s helper CPU; the clean 4 KiB screen had 17.281 s and 110.351 s. Outer wall also favored 64 KiB, but its 100 ms sampler granularity is material and there is no isolated repeated full pair. | Low-confidence small improvement; kept because the mechanism is measured and costs only about 60 KiB per helper. |
| 256 KiB reader | A larger buffer may amortize more pipe reads. | The 20k screen was 19.729 s wall, 17.283 s scan, and 110.400 s helper CPU, slower than the 64 KiB arms. | Lost; reverted. |
| Request-local `oidset` | Replace quadratic duplicate OID validation (about 240 M comparisons over the full corpus) with Git's pre-sized exact set. | 107.633 s full wall did not improve over the buffered reader alone; helper CPU moved only within noise. | Negative result; removed from kept source. Hardened cross-batch/non-adjacent duplicate tests remain in the delivered test patch. |
| `memchr` + XXH3 xdiff hash | Remove the profiled serial DJB2 dependency without changing diff semantics. | 20k wall 21.244→19.823 s, scan 18.756→17.320 s, CPU 119.904→110.933 s. Full matched-source wall 107.633→99.221 s and CPU 998.593→925.855 s. | Largest CPU win; kept as a pinned build layer. |
| O3 + LTO | Compiler optimization may improve the post-XXH3 helper. | Two 20k screens: 19.220–19.424 s wall and 108.020–108.324 s CPU versus O2/64 KiB 19.519–19.524 s and 109.935–110.153 s. | About 1% wall; not selected as the portable default because it is inside layout/toolchain sensitivity and adds build complexity. |
| Five workers | Fewer processes may reduce repeated Git work and memory. | 25.629 s wall, 106.787 s CPU, 5.893 GiB RSS on 20k. | Latency loss; useful memory-saving mode only. |
| 20 and 32 workers, 20k screen | More independent helpers may expose useful host parallelism. | 20 workers: 15.676 s and 15.469 GiB. 32 workers: 15.263 s and 20.697 GiB. | Advanced to full-corpus scaling. |
| Worker scaling, full corpus | Find the latency/RSS knee on the 64-core host. | 20/32/48/64 workers: wall 55.685/39.699/32.473/35.000 s; CPU 983.042/1041.403/1162.613/1286.949 s; RSS 27.339/40.132/55.221/67.932 GiB. | 48 is the tested latency optimum; 10 remains CPU/RSS-efficient. Host-specific, no default change. |
| Contiguous batches of 256 or 2,048 | Contiguous history may improve object locality. | 20k wall 27.834 and 28.246 s versus ~19.52 s for production's dealt 16-commit runs. CPU fell, but load imbalance dominated wall time. | Lost. Keep production dealing. |
| Recycle after each 256-commit batch | Bound retained object/allocator state. | 27.218 s and 4.370 GiB versus persistent batch-256 at 27.834 s and 6.305 GiB; single-run timing is inconclusive, and both were much slower than production batching. | Not selected for speed; memory trade remains available. |
| `packedGitWindowSize=64m`, `packedGitLimit=512m` | Bound pack mappings/cache residency. | 19.737 s, 114.615 s CPU, 3.811 GiB RSS versus O2/64 KiB ~19.52 s, ~110.0 s CPU, ~10 GiB RSS. | Speed loss, large RSS win; not the performance winner. |
| mmap/io_uring/xNVMe/SPDK | Storage redesign should proceed only when cold storage is material. | Primary full rows had zero storage-read bytes and zero major faults. Corpus pack is much smaller than RAM. | Rejected by precondition; no storage claim from warm data. |
| Hand SIMD/SWAR/assembly | Replace the remaining hash/search work with target-specific code. | Maintained glibc `memchr` already supplies NEON; 98.622% of sampled lines were at most 240 bytes, and post-change long XXH3 was 0.18% of gprof time. | Rejected by hotspot gate. Portable scalar fallback retained. |

The 20k sample and full-corpus rows are not mixed when claiming the primary
before/after result. The sample exists only to screen candidate direction; the
kept code path was repeated three times on the full corpus with the original
ten-worker/cache/process settings.

## Correctness and safety evidence

- Full stock-Git comparator: `compared=197178 mismatches=0`.
- One stock typed-reference full pass established the current digest; all
  serious full candidates were rejected at runtime unless ref, revision,
  record, file, hunk, byte, and canonical digests matched.
- Hostile inherited settings (`GIT_DIFF_OPTS`, diff algorithm, rename, context,
  inter-hunk context, and packed-Git injection) replayed 1,000 commits with the
  exact expected digest.
- `go vet` and ordinary/race tests passed for `internal/gitengine` and every
  `contrib/gitengine` package. Full `go test ./...` passed outside the sandbox;
  the first in-sandbox attempt was blocked only by loopback socket policy.
- Both delivered patches applied to a pristine pinned Git checkout, produced
  the complete 168-line protocol test ending in `test_done`, and passed the
  documented `make all` build.
- Git `t4218-betterleaks-engine.sh` passed all eight protocol scenarios,
  including SHA-256 OIDs, malformed frames, terminal errors, cross-batch state,
  and non-adjacent duplicates.
- Git `t4013-diff-various.sh` passed 228 tests; `t4202-log.sh` and `make
  check-docs` passed.
- The XXH3 cursor/range/guard-page harness passed 38,770,589 cases in both O2
  and ASan+UBSan builds using the retained `XXH_NO_STREAM` and AArch64 scalar
  macros. Leak detection was disabled for the sanitizer run because LSan
  cannot operate under this host's ptrace policy; address and UB checks ran.
- The full Git builtin passed `t4218` under ASan+UBSan. This host required a
  temporary loader directory providing the installed sanitizer SONAME links;
  the initial loader failure did not execute tests.
- A 38-command synthetic Git differential matrix passed across algorithms,
  whitespace modes, binary/rename/merge/blame/patch-id/function-context paths.
- 1,000 randomized repository differentials completed with zero failures.
- The Go change adds no unsafe code. The xdiff dependency retains a portable
  scalar implementation; AArch64 explicitly uses it for XXH3 while `memchr`
  supplies the maintained vector path.

## Rejected prior directions

The archived tournament already contains libgit2, gix, hybrid rename fallback,
process recycling, pack limits, parser SWAR/pre-index/manual-copy variants, and
zero-copy span prototypes. Those remain useful negative evidence, but their old
corpus snapshot and profiles were not reused as current performance data. The
live profiles did not produce a new premise strong enough to rerun the
canonically incomplete libgit2/gix engines during this tournament.

## Remaining high-potential experiments

1. Profile and isolate zlib-ng versus system zlib. Dynamic-library time is not
   attributed well by the current gprof data, so the old zlib result is still a
   hypothesis for this custom builtin.
2. Investigate post-XXH3 `xdl_prepare_ctx` allocation/classification work. Do
   not optimize `XXH3_hashLong`: it was only 0.18% in the post-change profile.
3. Build a memory-budgeted worker autotuner using CPU quota, available memory,
   expected per-helper residency, and a conservative cap; validate on more than
   this 64-core/123 GiB host.
4. Revisit frame/record allocation only with an ownership-safe design. The
   post-change Go profile still shows frame and canonical copies, but
   `readBatch` is now only 10.42% cumulative and earlier zero-copy variants
   regressed.
5. Evaluate PGO/BOLT after the source hotspot work. O3+LTO screened at only
   about 1% wall and is not enough evidence to change the portable build.
6. Run a genuinely cold corpus larger than RAM before testing mmap, io_uring,
   xNVMe, or SPDK. Preserve separate warm and cold result sets.
7. Wire the native engine into the production scanner behind an explicit
   fallback and repeat detector/finding-digest benchmarks before claiming a
   production Betterleaks speedup.

## Risks and maintenance cost

- XXH3 is a pinned build dependency with BSD-2-Clause notice obligations; the
  patched Git binary remains GPLv2. The builtin-only Git build is the portable
  correctness fallback if the optimization layer cannot be built.
- Changing xdiff's internal hash is semantically safe only while classifier
  equality checks remain intact. The differential, fuzz, sanitizer, and full
  comparator gates must remain release gates when rebasing Git or xxHash.
- The 64 KiB reader retains roughly 60 KiB more memory per live helper than the
  default reader. It must remain the same reader across handshake and batches;
  replacing it between frames can discard prefetched bytes.
- Worker-count speedups spend large amounts of RSS and extra CPU. Forty-eight
  workers are not an acceptable universal default and can cause OOM or noisy-
  neighbor harm under smaller limits.
- All storage observations here are warm-cache observations. The sampler's
  zero `read_bytes`/fault counts do not generalize to repositories larger than
  RAM or different filesystems.
- The benchmark host was not isolated and hardware PMU/frequency counters were
  unavailable. The primary code win is nevertheless larger than the observed
  full-run range and is backed by exact helper CPU plus the observed profile
  shift and direct-read removal; near-1% compiler-only candidates were not
  promoted.

Raw rows are in `gitlab-current-warm-tournament-2026-07-11.jsonl`; executable,
corpus, host, and hash identities are in
`gitlab-current-warm-tournament-2026-07-11.provenance.json`.
