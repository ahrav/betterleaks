# GitLab 20k sampled matrix

All rows used ten workers, production batch sizing, the frozen GitLab ref and
revision digests, and the deterministic 20,000-commit contiguous-run sample.
Every row produced 2,773,077 records, 612,112,014 canonical bytes, and digest
`d06726bb2e56dc7d11dda9689c4d79fbe7dc5eb6f84797f3385c82f72a6462ff`.

This matrix predates Git-environment parity in the harness. Its custom helpers
inherited Git's 96 MiB delta-base cache instead of the 128 MiB production
profile. All rows are marked
`profile_confounded_delta_cache_96m_vs_128m`: output identity remains valid,
but timing and helper RSS are not release-profile measurements.

The common command was:

```sh
python3 contrib/gitengine/custom-git/measure_bench.py --interval 0.5 -- \
  go run ./contrib/gitengine/bench -engine custom-git \
    -helper /Users/ahrav/Projects/benchmarks/git-source/git \
    -repo /Users/ahrav/Projects/benchmarks/gitlab-foss \
    -workers 10 -sample-commits 20000 -timeout 30m
```

Each row's `packed_git_window`, `packed_git_limit`, and `recycle_batches`
fields are the additional command arguments for that variant. Page-ins were
zero throughout, so these are warm-cache mmap/CPU/helper-RSS measurements.
