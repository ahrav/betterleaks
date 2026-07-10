# Git engine tournament

This directory contains isolated implementations of the version-1 protocol in
[`internal/gitengine/PROTOCOL.md`](../../internal/gitengine/PROTOCOL.md):

- `libgit2-helper`: C helper pinned to the installed libgit2 API;
- `gix-helper`: Rust helper pinned by `Cargo.lock`;
- `bench`: the common commit scheduler, counter check, timer, and canonical
  order-independent record digest.

The custom Git implementation is maintained as a patch against upstream Git,
not as copied Git source. Its pinned base and generated patch live in the
`custom-git` directory once exported from the development checkout.

None of these helpers is wired into production `ParallelGit`. They first have
to match the stock-Git reference record multiset. Matching commit and file
counts is insufficient: hunk boundaries, positions, bytes, final-newline
state, mailmap output, modes, OIDs, paths, and binary classification all
participate in the digest.

Build each helper using its own README, then compare it with the reference:

```sh
go run ./contrib/gitengine/bench \
  -engine reference -repo /path/to/repo -workers 10

go run ./contrib/gitengine/bench \
  -engine libgit2 \
  -helper "$PWD/contrib/gitengine/libgit2-helper/betterleaks-libgit2-engine" \
  -repo /path/to/repo -workers 10

go run ./contrib/gitengine/bench \
  -engine gix \
  -helper "$PWD/contrib/gitengine/gix-helper/target/release/betterleaks-gix-helper" \
  -repo /path/to/repo -workers 10

# Explicit Git-assisted falsification mode; never report this as plain gix.
go run ./contrib/gitengine/bench \
  -engine gix-git-rename-fallback \
  -helper "$PWD/contrib/gitengine/gix-helper/target/release/betterleaks-gix-helper" \
  -repo /path/to/repo -workers 10

go run ./contrib/gitengine/bench \
  -engine custom-git -helper /path/to/patched/git \
  -repo /path/to/repo -workers 10
```

The frozen GitLab FOSS snapshot additionally requires:

```text
ref digest      d3a5a08289c2471fe300abcb50c58308fda1e43c78d2fd77fa667b608d3711d9
revision digest 16fd50d990ed2df94e6384a542c1aa2bfb2054fb83ca0aced917115a8f441eeb
```

Pass those through `-expected-ref-digest` and
`-expected-revision-digest`. A mismatch stops before helper preflight or timed
scanning, preventing ref movement from contaminating an interleaved series.
