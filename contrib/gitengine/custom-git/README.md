# Custom Git builtin experiment

This arm applies a source patch to upstream Git rather than copying Git's
GPLv2 source tree into Betterleaks. The patch is pinned to:

```text
c44beea485f0f2feaf460e2ac87fdd5608d63cf0
```

Apply `betterleaks-diff-engine.patch` to that commit, then build and run the
focused gates:

```sh
set -eu
git apply /path/to/betterleaks-diff-engine.patch
make DEVELOPER=1 -j8 all
make DEVELOPER=1 -C t T=t4218-betterleaks-engine.sh
make DEVELOPER=1 -C t T=t4013-diff-various.sh
make DEVELOPER=1 -C t T=t4202-log.sh
make DEVELOPER=1 check-docs
```

## Profiled performance build

Starting from an untouched checkout at the pinned Git commit, the kept
performance configuration also applies the repository's xdiff line-hash
patch. Fetch the released header by content hash; do not build against an
unpinned checkout:

```sh
set -eu
git apply /path/to/betterleaks/contrib/gitengine/custom-git/betterleaks-diff-engine.patch
curl --proto '=https' --tlsv1.2 -fL \
  https://raw.githubusercontent.com/Cyan4973/xxHash/v0.8.2/xxhash.h \
  -o xdiff/xxhash.h
echo 'be275e9db21a503c37f24683cdb4908f2370a3e35ab96e02c4ea73dc8e399c43  xdiff/xxhash.h' |
  sha256sum -c -
git apply /path/to/betterleaks/contrib/fastgit/xdiff-xxh3.patch
make -j8 NO_CURL=1 NO_GETTEXT=1 all
make NO_CURL=1 NO_GETTEXT=1 -C t T=t4218-betterleaks-engine.sh
make NO_CURL=1 NO_GETTEXT=1 -C t T=t4013-diff-various.sh
make NO_CURL=1 NO_GETTEXT=1 -C t T=t4202-log.sh
make NO_CURL=1 NO_GETTEXT=1 check-docs
```

The patch replaces xdiff's serial DJB2 line hash with `memchr` plus XXH3.
Record equality is still verified by xdiff, so the hash remains an internal
bucket key. On AArch64 the patch selects xxHash's portable scalar backend to
avoid its unaligned NEON loads under UBSan; `memchr` remains the maintained
vectorized newline scan. Other targets use xxHash's normal compile-time
selection, with its portable scalar implementation as the build fallback.
Distributing the header retains its BSD-2-Clause notice; distributing the
patched Git remains subject to Git's GPLv2 terms.

The native protocol consumer retains one 64 KiB buffered reader across the
HELO handshake and every batch. This is part of Betterleaks itself and does
not change the wire format; it does not imply that the native engine is wired
into the production scanner.

The patch adds `git betterleaks--diff-engine`, a persistent builtin that emits
the common native-record protocol. Use the resulting Git executable through
the benchmark driver:

```sh
go run ./contrib/gitengine/bench \
  -engine custom-git -helper /path/to/patched/git \
  -repo /path/to/repository -workers 10
```

## Correctness gate

Validate complete record parity before using a candidate timing. The
comparator retains per-commit count, byte, and additive-hash summaries during
the full pass, then retains detailed records only for mismatching commits:

```sh
go run ./contrib/gitengine/compare \
  -engine custom-git -helper /path/to/patched/git \
  -repo /path/to/repository -workers 10 -batch-commits 256
```

The first full GitLab comparison reported 1,691 commit-message whitespace
differences. That exact first-pass count is a contemporaneous observation; its
comparator transcript was not retained. Normalizing the builtin's
pretty-medium title and body removed the observed class of differences. The
subsequent retained comparison covered all 197,173 commits with zero
mismatching commits.

The frozen full-history benchmark requires this authoritative result:

| Field | Value |
|---|---:|
| Requested commits / commit records | 197,173 |
| File records | 2,791,965 |
| Hunk records | 20,803,689 |
| Total records | 23,792,827 |
| Canonical bytes | 5,417,932,516 |
| Canonical multiset digest | `15bf91a9c237467054c9e2f3e11f405109256231e5c7928d13621b2cdb7d6e72` |
| Ref snapshot digest | `d3a5a08289c2471fe300abcb50c58308fda1e43c78d2fd77fa667b608d3711d9` |
| Revision snapshot digest | `16fd50d990ed2df94e6384a542c1aa2bfb2054fb83ca0aced917115a8f441eeb` |

The live corpus advanced before the 2026-07-11 tournament. Its separate
authoritative identity is:

| Field | Value |
|---|---:|
| Requested commits / commit records | 197,178 |
| File records | 2,820,879 |
| Hunk records | 21,235,333 |
| Total records | 24,253,390 |
| Canonical bytes | 5,495,475,641 |
| Canonical multiset digest | `49139d357cb54d84b515204f23ef221be21fe1210287101cda1002cda1393b93` |
| Ref snapshot digest | `deed23c8c84f6c1ae6037989040f5b9b7f3ebbe7a395f896ec86ee565436783c` |
| Revision snapshot digest | `e2e8f26e5fed457385c0b782346d05daee3bd31dbf4ea4d0eba13aeffb4e6c59` |

Do not combine timing rows from the frozen and live corpus identities.

`contrib/gitengine/bench` rejects a run when an expected snapshot digest,
record digest, record count, or canonical byte count differs. Matching only
the total record count is insufficient.

## Warm-cache memory tournament

Persistent helpers retain allocator and Git object-store high-water memory.
The benchmark can restart each helper after a fixed number of completed
batches without changing the requested commit set or canonical record stream:

```sh
python3 contrib/gitengine/custom-git/measure_bench.py -- \
  go run ./contrib/gitengine/bench \
    -engine custom-git -helper /path/to/patched/git \
    -repo /path/to/repository -workers 10 -recycle-batches 1
```

The 20,000-commit tournament compared the default persistent process; packed
Git mmap window/limit pairs `16m/256m`, `64m/512m`, and `256m/1g`; helper
recycling after 1, 2, 4, or 8 batches; and `64m/512m` combined with recycling
after 1 or 4 batches. Every row used ten workers and production batch sizing
and produced 2,773,077 records, 612,112,014 canonical bytes, and digest
`d06726bb2e56dc7d11dda9689c4d79fbe7dc5eb6f84797f3385c82f72a6462ff`.
These single observations select configurations for full-history testing; they
do not establish a performance distribution or statistical significance.

The matrix predates the benchmark-environment parity fix. Its custom helpers
used Git's 96 MiB default delta-base cache while the production/reference path
used 128 MiB. Every row is therefore marked
`profile_confounded_delta_cache_96m_vs_128m`. Its digests and counts remain
valid correctness evidence, and same-profile comparisons remain exploratory,
but its timing and helper RSS are not release-profile measurements.

All tournament rows reported zero helper page-ins. The matrix therefore
measures warm-cache mmap, CPU, process-start, and memory effects only. Zero
page-ins does not prove that storage is immaterial under a cold cache, on a
repository larger than RAM, or on another operating system.

## Full-history warm results

The corrected-profile full-history set contains three rotated-order replicates
for the stock reference, default custom builtin, and `64m/512m` plus
recycle-after-one-batch configuration. Both Git-based arms used the same
effective profile: isolated global/system config, replace objects disabled,
terminal prompting disabled, a 128 MiB delta-base cache,
`MALLOC_ARENA_MAX=2` when unset, and no zlib preload. The parent environment was
reconstructed immediately after the runs and had `GIT_DIFF_OPTS` and inherited
`GIT_CONFIG_*` injection unset; the wrapper did not capture those absences at
process launch. The harness now removes them mechanically and hostile-env tests
lock that behavior. Medians and ranges below use wall time; scan-only time
excludes preflight and worker setup/teardown recorded by the outer wrapper.

| Configuration | Wall median, range (s) | Scan median, range (s) | Peak aggregate helper RSS median, range (GiB) |
|---|---:|---:|---:|
| Stock reference | 125.337, 124.349–145.309 | 122.940, 122.360–142.608 | 9.347, 8.757–9.656 |
| Custom default | 111.432, 108.131–112.648 | 108.768, 105.348–109.657 | 19.340, 19.253–19.365 |
| Custom `64m/512m`, recycle 1 | 121.471, 116.070–147.430 | 118.313, 113.637–145.156 | 6.770, 6.725–6.842 |

Three replicates are too few to claim a statistically significant speedup,
especially with the visible order-to-order drift. In these observations the
default builtin's wall-time range did not overlap the reference range and its
median was 11.1% lower, but its median aggregate helper RSS was 106.9% higher. The
bounded/recycled arm's wall-time range overlapped both other arms; its median
was 3.1% below reference with 27.6% lower helper RSS. Relative to custom default,
bounded/recycled was 9.0% slower and used 65.0% less aggregate helper RSS.

The earlier bounded-persistent `64m/512m` observations remain archived and
profile-confounded. They were not rerun because the same-profile exploratory
pair was dominated by recycle-after-one in both run orders. They are not part
of the corrected release-profile comparison.

`measure_bench.py` samples only matching descendant helper processes. Peak
aggregate helper RSS is the largest sampled sum of resident pages across helpers;
peak individual RSS is the largest sampled helper value. VSZ measures reserved
virtual address space, not resident physical memory, and summing VSZ across
processes does not measure memory pressure. The sampler can miss peaks between
samples and excludes the Go benchmark process.

## Artifact validity

Use only rows that satisfy all of these conditions:

- the command exits with status 0 and emits a non-null benchmark result;
- ref and revision snapshot digests match the frozen GitLab snapshot;
- record digest, counts, and canonical bytes match the authoritative values;
- the row's embedded validity is admissible.

All nine corrected-profile rows satisfy these conditions and are marked
`admissible_same_effective_profile_reconstructed_env`. Their content-addressed
closeout provenance records the reconstructed effective Git settings and
post-run SHA-256 identities of the benchmark and engine executables. The
benchmark executable was rebuilt after the series; its recorded hash matches
the post-run binary and is not contemporaneous proof of the run binary. The older
balanced artifact embeds its overlap quarantine and invalid-preflight status
directly in rows 1 and 9; its matching sidecar remains as audit history. All of
its custom performance rows are additionally marked with
the delta-cache profile confound. Rows marked quarantined, invalid, or
profile-confounded do not enter the corrected medians or ranges.

Raw artifacts:

- `contrib/gitengine/custom-git/results/gitlab-full-warm-corrected-profile-2026-07-10.jsonl`
- `contrib/gitengine/custom-git/results/gitlab-full-warm-corrected-profile-2026-07-10.notes.md`
- `contrib/gitengine/custom-git/results/gitlab-full-warm-corrected-profile-2026-07-10.provenance.json`
- `contrib/gitengine/custom-git/results/gitlab-full-warm-balanced-2026-07-10.jsonl`
- `contrib/gitengine/custom-git/results/gitlab-full-warm-balanced-2026-07-10.validity.jsonl`
- `contrib/gitengine/custom-git/results/gitlab-full-warm-balanced-2026-07-10.notes.md`
- `contrib/gitengine/custom-git/results/gitlab-sample20k-matrix-2026-07-10.jsonl`
- `contrib/gitengine/custom-git/results/gitlab-sample20k-matrix-2026-07-10.notes.md`
- `contrib/gitengine/custom-git/results/gitlab-current-warm-tournament-2026-07-11.jsonl`
- `contrib/gitengine/custom-git/results/gitlab-current-warm-tournament-2026-07-11.notes.md`
- `contrib/gitengine/custom-git/results/gitlab-current-warm-tournament-2026-07-11.provenance.json`

The 2026-07-11 notes and provenance retain the full comparator's terminal
result, `compared=197178 mismatches=0`, alongside the current corpus and binary
identities. Older artifacts retain benchmark output identity but not their
zero-mismatch comparator transcript.

`rss_probe.py` remains the lower-level single-helper tool for observing RSS at
each batch boundary while discarding record payloads.

The patch is an implementation experiment, not an upstream Git interface.
Distribution of patched Git source or binaries remains subject to Git's GPLv2
license and source-compliance requirements; the patch stays outside the
MIT-licensed Betterleaks binary.
