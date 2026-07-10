# GitLab corrected-profile rotated full-history series

All nine rows used the frozen 197,173-commit GitLab snapshot, ten workers,
production batch sizing, and the same effective Git subprocess profile for the
stock reference and patched custom Git. The environment isolates global/system Git
configuration, disables replace objects and terminal prompting, sets
`core.deltaBaseCacheLimit=128m`, sets `MALLOC_ARENA_MAX=2` because it was unset,
and applies no zlib preload because `BETTERLEAKS_GIT_ZLIB` was unset.
`GIT_DIFF_OPTS` and inherited `GIT_CONFIG_*` injection were confirmed unset in
the unchanged parent environment immediately after the runs, not captured by
the wrapper at launch. The harness now removes them and tests hostile values.

The three-round rotated order was:

1. reference, custom default, custom `64m/512m` plus recycle-after-one;
2. custom default, bounded/recycled custom, reference;
3. bounded/recycled custom, reference, custom default.

Every row passed the authoritative snapshot digest, record digest, count, and
canonical-byte gates and reported zero helper page-ins. There was no known
overlapping engine, comparator, Cargo build, or repository scan. Each row is
marked `admissible_same_effective_profile_reconstructed_env` and embeds the
reconstructed Git profile plus post-run executable/source provenance.

| Configuration | Wall median, range (s) | Scan median, range (s) | Aggregate helper RSS median, range (GiB) |
|---|---:|---:|---:|
| Reference | 125.337, 124.349–145.309 | 122.940, 122.360–142.608 | 9.347, 8.757–9.656 |
| Custom default | 111.432, 108.131–112.648 | 108.768, 105.348–109.657 | 19.340, 19.253–19.365 |
| Custom `64m/512m`, recycle 1 | 121.471, 116.070–147.430 | 118.313, 113.637–145.156 | 6.770, 6.725–6.842 |

Three repetitions and visible run-order drift are insufficient for a formal
significance claim. These are warm-cache CPU/helper-RSS results only; zero sampled
page-ins provides no cold-cache, larger-than-RAM, NVMe, `io_uring`, xNVMe, or
SPDK evidence.

RSS covers matching engine-helper descendants only. It excludes the Go
benchmark coordinator/root process and can miss peaks between samples.
