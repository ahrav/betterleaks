# Betterleaks gix engine candidate

This crate is the independent gitoxide arm of the Git-engine tournament. It is
a synchronous, persistent helper for the binary protocol in
`internal/gitengine/PROTOCOL.md`; it does not change or replace `ParallelGit`.

Build and run its correctness gates with:

```sh
cargo fmt --check
cargo clippy --all-targets -- -D warnings
cargo test
cargo build --release
BETTERLEAKS_GIX_HELPER="$PWD/target/release/betterleaks-gix-helper" \
  go test -tags=gitengine_gix . -run TestGixMatchesStockReference -count=1
```

The process accepts one repository path. It advertises protocol v1 with SHA-1
or SHA-256 OID width and the production A/M/R profile. All-status and copy
profiles are deliberately not advertised. Root commits diff against the empty
tree, one-parent commits diff against their parent, and merge commits emit
metadata without a patch.

The repository and a 128 MiB object cache live for the process lifetime. One
gix resource cache is reused across batches while its per-resource contents are
cleared between commits. Gix owns object lookup, commit/tree traversal,
attributes, binary classification, mailmap projection, and exact rename
detection. A safe Rust port of Git's 64-byte span-hash similarity metric handles
fuzzy renames. It retains four sources per destination in diff-queue encounter
order, applies Git's score/basename ordering with a stable sort, and greedily
uses each source and destination once. The final text hunk engine is libgit2's
xdiff through the safe `git2` API. It runs Myers with the indent heuristic over
raw ODB blobs.

The explicit `--ambiguous-rename-fallback` process flag enables a
falsification-only hybrid. Ambient environment variables cannot activate it.
When retained candidates contain a competing score/basename tie, the helper
asks stock Git for only the raw rename pairs, then continues to load objects
and emit hunks through the candidate engine. The bench and compare tools expose
this only as `gix-git-rename-fallback`; `gix` always means the independent
matcher. This mode is useful for proving that residual parity failures are
isolated to rename pairing, but it is not an independent engine result and adds
a Git subprocess on affected commits.

Before xdiff, the helper reproduces core Git's zero-context large-common-tail
reduction: it removes matching 1024-byte blocks from the end while retaining
through the next newline. This wrapper behavior is outside public libgit2 and
was necessary for exact hunk grouping. A local attribute workaround also
handles a gix 0.85.0 gap where the bare `diff` attribute does not force
NUL-containing content to text; the helper queries the same attribute stack
and sends those raw blobs through the same xdiff path.

Repository-local mailmaps are loaded once and applied to author identity.
Configured `diff.*.textconv` and per-driver `diff.*.algorithm` are clean
pre-HELO unsupported results because this candidate cannot promise canonical
output for them. A commit with a declared non-UTF-8 encoding is discovered only
when its batch is known, so it produces a terminal helper error before BGIN or
records; protocol v1 cannot turn that commit-local condition into a safe
repository-wide fallback. Submodule patch projection is also a terminal error
and invalidates the complete scan. External diff commands remain disabled,
matching `--no-ext-diff`.

Gix can expose the raw commit headers directly, so an otherwise valid commit
without an `author` header is represented with owned empty author fields and
both author-presence flags false, matching stock Git. Focused protocol and Go
interop tests cover that uncommon object shape.

## Tournament result

On the repository snapshot with revision digest
`b26345e77aac9715fedfe2fdc47071159905e539b093a39ced2af7505a235743`, the
four-worker full-history gate covered 595 commits and matched stock Git exactly:
19,018 records, 3,481 files, 14,942 hunks, 25,714,659 canonical bytes, and
canonical multiset digest
`19d5317b482fb6de07df0b5413ca73ef0381901adc2b5d20cba1bf53cea3df78`.
This repository is a correctness corpus, not the large-monorepo performance
corpus, so its timings are deliberately not presented as the tournament's
performance result.

The explored text engines did not collapse into one untested choice:

- Pure gix Myers plus slider heuristics preserved all file records but emitted
  44 fewer hunks on this snapshot.
- Gix storage plus public libgit2 xdiff, without the core-Git wrapper reduction,
  emitted seven extra hunks.
- Disabling the slider, Myers-minimal, disabling libgit2's indent heuristic,
  and coordinate-only hunk coalescing all failed the full oracle.
- Standalone LGPL xdiff from `threeway_merge` reproduced libgit2's seven-extra
  shape. It confirmed the missing behavior was outside xdiff, but its private
  FFI, higher Rust floor, and additional distribution obligations bought no
  semantic advantage, so it is not a dependency of the final candidate.
- Libgit2 xdiff plus the narrow common-tail reduction matched the complete
  canonical record multiset.

The GitLab FOSS ordering experiment did not stop at one tie-break. On the first
100 commits of the frozen large-repository corpus:

- The original all-candidates matcher with an artificial descending source
  tie-break had two mismatches.
- Preserving diff-queue encounter order, retaining four candidates per
  destination, and using a stable score/basename-only sort had six mismatches.
  It fixed the two residual commits from the artificial tie-break but exposed
  four other choices among templated feature-flag files.
- Expanding similarity scores from integer percentages to Git's 0..60000
  resolution produced the same six mismatches, ruling out score truncation.
- Git 2.51.0 itself uses a stable matrix sort, so libc `qsort` permutation is
  not an applicable explanation for this corpus.
- The opt-in ambiguous-tie stock-pair fallback matched all three targeted
  commits and all 100 commits. This localizes the remaining gap to candidate
  admission/source ordering around exact and basename pre-matches; it does not
  qualify as a standalone gix-engine win.

## Dependencies and licensing

`gix` 0.85.0 and `git2` 0.21.0 are exact-pinned because their tree, attribute,
rename, and diff behavior is part of the experiment. `Cargo.lock` currently
resolves `libgit2-sys` 0.18.5 with libgit2 1.9.4. The Rust `git2` wrapper is
MIT/Apache-2.0; libgit2 is GPL-2.0 with its project linking exception, and its
embedded xdiff sources retain LGPL-2.1-or-later notices. A production release
must preserve the applicable third-party notices and source obligations. The
helper itself is MIT, matching Betterleaks. `tempfile` is test-only for isolated
live repositories.
