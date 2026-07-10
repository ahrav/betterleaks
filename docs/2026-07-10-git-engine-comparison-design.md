# Git engine comparison design

Date: 2026-07-10
Status: the shared contract, Go harness/reference, digesting benchmark, and all
three engine prototypes are implemented. Custom Git, libgit2 with two public
compatibility adapters, and a gix+safe-libgit2-diff hybrid match the complete
595-commit Betterleaks history. The custom Git arm also matches all 197,173
commits in the frozen GitLab snapshot after its first pass exposed a
commit-message whitespace-normalization mismatch class and the builtin adopted
the reference's title/body normalization. Its corrected-profile full-history
warm-cache measurements are
complete. The other candidates have not passed the complete correctness,
memory, and repeated-performance gates. No arm has established cold-storage
behavior or a statistically significant performance difference.

## Objective

Compare three implementations of the component that turns an ordered set of
commit IDs into the scanner-consumed Git records:

1. a small custom builtin based on canonical Git internals;
2. a libgit2 helper;
3. a gix helper.

The current `git log --no-walk --stdin` plus fast parser remains the semantic
oracle and portable fallback. All candidates use ordinary repository storage
in this phase. Object representation and SPDK are separate later experiments.

## Why the comparison is worth doing

The full GitLab FOSS benchmark contains 197,173 commits, 3,436,603 packed
objects, and about 4.18 GB of scanner input. The default scan took 189.02
seconds; the same source/parser/scheduler path with the default prefilter and
no rules took 128.73 seconds. Git production alone took 88.36 seconds.

The stock static persistent-text prototype was 8.0% slower. The original
dynamic result is invalid: it used a valid empty-tree pair as a delimiter, but
that path is not flushed by `diff-tree --stdin`; `stdbuf` and line-by-line
Python draining then measured a different, artificially expensive protocol.
The corrected invalid-input delimiter and concurrent pipe handling completed
the full GitLab run. Against equivalently chunk-drained output, ten persistent
dynamic workers took 104.30 seconds versus 113.12 seconds for 81 short-lived
workers, a 7.8% wall improvement. A second same-order pair was 88.63 versus
150.40 seconds, a much larger 41.1% gap. In the reversed-order pair,
short-lived Git ran in 128.11 seconds and persistence ran in 129.60 seconds,
losing by 1.2%. This order/cache sensitivity precludes a central estimate but
keeps stock persistence alive as a text control. It does not predict
embedded/native-record performance and needs a longer alternating series.

## Custom Git measured result

The first complete custom Git comparison reported 1,691 commits whose message
fields differed only in whitespace normalization. That exact first-pass count
is a contemporaneous observation whose comparator transcript was not retained,
so the durable claim is the mismatch class rather than the number. The builtin
now normalizes Git's pretty-medium title and body before emitting `CMIT`. A
subsequent retained full comparison covered all 197,173 commits with zero
mismatching commits.

The frozen GitLab snapshot produced the following authoritative identity for
both the stock reference and custom Git:

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

The benchmark enforces snapshot digests, the canonical multiset digest, record
counts, and canonical byte count before it emits a successful result. These
checks prevent a missing or changed record stream from appearing as a speedup.

### 20,000-commit configuration tournament

The warm-cache tournament held the frozen snapshot, ten workers, production
batch sizing, and a deterministic sample of contiguous commit runs constant.
It compared:

- the default persistent custom Git workers;
- packed Git mmap window/limit pairs `16m/256m`, `64m/512m`, and `256m/1g`;
- default helpers recycled after 1, 2, 4, or 8 completed batches;
- `64m/512m` helpers recycled after 1 or 4 completed batches.

Every row produced 2,773,077 records, including 330,396 file records and
2,422,681 hunk records, 612,112,014 canonical bytes, and digest
`d06726bb2e56dc7d11dda9689c4d79fbe7dc5eb6f84797f3385c82f72a6462ff`.
Each configuration has one observation. The matrix selects configurations for
full-history measurement; it does not establish a performance distribution.

This historical matrix predates Git-environment parity in the harness. Its
custom helpers inherited Git's 96 MiB delta-base cache while the production
reference used 128 MiB. All rows are marked
`profile_confounded_delta_cache_96m_vs_128m`. Counts and digests remain valid,
and same-profile comparisons remain exploratory, but these are not
release-profile timing/helper-RSS measurements.

All rows reported zero helper page-ins. The tournament therefore measures
warm-cache mmap, CPU, process-start, and memory effects. It provides no evidence
about a repository larger than RAM, cold page cache behavior, NVMe limits,
`io_uring`, `io_uring_cmd`, xNVMe, or SPDK.

### Full-history warm-cache measurements

The corrected series contains three rotated-order observations for the stock
reference, custom Git with default packed Git settings, and custom Git with
`64m/512m` plus recycling after one batch. Both Git-based arms used the same
effective profile: isolated global/system configuration, replace objects and
prompting disabled, a 128 MiB delta-base cache, `MALLOC_ARENA_MAX=2`, and no
zlib preload. The parent environment reconstructed immediately after the runs
had `GIT_DIFF_OPTS` and inherited `GIT_CONFIG_*` injection unset; the wrapper
did not capture those absences at launch. The shared environment builder now
removes them mechanically and hostile-environment tests cover both arms.

| Configuration | Wall median, range (s) | Scan median, range (s) | Peak aggregate helper RSS median, range (GiB) |
|---|---:|---:|---:|
| Stock reference | 125.337, 124.349–145.309 | 122.940, 122.360–142.608 | 9.347, 8.757–9.656 |
| Custom default | 111.432, 108.131–112.648 | 108.768, 105.348–109.657 | 19.340, 19.253–19.365 |
| Custom `64m/512m`, recycle 1 | 121.471, 116.070–147.430 | 118.313, 113.637–145.156 | 6.770, 6.725–6.842 |

Three observations are insufficient for a formal significance claim,
especially with visible run-order drift. In the observed rows, default custom
Git's wall range did not overlap reference and its median was 11.1% lower, at a
106.9% higher aggregate-helper-RSS median. The bounded/recycled arm overlapped both
other wall-time ranges; its median was 3.1% below reference with 27.6% lower
helper RSS. Relative to custom default, it was 9.0% slower with 65.0% lower
helper RSS.

The earlier bounded-persistent `64m/512m` observations are archived and
profile-confounded. That arm was not rerun because the same-profile exploratory
pair was dominated by recycle-after-one in both orders. It is excluded from
the corrected release-profile comparison.

`measure_bench.py` samples matching helper descendants of the benchmark
wrapper. Peak aggregate helper RSS is the largest sampled sum of resident pages;
peak individual RSS is the largest sampled process value. VSZ measures reserved
virtual address space rather than physical memory, and aggregate VSZ must not
be interpreted as resident memory pressure. Sampling can miss peaks between
intervals and excludes the Go wrapper.

A row enters the timing summaries only when the command exits successfully,
emits a benchmark object, matches every frozen snapshot/output value above,
uses the same effective Git profile, and has admissible embedded validity. All nine
corrected rows pass and are marked
`admissible_same_effective_profile_reconstructed_env`. The content-addressed
closeout provenance preserves their exact wrapper commands, post-run executable
hashes, reconstructed Git settings, host/tool identity, repository identity,
and a clearly labeled post-run Betterleaks worktree snapshot. The benchmark
binary was rebuilt after the series, so its post-run matching hash is not
contemporaneous proof of the run binary. The older balanced artifact embeds
the overlap and failed-preflight statuses directly in rows 1 and 9; its
matching sidecar remains audit history. Its custom rows carry the delta-cache
profile-confound marker.

Raw measurements and their validity annotations are stored in:

- `contrib/gitengine/custom-git/results/gitlab-full-warm-corrected-profile-2026-07-10.jsonl`;
- `contrib/gitengine/custom-git/results/gitlab-full-warm-corrected-profile-2026-07-10.notes.md`;
- `contrib/gitengine/custom-git/results/gitlab-full-warm-corrected-profile-2026-07-10.provenance.json`;
- `contrib/gitengine/custom-git/results/gitlab-full-warm-balanced-2026-07-10.jsonl`;
- `contrib/gitengine/custom-git/results/gitlab-full-warm-balanced-2026-07-10.validity.jsonl`;
- `contrib/gitengine/custom-git/results/gitlab-full-warm-balanced-2026-07-10.notes.md`;
- `contrib/gitengine/custom-git/results/gitlab-sample20k-matrix-2026-07-10.jsonl`;
- `contrib/gitengine/custom-git/results/gitlab-sample20k-matrix-2026-07-10.notes.md`.

The saved benchmark artifacts enforce the complete output identity but do not
retain the zero-mismatch comparator transcript. The next scheduled full
comparison must preserve that transcript beside the benchmark JSONL.

## Exact consumed-output contract

The v1 engine output is a sequence of requested commits containing zero or
more typed file records for the exact production filter. Lower-case
`--diff-filter=tuxdb` excludes type-change, unmerged, unknown, delete, and
broken-pair statuses; with rename detection pinned by `-M50%`, production
output statuses are add, modify, and rename. Copy detection is not enabled in
v1. The engine oracle asserts both included records and the
absence of excluded statuses. Deletes remain on the existing user-options and
staged compatibility paths rather than the v1 replacement engine. The scanner
consumes:

- commit SHA;
- complete commit message using the current title/body joining semantics;
- author name and email, including the distinction between absent/invalid and
  present-but-empty values;
- author date, with zero versus valid time preserved and formatted as UTC
  RFC3339 by the Go layer;
- destination path as arbitrary non-NUL Git path bytes stored in a Go string;
- status and binary flags;
- destination blob OID when a destination blob exists, so archive attribution
  does not depend on a second `commit:path` lookup;
- for each text hunk, the new-side starting line and the exact concatenation of
  added-line bytes, including empty lines, CRLF, and final-newline state.

At the separate scanner-projection layer, non-archive binaries are skipped.
Archive binaries
require access to the exact destination blob named by the engine record.
Global prefiltering observes commit metadata and path before fragment
scheduling. Engine equivalence therefore cannot be inferred from findings
alone: modes and non-archive binaries may legitimately yield none.

The default history contract first pins a Betterleaks scan profile rather than
inheriting mutable repository-local presentation and merge defaults:

```text
git log -p -U0 --no-walk=unsorted --stdin --diff-filter=tuxdb
  --pretty=medium --encoding=UTF-8 --root --no-diff-merges -M50%
  --full-index --src-prefix=a/ --dst-prefix=b/
  --no-color --no-ext-diff --textconv
  --diff-algorithm=myers --indent-heuristic
  --use-mailmap --date=default
  --no-decorate --no-notes --no-show-signature
```

The stock reference command and every candidate must use this same profile.
The environment clears `GIT_DIFF_OPTS`, so it cannot override `-U0`;
reference CLI commands additionally pin `core.quotePath=true`. Full index lines
cross-check blob OIDs when Git emits an
`index` header; pure renames and mode-only changes omit that header. The exact
reference adapter therefore pairs the patch stream with a separate
NUL-delimited `git diff-tree --raw --full-index` stream using the identical
root/merge/rename/filter profile. That companion is authoritative for status,
paths, modes, and old/new OIDs; the patch stream is authoritative for metadata,
binary classification, hunk positions, and added bytes. Fixed prefixes and
UTF-8 output make the parser-visible patch grammar deterministic. Live fixtures
still differential-test commit encoding headers, invalid bytes, and quoted
paths.

The default history contract also includes:

- the exact commit set selected by `git rev-list --all` under the isolated Git
  configuration;
- default `git log -p -U0 --no-walk --stdin --diff-filter=tuxdb` root, merge,
  rename/copy, binary, attribute, submodule, and mode-change behavior;
- no replace objects, no user/system Git configuration, and a 128 MiB
  delta-base cache;
- committed and info attributes retain canonical Git semantics, including
  `diff`/`-diff`/`text` and textconv selection. The custom Git arm preserves
  textconv inside Git's builtin diff path. libgit2/gix must preflight local
  textconv drivers and return `ErrUnsupported` before emitting a record unless
  they implement matching behavior. Arbitrary external diff commands are
  excluded by `--no-ext-diff`;
- cancellation without partial success, leaked workers, or silent record loss.

User-provided `--log-opts` remain on the current compatibility path in Step 1.

## Common Go boundary

Each engine is a persistent worker owned by one `ParallelGit` worker. The Go
queue continues to assign production batches dynamically. The boundary uses
engine-neutral records rather than the parser-specific `fastGitFile`:

```go
type OID []byte // exactly 20 or 32 bytes, as declared by Capabilities

type CommitRecord struct {
    OID OID
    Message, AuthorName, AuthorEmail []byte
    AuthorUnixSeconds int64
    AuthorUTCOffsetMinutes int32
    HasAuthor, HasAuthorTime bool
}

type FileRecord struct {
    Commit OID
    Status byte // A, M, or R in the production v1 profile
    OldMode, NewMode uint32
    OldOID, NewOID OID
    OldPath, NewPath []byte
    Binary bool
}

type HunkRecord struct {
    Commit OID
    NewPath []byte
    NewPosition uint64
    Added []byte
    MissingFinalNewline bool
}

type Record struct { // exactly one variant is present
    Commit *CommitRecord
    File *FileRecord
    Hunk *HunkRecord
}

type gitEngineFactory interface {
    Preflight(ctx context.Context, repoPath string, profile ScanProfile) (Capabilities, error)
    Open(ctx context.Context, repoPath string, profile ScanProfile) (gitEngineWorker, error)
}

type gitEngineWorker interface {
    ScanBatch(ctx context.Context, request BatchRequest, emit func(Record) error) (BatchResult, error)
    Close() error
}
```

Commit records are emitted even for empty commits, so requested-commit
coverage is observable independently of findings. File records carry exact
modes, status, paths, and post-image OID. Concrete codec implementation may
encode these variants without Go pointers.

The concrete API may change during implementation, but these ownership rules
are fixed:

- `ScanBatch` is synchronous; callers add concurrency by opening workers;
- an emitted record owns every string/byte sequence that can outlive the
  callback or helper read buffer;
- `emit` must return promptly, observe the supplied context, and must not call
  methods on the same worker reentrantly. Go cannot forcibly cancel arbitrary
  caller code blocked inside a callback; this is an explicit interface
  precondition rather than a false no-goroutine-leak promise;
- records stream to bound memory, and a batch succeeds only after an explicit
  success frame. Callbacks from a subsequently failed batch may already have
  run; the overall scan returns an error and its partial findings/report must
  be discarded and never published. There is no rollback or partial success;
- cancellation terminates the helper and unblocks protocol reads/writes;
- `Close` is idempotent at the Go wrapper and observes helper exit status;
- repository-wide `Preflight` runs once before any worker opens or emits. It
  validates protocol/capabilities, object format, repository features, and
  unsupported textconv/config. A helper that cannot support the repository
  emits terminal `ERRO` with batch ID 0 and error-kind byte 5 instead of
  `HELO`, flushes, and exits cleanly. This is the sole signal that permits
  fallback, and fallback is decided only at this barrier. Kind 5 after `HELO`
  is a protocol violation. Any open/runtime failure after workers start is a
  terminal scan error; there is no mid-batch, mixed-engine, or racing
  per-worker fallback.

External helpers use the same versioned, length-prefixed binary protocol so
the comparison does not confound one engine with JSON/text formatting. The
wire schema must represent raw OIDs, paths, messages, identities, and added
bytes without UTF-8 assumptions, include explicit batch IDs and begin/end
frames, and bound every decoded allocation.

Hunks larger than one frame use `HBGN` for commit/path/position, bounded `HADD`
continuation chunks, and `HEND` carrying the final missing-newline boolean; the
decoder applies a separate assembled-hunk bound. The end-frame placement lets
the C sink stream bytes before xdiff reveals final-newline state. A legitimate
line or hunk larger than the default 16 MiB frame is never rejected merely
because the transport has a per-frame allocation limit.

Batch requests reject duplicate OIDs before writing anything, because stock
`git log --stdin` deduplicates them while other helpers may not. An empty batch
is explicitly valid and completes with an OK zero-count `BEND`.

## Comparable state and oracle

Parallel worker completion order is not part of the scanner contract. Exact
sequence comparison is required within a commit/file/hunk; cross-worker output
is compared as a multiset so duplicate occurrences remain observable.

The canonical identity is the complete typed record, not merely
`SHA/path/start-line`: repeated identical hunks count separately. Each field is
length-prefixed before records are sorted and compared, so arbitrary bytes and
field boundaries cannot collide. Four separate bags are retained:

1. all supported engine commit/file/hunk records plus asserted absence of the
   profile-excluded statuses;
2. scanner text fragments and every attribute;
3. archive-derived fragments;
4. final findings.

The reference oracle is canonical Git through the current parser plus an exact
CLI reference adapter for live fixtures. The end-to-end oracle compares
canonicalized finding multisets excluding only presentation-only fingerprints
already excluded by `scripts/compare_reports.py`.

No candidate benchmark is admissible unless both oracles pass.

## Blocking correctness gates

### Deterministic repository fixtures

- root, linear, empty, and merge commits;
- merge commits with changes relative to each parent and the default no-merge-
  patch behavior;
- add, modify, rename, mode-only, mode-plus-content, symlink, and binary
  records; mode-only produces a file record with zero hunks;
- copy candidates that assert production emits add, plus a test-only
  all-status/copy profile that exercises `-C50% --find-copies-harder` and `C`;
- delete, type-change, unmerged, unknown, and broken-pair negative
  fixtures that assert the v1 profile emits no file/hunk record while still
  emitting the requested commit record;
- exact rename, edited rename, copy candidate, rename-limit exhaustion;
- heuristic binary, `-text`, `text`, custom diff driver, and attributes that
  change over history;
- empty blob, huge line, multi-hunk, empty added line, CRLF, and missing final
  newline;
- spaces, tabs, newlines, quotes, backslashes, invalid UTF-8, and Unicode paths;
- empty/multiline/unicode messages, unusual identities, timezone offsets, and
  every date shape currently accepted by the Go projection;
- SHA-1 and SHA-256 repositories when supported;
- alternates, replace refs (which must be ignored), packed and loose objects,
  MIDX, before/after repack and garbage collection, and missing/promisor
  objects. Initial repack coverage is sequential; concurrent repository
  mutation is not claimed unless a later snapshot contract explicitly adds it;
- valid ZIP archives at the same path across commits, rename and delete,
  invalid archive bytes, nested archives at the depth boundary, and a wrong-
  parent/wrong-path blob mutation.

Every positive case has a discriminating negative or boundary twin.

### Metamorphic and property checks

- changing worker count or batch partition preserves the record multiset;
- permuting complete input batches preserves the record multiset;
- splitting and recombining a batch preserves multiplicity and content;
- replaying a batch on a pinned repository snapshot is idempotent;
- no commit is lost or duplicated across the production batch builder;
- every emitted hunk position and byte string equals the canonical Git oracle;
- protocol encode/decode is a lossless round trip within configured bounds.
- SHA-1 and SHA-256 semantic twins produce the same projected records without
  assuming a 20-byte OID;
- moving reachable objects to an alternate, and packing/repacking the same
  graph, preserves the canonical bags; removing the alternate or promisor
  produces an error rather than an empty file.

### Robustness, fuzzing, and lifecycle

- arbitrary/truncated/oversized protocol input cannot panic, hang, or allocate
  beyond the configured frame limit;
- generated valid repositories/commit sequences compare against canonical Git;
- cancellation at request write, object lookup, diff callback, response read,
  and a context-aware blocked Go emission terminates internal goroutines and
  helper processes;
- invalid OIDs, missing/corrupt objects, malformed helper output, version
  mismatch, stderr warnings, nonzero exit, clean premature EOF, and mid-frame
  EOF produce typed errors and never partial success;
- archive lookup/read failures fail the scan; they are not logged and converted
  to success;
- race tests cover queue ownership, callback lifetime, cancellation, and close.

The verification model must record a history of
`invoke(batchID, OIDs)`, `record(batchID, record)`,
`complete(batchID, status)`, and `cancel(batchID)`. Every completed batch must
linearize between invocation and completion and every record must be attributed
to that batch. Required tests include slow/fast overlapping requests, identical records
from distinct OIDs, blocked output cancellation, helper failure with work in
flight, shutdown followed by submission, and the final partial batch.

### Real-repository and findings gates

- betterleaks self-history on every normal test run where Git is available;
- configured corpus replay through `BETTERLEAKS_TEST_CORPORA` and date formats;
- a fixed GitLab FOSS ref snapshot as the primary large-monorepo gate;
- exact native-record multiset and canonicalized final-finding multiset;
- full GitLab execution is a scheduled/explicit integration test, not a unit
  test silently skipped in CI.

## Mutation backstop

The suite must fail clearly if any of these mutations are introduced:

- drop or duplicate the final file, commit, batch, or hunk;
- emit an excluded status or flip binary status;
- use the old path instead of the destination path;
- shift `newPosition` by one;
- remove an empty added line or final newline;
- normalize raw path/message bytes or timezone semantics;
- reuse a helper buffer after the callback returns;
- treat an error/EOF as successful batch completion;
- attach the wrong batch ID or emit after terminal failure;
- substitute a parent or same-path historical blob for an archive;
- compare records as a set and thereby hide duplicates;
- let cancellation return while a helper or emitter remains live.

## Engine-specific research gates

### Custom Git builtin

The implementation follows the researched Git source paths for
revision/object/diff setup, callbacks, rename detection, attributes/binary
classification, commit metadata, object lifetime, batch framing/flush,
cancellation, and build/distribution. The patch remains version-pinned and
does not expose `libgit.a` as a purported stable ABI.

The selected architecture is one persistent builtin process per Go worker:

```text
shared dynamic Go batch queue
  -> custom-git betterleaks--diff-engine --protocol=1
  -> Git revision/object/diff machinery
  -> versioned native record frames
```

Requests are sequential within a process because Git's diff queue and related
state are process-global. Concurrency comes from independent workers, retaining
the existing dynamic scheduler. Repository, configuration, index, mailmap,
object store, pack windows, and delta-base cache live for the process lifetime;
each `SCAN` creates and later releases a fresh `rev_info` so mutable diff state
cannot leak across batches. The request applies the pinned synthetic revision
options and resolves each raw OID with non-dereferencing
`lookup_commit_object()`. Missing objects, annotated tags, trees, and blobs are
typed request rejections; the emitted commit OID must equal the requested OID.
It then emits commit metadata and calls `log_tree_commit()`. Root and ordinary
parent diffs stay in Git; merge diff formats are pinned off because
combined/remerge paths write through separate `combine-diff.c` output
machinery.

#### Native sink patch against Git v2.51

Add a nullable native sink/vtable to `struct diff_options`, with callbacks for
file begin/end, hunk begin, added bytes, and binary state. Keep the patch below
diffcore and driver selection so Git continues to own attributes, rename/copy,
binary detection, textconv, per-driver algorithms, xdiff, and no-final-newline
semantics.

The narrow hook map is:

- `run_diff_cmd()` in `diff.c`, after driver selection and before
  `builtin_diff()`: begin/end each post-diffcore file with access to the
  authoritative `diff_filepair`, resolved status, paths, modes, and OIDs.
  External-diff selection is terminally unsupported with a native sink;
- `fn_out_consume()` immediately after `find_lno()`: report
  `lno_in_postimage` as the hunk start;
- `emit_diff_symbol()`: forward `DIFF_SYMBOL_PLUS` payloads,
  track plus/minus/context, and handle both `CONTEXT_INCOMPLETE` and
  `NO_LF_EOF`. For the exact `CONTEXT_INCOMPLETE` no-newline marker, remove one
  synthetic trailing LF only when the prior symbol was PLUS and mark the hunk
  missing-final-newline. For rewrite-path `NO_LF_EOF`, never remove a byte—the
  payload already lacks LF—and only mark missing when the prior symbol was
  PLUS. MINUS/old-side markers never alter added bytes;
- `emit_rewrite_diff()`: create the line-1 hunk for complete rewrites;
- `builtin_diff()`: report binary files after Git's definitive binary
  classification;
- `diff_flush_patch_all_file_pairs()`: bypass delayed `color_moved` buffering
  when native output is active.

`DIFF_FORMAT_CALLBACK` is not sufficient: it sees file pairs but would require
duplicating Git's builtin binary, textconv, attribute, rewrite, and xdiff path.
Hooking xdiff directly is also unnecessary for v1 because `fn_out_consume()`
already provides the scanner-visible line semantics. The broader test profile
covers delete and type-change exclusion; production exposes only the pinned
A/M/R profile.

The Git-fork files are:

- `builtin/betterleaks--diff-engine.c` for setup and the request loop;
- `betterleaks-engine-protocol.c/.h` for framing and sink state;
- `diff.h` and the six `diff.c` hook points above;
- `builtin.h`, `git.c`, `Makefile`, and `command-list.txt` for registration;
- `t/helper/betterleaks-engine-client.py` and
  `t/t4218-betterleaks-engine.sh` for protocol tests;
- `Documentation/git-betterleaks--diff-engine.adoc` plus
  `Documentation/meson.build` and `t/meson.build` for documentation and test
  registration.

Git's buffered `cat-file --batch-command` request loop is the implementation
template. The helper emits `HELO`, request `BGIN`, typed commit/file/hunk
frames, and `BEND` with counters, flushing only handshake, batch completion,
and error frames. A small hunk may use one `HUNK`; larger hunks stream through
`HBGN`/bounded `HADD` chunks/`HEND`, avoiding both a syscall per added line and
an unrepresentable whole-hunk frame.

Each request resets output/counters and constructs a fresh `rev_info` through
`repo_init_revisions()` and `setup_revisions()`. After setup and before the
first commit it explicitly sets `rev.diffopt.no_free = 1`, as required for
looped `diff_flush()`. Before each commit it clears `found_changes`,
`has_changes`, `check_failed`, and `shown_dashes`; after `log_tree_commit()` it
asserts that no record/hunk remains open and the diff queue is empty. Completion
detaches the sink, sets `no_free = 0`, calls `release_revisions()`, emits
`BEND`, and flushes.

After each commit, the builtin conservatively calls `free_commit_buffer()` for
the commit and directly parsed parents while retaining parsed commit objects
and parent lists. It does not use `unparse_commit()` or clear parent lists in
v1, preserving duplicate-request correctness. Helper RSS must be measured with all
workers: the configured 128 MiB delta-base limit alone can retain roughly
1.25 GiB across ten processes, before pack windows and parsed object state.

Commit metadata uses typed `CMIT` fields. Before `log_tree_commit()`, the helper
calls `pretty_print_commit()` with the pinned medium/date/mailmap/UTF-8 context,
normalizes the rendered title and body, parses the raw author identity and
timestamp, applies mailmap to the name and email, and writes the resulting byte
fields and presence flags. The request sets `rev.no_commit_id = 1` and directs
`diffopt.file` to a non-protocol fallback so `show_log()` cannot write raw or
duplicate metadata to stdout when `log_tree_commit()` flushes the diff. The
full GitLab exact comparison gates these typed fields against the stock
reference.

Cancellation initially terminates the worker rather than adding an in-band
cancel request: close stdin, terminate, escalate to kill after a bounded grace,
drain, and reap exactly once. A helper can be CPU-bound in xdiff and unable to
read a cancel frame.

The helper does not attempt `longjmp` recovery from Git `die()`. Preflight
unsupported features may choose another engine before workers start. Invalid
frames/OID lengths and missing or non-commit OIDs are typed request rejection
followed by clean helper termination. Any `die()`, signal, EOF before `BEND`,
short frame, or nonzero exit is an `EngineTerminalError`; sibling workers are
canceled, partial results are discarded, and the scan is neither retried nor
fallen back. Clean EOF is valid only after `QUIT`.

The patch is version-pinned internal Git code and requires a maintained rebase
and GPLv2 source-compliance plan. Linking a standalone daemon against
`libgit.a` or via cgo does not remove that burden and adds unstable
initialization/lifetime coupling, so those are rejected mechanisms rather than
separate engine arms.

### libgit2

Pin libgit2 and use a C helper rather than per-line cgo callbacks. Candidate
APIs are `git_revwalk`, `git_diff_tree_to_tree`, `git_diff_find_similar`, and
`git_diff_foreach`; the helper must prove parity for Git defaults rather than
assuming similarly named flags are equivalent. The repository/ODB remains
open for the worker lifetime. Custom ODB work is out of scope for Step 1.

### gix

Pin a gix release and use a Rust helper with the common protocol. Candidate
APIs are tree changes, rewrite tracking, diff resource caches, commit-graph and
pack access, and blob diff. Attribute source, rename behavior, merge/root
semantics, object-cache sizing, and helper thread ownership require explicit
parity tests. Custom object storage is out of scope for Step 1.

## Implementation order

1. Define the typed record/error/batch contract and land its length-prefixed
   codec and canonical multiset comparator.
2. Implement the canonical Git CLI reference adapter and verify the existing
   engine against it.
3. Land live fixtures, object-database metamorphic cases, archive tests,
   semantic fault-injection mutations, protocol history checks, and helper
   lifecycle tests before any replacement candidate is trusted.
4. Implement the custom Git builtin from the reviewed research plan.
5. Implement libgit2 and gix helpers behind the same protocol and Go boundary.
6. Run the complete correctness matrix for all three; reject or document any
   semantic exception before performance comparison.
7. Run repeated full-history GitLab measurements with the current path as the
   interleaved baseline. Report wall, total process-tree CPU, RSS, faults,
   bytes, record digest, and finding digest.
8. Only after correctness and profiling, invoke the performance pipeline.
   SIMD work requires a measured CPU-bound vectorizable kernel, an independent
   scalar oracle/fallback, target-specific code-generation evidence, and
   production-distribution benchmarks.

## Open research questions

- The correct snapshot behavior during ref movement, repack, and object loss.
- Exact cross-platform packaging policy for C and Rust helpers.
- Which semantic differences in libgit2/gix are irreducible versus configurable.

## Known baseline defect to resolve before oracle promotion

The current `Git.Fragments` path waits its private `sync.WaitGroup` but does not
wait the `semgroup` that owns scheduled scanner work. Errors returned by
`yield`, archive blob lookup failures, and Git command exit failures can
therefore be logged or accumulated without being returned by `Fragments`.
Before the current implementation is promoted from behavioral reference to
correctness oracle, the suite must specify and enforce terminal error
propagation. A replacement is not allowed to copy silent-loss behavior merely
because it matches today’s code.
