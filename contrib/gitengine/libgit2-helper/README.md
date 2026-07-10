# libgit2 Git engine experiment

This directory is the independent libgit2 arm of the Git-engine tournament.
It is not wired into production `ParallelGit`. The helper speaks
`internal/gitengine/PROTOCOL.md` version 1 over stdin/stdout and keeps one
repository plus mailmap open for its process lifetime.

Build and test with:

```sh
make
make test
make sanitize
```

The sanitizer target runs ASan plus UBSan on Linux and UBSan on macOS. The
macOS 26 Homebrew Clang ASan runtime repeatedly deadlocked in
`InitializeShadowMemory` before `main`, so that platform result is not counted
as code validation; Clang's static analyzer covers ownership there instead.
The 17 MiB single-line diff is covered by the release test but omitted from
instrumented runs because sanitizer xdiff is deliberately pathological.

Run it as:

```sh
./betterleaks-libgit2-engine --repo /path/to/repository
```

Optional `--all-statuses` and `--find-copies` flags select the corresponding
test profiles and are reflected in `HELO`.

The Homebrew libgit2 1.9.1 build used by this experiment supports SHA-1 only.
The helper refuses SHA-256 repositories rather than emitting a false 20-byte
capability. It also rejects `diff.*.textconv`, `diff.external`,
`GIT_EXTERNAL_DIFF`, and configured non-UTF-8 commit encoding before `HELO`:
libgit2 has no core-Git-compatible textconv or encoding-conversion path. These
cases emit the protocol's batch-zero kind-5 `ERRO` and exit cleanly, allowing
the Go process factory to return `gitengine.ErrUnsupported` before workers
open or records are emitted.

A historical commit may carry its own non-UTF-8 `encoding` header even when
the current repository config is UTF-8. Discovering that header requires the
requested object, after `HELO`; the helper emits terminal kind-2 `ERRO` before
the batch acknowledgement. That is deliberately not a safe fallback. Protocol
v1 has no commit-set preflight request with which to classify it as kind 5.

The helper pins zero context, the indentation heuristic, first-parent diffs,
no merge patches, 50% rename detection, and A/M/R production filtering.
libgit2 owns attributes and binary detection. Commit identities are resolved
through libgit2's repository mailmap. Non-UTF-8 commit encodings are rejected
before the batch acknowledgement because libgit2 does not expose core Git's
`--encoding=UTF-8` conversion behavior.

Core Git also accepts a structurally valid commit with no `author` header and
emits empty author fields. Libgit2 refuses to materialize that object, so the
helper has a narrow raw-ODB fallback for exactly this case: it validates the
tree, parents, committer, encoding, and message, records both author-presence
flags as false, and then reuses the normal libgit2 diff path. Other malformed
commits remain terminal errors.

The process disables libgit2's PROGRAMDATA, system, XDG, and global search
paths before opening the repository. This prevents user-level config and
shared attribute files from changing either preflight eligibility or emitted
records. Repository-local and worktree config remain visible, and repository
`.gitattributes` files remain authoritative. The test suite poisons every
external config path while checking both a successful scan and a local
textconv rejection.

Each helper caps libgit2's object cache at 128 MiB, mmap windows at 64 MiB,
and total mapped pack windows at 128 MiB. Libgit2's 64-bit defaults permit a
1 GiB window and 8 GiB mapped total; ten uncapped GitLab workers were observed
at roughly 1.1–1.6 GiB RSS each, which is not an admissible baseline.

## Git-compatible diff layers

Native libgit2 1.9.1 is not record-equivalent to Git 2.51 even when similarly
named options are pinned. Two independently implemented adapters use only
public libgit2 APIs:

- zero-context text diffs remove a large identical tail before calling
  `git_diff_buffers`, retaining through the next newline. Core Git performs
  this reduction outside xdiff; libgit2 calls xdiff directly. Tree/file
  classification remains libgit2-owned;
- rename detection supplies a `git_diff_similarity_metric` that splits bytes
  at LF or 64 bytes, ignores CR in text CRLF, counts matching chunk bytes, and
  scores copied bytes over the larger blob. The implementation uses its own
  vector/sort data structure and does not vendor core Git's GPL diffcore code.

Pure binary renames and mode-only changes report `binary=false`, matching the
absence of a binary marker in Git's patch stream when blob OIDs are equal.
Compile with `-DBL_NATIVE_LIBGIT2_SEMANTICS` only to reproduce native-libgit2
behavior for a controlled experiment.

## Self-history experiment

The repository snapshot contained 595 requested commits and 3,481 supported
file records. All results below used four persistent workers.

| Variant | Hunks | Canonical bytes | Digest/result |
| --- | ---: | ---: | --- |
| stock Git reference | 14,942 | 25,714,659 | `19d5317b...` |
| native libgit2 1.9.1 | 15,019 | 25,709,994 | `be977376...`, mismatch |
| no indent heuristic | 15,019 | 25,709,994 | different digest, mismatch |
| minimal + indent | 15,019 | 25,709,994 | no change |
| patience | 14,890 | 25,701,600 | mismatch |
| inter-hunk context 1 | 12,360 | 25,541,813 | over-merged |
| inter-hunk context 2 | 11,278 | 25,472,999 | over-merged |
| latest libgit2 source | 15,019 | 25,709,994 | same mismatch shape |
| common-tail adapter only | 15,012 | 25,709,527 | four commits still differ |
| adapter + threshold 66% | 14,942 | 25,714,891 | counts match, records differ |
| both compatibility adapters | 14,942 | 25,714,659 | `19d5317b...`, exact |

Rename-limit values 50, 100, 200, 500, 1,000, and 2,000 produced identical
records. Thresholds from 55% through 90% traded false positives for false
negatives; no single threshold matched Git because the underlying similarity
scores differ. At 66%, one Git rename became an add while one Git add became a
rename despite exact aggregate counts.

Run the one-pass commit-level differential gate with:

```sh
go run ./contrib/gitengine/compare \
  -engine libgit2 \
  -helper ./contrib/gitengine/libgit2-helper/betterleaks-libgit2-engine \
  -repo .
```

The expected result for the measured snapshot is
`compared=595 mismatches=0`.

## GitLab FOSS hardening

The 197,173-commit GitLab FOSS gate found issues the small fixture and
self-history could not:

- a final partial similarity chunk could append when the vector was exactly
  full. The later heap-corruption trap appeared in pack allocation; the
  regression fixture uses a complete LF chunk followed by a partial chunk;
- `git_commit_author_with_mailmap` rejects a present author with an empty
  email. The helper now reads the base signature and calls
  `git_mailmap_resolve` directly, preserving `Name <>` as present-but-empty;
- `git_commit_summary`/`git_commit_body` do not reproduce pretty-medium
  message semantics. The helper now normalizes the raw message with Git's
  eight-column tab expansion and the Go parser's Unicode whitespace set.
  Regressions cover a tab-indented conflict line and U+0085 at a folded title
  boundary.

The cache caps reduced observed late-run worker RSS from roughly 1.1–1.6 GiB
to 0.15–0.51 GiB in the earlier run, but they do not cap all diff and rename
working memory. In the corrected full gate, ten helpers reached an observed
aggregate lower bound of 7,131,568 KiB and an individual lower bound of
763,440 KiB.

That corrected gate was stopped after more than 18 minutes because the runtime
and memory cost already constituted a decisive scalability loss. It had not
yet emitted its final digest or counts, so this run makes **no** GitLab-scale
correctness claim and provides only elapsed-time and RSS lower bounds. A brief
documentation search overlapped startup as well, which independently
quarantines timing. The partial JSONL result and its source/binary provenance
are in `results/`. Full GitLab parity therefore remains unproven; the exact
595-commit self-history gate is the strongest completed semantic result for
this arm.
