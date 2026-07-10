# Betterleaks Git engine protocol v1

The protocol is a bounded binary stream over helper stdin/stdout. Integers are
big-endian. A `bytes` value is `uint32 length` followed by that many opaque
bytes. A boolean is one byte and must be `0` or `1`. Signed integers use their
two's-complement bit representation.

Every frame is:

```text
uint32 frame_length  # bytes after this prefix; minimum 12
byte[4] tag
uint64 batch_id
byte[] payload       # frame_length - 12 bytes
```

The Go wrapper defaults to a 16 MiB frame limit and rejects a length before
allocating. Helpers must apply the same configured bound. Continued hunks have
a separate assembled-record limit (256 MiB by default). OIDs are raw 20-byte
SHA-1 or 32-byte SHA-256 values, as selected by `HELO`; they are not hex text.

## Handshake and requests

- `HELO`, batch 0: `uint16 version`, `uint16 oid_length`, `bool all_statuses`,
  `bool find_copies`. Version must be 1 and OID length must be 20 or 32.
- `ERRO`, batch 0, kind 5 may be emitted instead of `HELO` when repository
  preflight finds an unsupported feature. The helper then exits cleanly. This
  is the only unsupported signal that permits whole-scan fallback.
- `BGIN` request: `uint32 commit_count`, then `bytes oid` for every commit.
- `BGIN` response: empty payload. It acknowledges the request batch ID and is
  the first response frame for that batch.
- `QUIT`, batch 0: empty payload. The helper drains prior work, exits zero, and
  emits no response.

Requests are sequential within one process. The Go side may write `BGIN` while
it reads the response so a large request and early helper output cannot fill
both pipes and deadlock.

The commit list is a set: duplicate OIDs are rejected before any request bytes
are written. An empty list is valid and completes as an empty `BGIN`, followed
by an OK zero-count `BEND`.

## Record frames

The helper emits a nested stream:

```text
BGIN
  CMIT
    (FBEG (HUNK | HBGN HADD+ HEND)* FEND)*
  CEND
  ...
BEND
```

`CMIT` payload:

```text
bytes oid
bytes message
bytes author_name
bytes author_email
int64 author_unix_seconds
int32 author_utc_offset_minutes
bool has_author
bool has_author_time
```

`FBEG` payload:

```text
bytes commit_oid
byte status
uint32 old_mode
uint32 new_mode
bytes old_oid
bytes new_oid
bytes old_path
bytes new_path
bool binary
```

Known status bytes are `A C D M R T U X B`. Production v1 admits only `A M R`;
the others exist for the test-only all-status profile. Git paths must not
contain NUL. `old_oid` or `new_oid` may be empty only when a profile has no
object on that side; canonical Git normally emits an all-zero full-width OID.

`HUNK` payload:

```text
bytes commit_oid
bytes new_path
uint64 new_position
bytes added
bool missing_final_newline
```

When that payload would exceed the frame bound, the helper emits:

```text
HBGN  bytes commit_oid, bytes new_path, uint64 new_position
HADD  opaque added-byte chunk
...   one or more nonempty HADD frames
HEND  bool missing_final_newline
```

Every continuation frame repeats the batch ID. `HBGN` is valid only inside an
open file, `HADD` only inside an open continuation, and `HEND` closes exactly
one continuation. The final-newline state lives on `HEND` because Git discovers
it only after emitting the added bytes; this permits `HADD` to stream without
buffering the complete hunk. `FEND`, `CEND`, `BEND`, a second `HBGN`, ordinary
`HUNK`, malformed `HEND`, or EOF while a continuation is open is a terminal
protocol error. Each `HADD`
obeys the frame limit and the receiver checks the assembled size before every
append, so a helper cannot bypass the independent record limit.

`FEND` and `CEND` have empty payloads. Every record repeats the batch ID in its
frame header. Commit records must match requested OID order; file and hunk
records must match the currently open commit and destination path.

## Completion and errors

`BEND` payload:

```text
byte status          # 0 OK, 1 rejected
uint64 commit_count
uint64 file_count
uint64 hunk_count
```

The Go wrapper accepts a batch only after an OK `BEND` whose counters match the
observed stream and whose commit count matches the request. EOF, a short or
oversized frame, lifecycle violation, counter mismatch, callback error, or
nonzero helper exit makes the worker terminal. Already emitted callback data
belongs to a failed scan and must not be published.

`ERRO` payload:

```text
byte error_kind      # 1 protocol, 2 helper, 3 canceled, 4 emission, 5 unsupported
bytes operation
bytes message
```

`ERRO` is terminal. Kind 5 is valid only with batch 0 before `HELO`; at any
other point it is a protocol violation. Runtime failures never trigger a
mixed-engine retry. A fallback decision is valid only at repository-wide
preflight and before any worker opens or emits.

Record callbacks are cooperative. They must return promptly, observe the
`ScanBatch` context, and must not call methods on the same worker reentrantly.
Cancellation closes/terminates the helper and waits for the protocol reader,
but Go cannot forcibly terminate an arbitrary callback that ignores its
context; no stronger guarantee is claimed.
