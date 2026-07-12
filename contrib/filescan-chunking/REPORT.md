# File-scan chunking throughput study

Date: 2026-07-12

## Verdict

Keep the 100,000-byte logical chunk, 25,000-byte peek budget, 4 KiB reader,
and double-newline boundary contract, but replace the byte-at-a-time boundary
loop with buffered span scanning and lazily reserve pooled peek headroom: this
improves Linux-tree end-to-end wall time by 6.77% with a paired 95% interval of
5.41%--8.09%, reduces CPU by 4.94%, and preserves every admitted finding.

No fixed larger chunk or file-size adaptive policy is recommended. Larger
chunks helped wall time on one mixed source tree but became sharply more
expensive on the large-file proxy; smaller chunks reduced CPU on large files
without establishing an end-to-end throughput win.

## Environment and method

- Source revision: `b8c1d68a8cb58dd10a5aee9268a325459f93e4ea`.
- Host: Linux 6.12.94, aarch64, 64 one-thread cores, one NUMA node, 4 KiB
  pages, 64 MiB aggregate L2 and 32 MiB L3.
- Toolchain: Go 1.26.4, `-buildvcs=false -trimpath`.
- Baseline binary SHA-256:
  `a1c5061432b536691a51d83a78cb8a55da03ac20f2cb02fd1425bdfb0876c6e0`.
- Candidate binary SHA-256:
  `db90f0c16719c04f7f8adf2e1e1971c350c1e17e743cd00f03729cc67b4abd21`.
- Each final comparison used a fixed number of randomized, exactly
  counterbalanced paired blocks (equal A--B and B--A orders). No early stopping
  was used. Every execution emitted JSON, which was sorted by full canonical
  object JSON before count and SHA-256 comparison.
- Ratios are candidate / baseline. Intervals are two-sided paired bootstrap
  intervals over within-block ratios (200,000 resamples for Linux; 100,000 for
  the other classes).
- The practical Linux threshold was 5%. The lower improvement bound is 5.41%,
  so the balanced final series clears that threshold.
- The final warm Linux rows recorded zero major faults and zero input blocks
  for both binaries. This study therefore makes no cold-storage claim.

The Linux final plan SHA-256 is
`991107cd5f469c0fb3c6303dac63532937b79e2ca2d4f2cd85944acbba621dbb`;
its raw JSONL SHA-256 is
`0eaded5789d728dccb4a774452761e9e79713260b79907989034be2d296c8893`.
The checked-in `run_ab.py` and `analyze.py` are the exact final orchestration
and analysis programs.

## Final binary A/B

Wall and CPU are medians. RSS is per-process maximum resident set size, not
unique fleet memory. `95% wall ratio` is the paired interval.

| Corpus | Blocks | Baseline wall | Candidate wall | Wall ratio (95%) | Baseline CPU | Candidate CPU | Median RSS KiB (A / B) | Findings |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| Linux `2c7c88a`, 94,742 files / 1.8 GiB | 30 | 2.1226 s | 1.9519 s | 0.9323 [0.9191, 0.9459] | 44.631 s | 42.482 s | 313,966 / 333,670 | 64, identical |
| Rust `3ead112e`, 60,898 files / 416 MiB | 20 | 0.5102 s | 0.4952 s | 0.9735 [0.9548, 0.9927] | 11.276 s | 11.047 s | 207,264 / 202,582 | 0, identical |
| GitLab locale, 80 multi-MiB files / 159 MiB | 20 | 0.3431 s | 0.3378 s | 0.9840 [0.9691, 0.9986] | 5.812 s | 5.725 s | 110,194 / 99,604 | 0, identical |
| Bounded-small, 50,000 files / 562 MiB | 20 | 0.3475 s | 0.3537 s | 1.0042 [0.9829, 1.0226] | 8.624 s | 8.662 s | 119,460 / 124,832 | 0, identical |
| Go download cache, archive depth 1 / 150 MiB | 12 | 3.5677 s | 3.5679 s | 0.9984 [0.9953, 1.0011] | 9.355 s | 8.833 s | 111,608 / 110,740 | 216, identical |

Linux median RSS increased 19,704 KiB (6.3%) in the final series. This is the
cost of retaining extended buffers in the pool; it did not reproduce on the
other workload classes. The wall-time win is large enough to recommend the
change, but deployments with a tight per-process memory limit should include
this delta in their rollout guard.

The non-Linux result files have SHA-256 values:

- locale: `2661a13eb6e36b2114ee6b428636b6a8ec0440d70986cfc08d786d9e96272ca8`;
- bounded-small: `8ca5a235338ff64a62613d4e4096d29d754f5034a40206a27dbe99b7a4682ef6`;
- Rust: `eba23d8e7fc0d5be5244f6a0b580500628f1b9ca0017095531172aab379aea65`;
- archive-heavy: `ef1d6d0e25c06b18a08a43126f07f5c3a8c15f3e4f3cdd342b679f58444cdbcf`.

The archive-heavy default-depth oracle is intentionally excluded: malformed
nested input triggered the known third-party `rardecode` panic. The admitted
archive series used the previously frozen depth-1 contract.

## Profile attribution

The baseline Linux CPU profile attributed 69.1% cumulative sampled CPU to
`Detector.detectFragment`. `readUntilSafeBoundary` was nevertheless material:
2.69 seconds, or 7.4% of 36.28 sampled CPU-seconds. Its `ReadByte` calls and
`bytes.Buffer.WriteByte` growth were individually visible. System calls were
7.0% of samples, so the run was neither purely detector-bound nor storage-bound.

The candidate profile reduced boundary work to 0.79 seconds, or 2.3% of 33.80
sampled CPU-seconds. Allocated space fell from 5.09 GiB to 4.83 GiB, and the
baseline's 289 MiB `bytes.growSlice` site disappeared from the leading
allocation profile. `bytes.Buffer.String`, decoding, Aho-Corasick, and RE2/Wasm
remain the dominant work; those are the next plausible CPU levers.

## Tournament and negative results

### Chunk size

On Linux, larger chunks reduced wall time but not CPU further and increased
memory: 128,000-byte chunks had wall ratio 0.920 and +12% median
RSS; 192,000 bytes had ratio 0.911 and +31% RSS; 256,000 bytes had ratio 0.896
and +58% RSS. That signal did not generalize.

On the multi-MiB locale proxy, 128,000 bytes increased CPU 3.2%; 192,000 bytes
increased CPU 11.5% and wall 2.1%; 256,000 bytes increased CPU 25.5% and wall
9.9%; 1,024,000 bytes increased CPU 172.6% and wall 74.2%. Conversely, 32,000
bytes reduced CPU 12.1% but produced no wall improvement (wall ratio 1.015),
while 64,000 bytes reduced CPU 6.2% with an inconclusive 1.2% wall estimate.

Conclusion: detector cost is nonlinear in fragment size, but file-size
adaptation does not yet improve the requested throughput metric. Keep the
existing fixed size. A separate resource-efficiency mode could revisit small
chunks for large text files, but it is not this result.

### Boundary semantics

- A single-newline boundary loses a supported multiline rule when the match
  crosses the 100,000-byte split.
- Fixed chunks lose a secret that straddles the split.
- Reducing max peek from 25,000 to 4,096 bytes loses a long multiline match.

These configurations happened to match the Linux corpus but fail the
adversarial configured-rule test. They are broken, not fast, and were excluded
from timed confirmation. The optimized implementation is differentially
checked against a bytewise oracle over 5,000 deterministic randomized inputs,
including reader sizes from 16 to 256 bytes and peek budgets from 0 to 511.

### Reader buffering and lower-level backends

Increasing the boundary reader from 4 KiB to 16/32/64 KiB added only about
0.5/0.7/1.1 percentage points of Linux CPU improvement beyond the bulk
algorithm in a six-block pilot. The 64 KiB arm raised median RSS about 24%.
The reader stays at 4 KiB.

The logical fragment buffer is downstream of the experimental buffered,
direct-I/O, mmap, and io_uring readers. Their physical prefetch or registered
buffer size is independent: io_uring uses `p.bufSize`, per-file depth, and
ordered stream delivery; direct I/O additionally rounds at least
`defaultBufferSize` to device alignment. Keeping 100,000 bytes avoids changing
those budgets and alignments. `Peek`/`Discard` consumes the same `bufio.Reader`
stream and therefore composes with those backends without changing logical
output. Existing local backend pilots did not establish an io_uring or direct
I/O wall-time winner, and the final warm runs have no storage-read signal, so
no storage/SPDK work is justified by this study.

## Hardware-mechanism decisions

- Cache blocking is not the mechanism: file bytes are streamed once and the
  detector, not a reused loop tile, dominates after the boundary fix.
- Non-temporal loads/stores and explicit cache-line maintenance are inapplicable
  to ordinary coherent heap buffers that are immediately read by the detector.
- No application-cache freshness or invalidation contract is involved.
- SIMD was not added. The scalar bulk algorithm removes most of the measured
  hotspot without unsafe ISA-specific code; the residual boundary path is only
  2.3% cumulative CPU.
- `perf` was unavailable, so no PMU claim is made about cache misses, memory
  bandwidth, MLP, or TLB walks. The data do not justify huge pages, prefetch,
  or translation changes.

## Recommended change

1. Use buffered `Peek`/scan/`Discard` spans while preserving the exact
   whitespace-aware double-newline state machine and 25,000-byte limit.
2. Start each file with the existing 100,000-byte pooled buffer. Only after a
   full logical read, lazily swap to a pooled buffer with 25,000 bytes of spare
   capacity, avoiding repeated grow/copy allocations on multi-chunk files and
   avoiding an up-front charge for small files.
3. Keep all public behavior and constants unchanged.

## Validation

- `go test ./...`: pass.
- `go test -race ./sources ./detect`: pass.
- `go vet ./sources ./detect ./cmd`: pass.
- `python3 -m py_compile contrib/filescan-chunking/run_ab.py contrib/filescan-chunking/analyze.py`: pass.
- `git diff --check`: pass.
- Full `go vet ./...` remains red on two untouched generator warnings:
  `cmd/generate/config/rules/minimax.go:27` and `zai.go:34` both call
  `append` with no values. The changed packages pass vet.
