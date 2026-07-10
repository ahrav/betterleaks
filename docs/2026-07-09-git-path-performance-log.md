# Git parsing/scanning performance experiment log

Started: 2026-07-09
Last updated: 2026-07-10

Status: complete

## Objective

Find and validate the next meaningful performance improvement to the default
`git log -p -U0 --diff-filter=tuxdb` parsing/scanning path without changing the
scanner-consumed output contract. Record accepted and rejected candidates,
including negative results.

## Live baseline context

- Branch: `perf/05-fast-gitlog-parser`
- HEAD: `17c568c test(detect): hydrate sanitized digest fixture`
- Worktree before this log: clean
- Toolchain: `go version go1.25.6 darwin/arm64`
- Host: Apple M1 Pro, 10 logical CPUs, 128-byte cache line, 64 KiB L1d,
  4 MiB L2 (reported by `sysctl`)
- Module language/toolchain contract: `go 1.25.0`, `toolchain go1.25.10`

## Verified live contract and path

- The fast parser is selected only when betterleaks constructs the default Git
  log arguments. User `--log-opts` use `gitdiff.Parse`.
- The parser currently emits `*gitdiff.File` through a buffered channel from a
  dedicated goroutine.
- `Git.Fragments` skips deleted files first; it checks binary/archive handling,
  builds commit attributes, applies the global prefilter, then schedules work
  through the detector semaphore.
- Text scanning consumes file name, delete/binary flags, patch SHA/message/
  author/date, fragment new position, and added raw text. The fast parser
  represents each hunk's added text as one synthetic `gitdiff.Line` so
  `rawAddedText` can return it without rejoining lines.
- The default CLI path uses `ParallelGit`; `--git-workers=0` means
  `runtime.NumCPU()`, while explicit `--git-workers=1` isolates a single Git
  producer/parser.

## Measurement design

Primary microbenchmarks:

- `BenchmarkFastParseGitLog/fast`: generated mixed-shape parser workload with
  downstream field consumption.
- `BenchmarkFastParseGitLogConstructed`: larger constructed default-shaped
  stream.

Primary system measurements:

- CPU and allocation profiles of both parser benchmarks.
- Single-worker end-to-end scan to expose parser/scanner coupling.
- Default-worker end-to-end scan to test whether a parser-only gain survives
  concurrent Git production and detector work.
- Real-repository replay/digest parity for accepted candidates.

Discipline:

- Establish repeated baseline runs before edits.
- Treat benchmark-process execution as the highest varying local unit where
  practical; do not treat inner benchmark iterations as independent samples.
- Use identical input, toolchain, CPU, environment, and output handling for
  before/after comparisons.
- Report ns/op, B/op, allocs/op, run count, and benchstat confidence intervals.
- Re-profile after every accepted change because the hotspot may move.

## Candidate register

| Candidate | Initial rationale | Evidence/result | Status |
| --- | --- | --- | --- |
| Parser-to-consumer fusion / scanner-native records | Current path allocates a `gitdiff` object graph and pays goroutine/channel handoff only to immediately project it into `Fragment` values. | The final scanner-minimal representation plus a cancellation-safe buffered batch of 64 cuts scanner-path parser time about 19% to 20%, bytes 51% to 61%, and allocations 26% to 31% on synthetic workloads. | Selected |
| Fully native commit headers | Even the first native-record prototype retained disposable `gitdiff.PatchHeader` and `PatchIdentity` objects. | Native headers now retain only SHA/message/author/date, while the compatibility target assembles complete historical headers directly. | Selected |
| Parser-local allocation reduction | Per-file, per-hunk, path/date/message conversions and builders may dominate B/op and GC work. | Builder pre-growth, fragment-capacity hints, and lookahead-capacity hints were all implemented and measured. Builder growth made the constructed benchmark 25.5% slower; lookahead was 15% slower; fragment capacity was immaterial. | Rejected after experiment |
| Faster line classification (first-byte tree, word tags, SWAR/SIMD search) | Each line can traverse several prefix checks; Go does not auto-vectorize. | First-byte dispatch improved the constructed parser workload by 12.9%, but the mixed result was not significant and it added 96 source lines. A four-byte word-tag classifier lost to it. The larger boundary redesign has better system evidence. | Measured follow-up, not selected |
| Buffer-size sweep | The parser reserves a 512 KiB `bufio.Reader` per parser; parallel mode multiplies it by worker count. | A 12-process, order-balanced 64/128/512 KiB sweep found 64 KiB 9.9% faster on the mixed parser benchmark, unchanged on captured history, and 448 KiB smaller per parser. The 600 KiB spill differential passed. | Selected |
| Whole-input and custom chunk readers | A whole-input parser could remove `bufio` state; an owned-chunk reader could reduce fixed capacity while preserving streaming. | `io.ReadAll` was 12% slower and allocated 173% more on captured history. Custom 64/256/512 KiB readers were no faster than `bufio.ReadSlice`; 64 KiB was 28% slower on captured history. | Rejected after experiment |
| Cache tiling/blocking | The parser is a single streaming pass with little reuse. | The technique's reuse/intensity gate currently fails; retain only if profiling shows a reused working set. | Provisionally rejected |
| Succinct/static index | The stream is consumed once, not queried repeatedly. | The build-once/read-many admission condition currently fails. | Provisionally rejected |
| Explicit cache-line maintenance | Ordinary coherent application memory crosses no PMEM, non-coherent DMA, or JIT publication boundary. | The technique is semantically unrelated to this path. | Rejected |
| DMA/DPDK/io_uring/NIC path | Input is a local Git subprocess pipe and parser/scanner CPU work; no device-owned buffer lifecycle exists. | Would add a subsystem without evidence that I/O submission is limiting. | Provisionally rejected |
| FFI/C classifier | Could unlock C compiler vectorization, but cgo has a fixed transition cost and complicates portability. | A standalone classifier produced identical counts on the 46 MB capture. Per-line cgo took 56.81 ms; batch sizes 4 through 65,536 took 39.13 to 21.27 ms; whole-buffer C took 24.07 ms; the relevant Go line-slice loop took 19.65 ms. | Rejected after experiment |

## Tournament results before final composition

Every serious candidate was implemented far enough to run correctness checks
and repeated measurements; profiling alone was not used as a rejection gate.

### Reader design and capacity

The buffer-size sweep used three separately compiled binaries and 12 independent
processes per size. All six 64/128/512 KiB order permutations were repeated in
reverse order. Captured history had a maximum line length of 2,822 bytes, so
the 64 KiB real replay did not spill; a synthetic line over 600 KiB verified
the spill path.

| Workload | 64 KiB | 128 KiB | 512 KiB |
| --- | ---: | ---: | ---: |
| Mixed parser time | 692.8 us +/- 8% | 730.8 us +/- 15% | 761.2 us +/- 21% |
| Constructed parser time | 2.581 ms +/- 49% | 2.472 ms +/- 38% | 2.579 ms +/- 48% |
| Captured-history time | 34.91 ms +/- 10% | 36.96 ms +/- 10% | 35.48 ms +/- 3% |
| Mixed bytes/op | 533.9 KiB | 597.9 KiB | 981.9 KiB |
| Captured-history bytes/op | 138.0 MiB | 138.1 MiB | 138.5 MiB |

The 64 KiB reader saves 448 KiB per live parser, or 4.375 MiB at ten
concurrent Git workers. Allocation counts were identical. Against 64 KiB, the
512 KiB mixed result was 9.87% slower (`p=0.045`); constructed and captured
history were statistically unchanged.

Whole-input and specialized-chunk readers were also implemented. On captured
history, the baseline `bufio.ReadSlice` parser took 37.07 ms and allocated
138.5 MiB. `io.ReadAll` took 41.57 ms and 377.9 MiB. Specialized 64, 256, and
512 KiB chunk readers took 47.43, 39.38, and 40.14 ms respectively. The
bounded `bufio` lifecycle therefore remains the selected architecture, with
only its capacity reduced.

### Local parser changes

- Pre-growing the added-text builder reduced a small number of allocations but
  made the constructed workload 25.53% slower.
- A two-element initial fragment capacity did not produce a practical win.
- A lookahead-derived capacity hint made the constructed workload 15% slower.
- First-byte file-diff dispatch changed no allocations and improved the
  constructed workload from 2.499 to 2.177 ms (`-12.88%`, `p=0.000`, 10
  processes), but its mixed result changed from 788.6 to 745.2 us and was not
  significant (`p=0.089`). A four-byte word-tag variant was slower.

The first-byte result is retained as a possible later instruction-level
follow-up. It is not being composed into the current structural candidate,
which already changes the same parser and has stronger end-to-end evidence.

### FFI boundary

A standalone C/Go classifier replayed the captured history and verified equal
classification counts before timing. Fixed single-thread medians were 19.65 ms
for Go over line slices, 35.91 ms for a Go rescan of the entire buffer, 56.81 ms
for cgo per line, 39.13/26.01/25.42/22.38/21.27 ms for C batches of
4/16/64/256/65,536 lines, and 24.07 ms for one whole-buffer C call. No C
boundary beat the relevant Go loop, and it would add `CGO_ENABLED` and linker
constraints.

### Native records and handoff shape

On captured history, the compatibility adapter took 42.60 ms and allocated
140.1 MiB with 105.7k allocations. Direct scanner-native records took
33.64 ms and allocated 114.3 MiB with 58.62k allocations. The parser-only
synchronous path was fastest; asynchronous individual records and batches of
4, 16, and 64 were slightly slower in isolation because they retain channel
costs.

That ranking reversed at the full-system boundary. Eight warmed, interleaved
default-worker scans produced:

| Handoff | Median wall time | Change vs baseline | Significance |
| --- | ---: | ---: | ---: |
| Existing eager parser | 3.695 s +/- 4% | baseline | - |
| Synchronous native | 3.705 s +/- 2% | statistically unchanged | `p=0.663` |
| Native batches of 64 | 3.630 s +/- 1% | `-1.76%` | `p=0.036` |

Every report normalized to the same findings digest. The mechanism is a
pipeline crossover: synchronous parsing minimizes parser-local work but stops
Git production while the consumer schedules each file; batches amortize the
channel boundary while retaining parser/detector overlap. The batch-of-64
design is the structural finalist.

Extending native ownership through commit headers then reduced mixed-workload
allocations from 9,302 to 8,208 (`-11.8%`) and constructed allocations from
30,726 to 26,630 (`-13.3%`). Mixed time fell from 649.2 to 607.1 us. This
increment is now being tested in combination with the 64 KiB reader and
batch-of-64 handoff; the isolated gains are not assumed to compose.

### Compatibility assembly and minimal native records

The first scanner-native prototype converted complete native records back into
`gitdiff` objects for `NewGitLogCmdContext`. The public output contract was
exact, but a ten-block paired audit at the same 64 KiB reader size found a real
compatibility allocation tax: mixed `B/op`/allocations increased 5.05%/3.87%,
constructed increased 18.92%/5.26%, and captured history increased 1.49%/3.22%.
Timing was too noisy to establish a regression, but the deterministic
allocation increase was not accepted without another experiment.

A single shared state machine with two concrete assembly targets was then
implemented. It selects the target once at semantic boundaries and appends
either compact scanner records or exact `gitdiff` headers/files/fragments;
there is no native-to-compatibility reconstruction traversal. Ten rotated
process blocks recovered the old compatibility allocation counts exactly:

| Compatibility workload | Old 64 KiB parser | Target-specific assembly |
| --- | ---: | ---: |
| Mixed | 533.9 KiB, 11.18k allocs | 533.9 KiB, 11.18k allocs |
| Constructed | 1.900 MiB, 38.92k allocs | 1.900 MiB, 38.92k allocs |
| Captured history | 138.0 MiB, 102.3k allocs | 138.0 MiB, 102.3k allocs |

No compatibility timing difference was significant. Against the conversion
adapter, the target-specific design removed the second file/fragment object
graph and traversal. Against the old direct parser, it changed native-path
geomean time by only `+0.24%` (`p>=0.481`) with identical bytes and allocation
counts.

Separating the targets also made adapter-only fields removable from native
records. Native fragments now retain only added raw text and new position;
native files retain name/delete/binary state; native headers retain SHA,
message, author, and author date. Compatibility parsing still constructs and
validates the complete historical `gitdiff` projection. This second arm won on
its own:

| Native workload | Full native projection | Scanner-minimal projection | Change |
| --- | ---: | ---: | ---: |
| Mixed bytes/op | 393.6 KiB | 348.5 KiB | `-11.46%` |
| Constructed bytes/op | 1.438 MiB | 1.016 MiB | `-29.34%` |
| Captured-history bytes/op | 113.7 MiB | 112.3 MiB | `-1.25%` |
| Mixed time | baseline | `-1.77%` | `p=0.043` |
| Constructed time | baseline | `-4.68%` | `p=0.015` |
| Captured-history time | baseline | statistically unchanged | `p=0.739` |

Allocation counts were unchanged except for 12 extra allocations across the
entire 46 MB capture (`+0.02%`). The combined parser therefore improves the
scanner path without making the exported compatibility channel pay for it.

### Batch-size, byte-bound, and cancellation audit

Fixed batches of 64, 128, and 256 were measured in eight single-threaded
process blocks after the native-header change. Across mixed, constructed, and
captured-history parser workloads, 64 was the best fixed size overall; 128 and
256 retained more batch storage and did not improve the captured replay.

A hybrid flush at 64 files or 1 MiB of owned added-text payload was also
implemented. It was neutral on the captured replay (35.19 ms versus 34.68 ms
for fixed 64 within local noise), used the same 113.9 MiB/op, and avoids waiting
for 64 file records when one early file is unusually large. It remains in the
end-to-end handoff tournament rather than being selected from parser timing.

An adversarial lifecycle audit found that the initial batch sender could block
forever after the consumer canceled, and a capacity-one channel allowed a
consumer-held, queued, and producer-blocked batch at once. The candidate was
changed before final timing:

- every full and final-partial send selects between delivery and cancellation;
- `Git.Fragments` cancels its private parser context before waiting for Git;
- cancellation is checked between files inside a received batch;
- both buffered and unbuffered fixed/byte-bounded variants are tested so the
  memory/overlap tradeoff is measured rather than assumed.

Adversarial focused tests and ten race-test repetitions verify blocked full and
partial sends exit, a separately canceled scan stops its producer, and
mid-batch cancellation performs no work after the first file. The full
repository test suite also passed after these changes.

The final end-to-end tournament used one immutable candidate binary, one
baseline binary, a separately recorded first/cold observation, and eight warm
rotated blocks per mode. Each block ran baseline, fixed buffered 64, fixed
unbuffered 64, buffered 64-or-1-MiB, and unbuffered 64-or-1-MiB in a rotated
order. All 45 reports contained six findings and normalized to the same digest.

| Handoff mode | Warm mean | Median | CV | Mean change vs baseline |
| --- | ---: | ---: | ---: | ---: |
| Existing parser | 4.1325 s | 4.050 s | 6.90% | baseline |
| Fixed 64, buffered | 3.9575 s | 3.930 s | 5.55% | `-4.23%` |
| Fixed 64, unbuffered | 4.0513 s | 4.060 s | 5.58% | `-1.97%` |
| 64 or 1 MiB, buffered | 3.9813 s | 3.910 s | 8.53% | `-3.66%` |
| 64 or 1 MiB, unbuffered | 4.2538 s | 3.940 s | 18.42% | `+2.93%` |

The fixed buffered candidate's within-block differences were
`[-0.14,-0.21,+0.02,-0.28,0.00,-0.67,-0.01,-0.11]` seconds: mean
`-0.175 s`/`-4.05%`, exact two-sided sign-flip `p=0.0469`. Unpaired benchstat,
which does not account for the strong block-to-block host drift, reported
4.050 s +/- 12% versus 3.930 s +/- 8%, `p=0.292`. Because four handoff modes
were present, the Holm-adjusted value is `p=0.1875`; this tournament is a
nominal replication of the already preselected fixed-64 candidate, whose
earlier independent eight-run comparison reported `p=0.036`, not a claim of
multiplicity-robust discovery. Fixed buffered 64 is selected because it wins
both tournaments, has the lowest warm mean and CV among the two buffered
designs, and the capacity-zero variants did not preserve overlap reliably.

All experiment-only mode switches, byte-bounded batching, unbuffered batching,
and synchronous/per-file handoff code were removed after selection. The native
scan constructor is private, so the exported `NewGitLogCmdContext` and
`DiffFilesCh` contract cannot accidentally expose a nil channel.

### Final parser comparison

Twelve independent, alternating single-threaded processes compare the shipped
scanner boundary with the selected batch path. Benchmark names were normalized
only after collection so benchstat compares identical workloads:

| Workload | Before time | After time | Time change | Before bytes | After bytes | Before allocs | After allocs |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Mixed | 717.3 us +/- 9% | 571.6 us +/- 2% | `-20.31%` | 981.9 KiB | 384.7 KiB | 11.175k | 8.220k |
| Constructed | 2.308 ms +/- 4% | 1.863 ms +/- 4% | `-19.31%` | 2.332 MiB | 1.145 MiB | 38.92k | 26.67k |

All time, byte, and allocation changes above have `p=0.000` at 12 process
samples. On the 46 MB captured history, time changed from 35.48 ms +/- 3% to
34.09 ms +/- 20% (`-3.91%`, `p=0.128`), while bytes fell 138.5 to 112.5 MiB
(`-18.76%`) and allocations fell 102.29k to 53.12k (`-48.07%`), both
`p=0.000`. The real replay therefore establishes the object-lifetime win but
does not independently establish a parser-time win under the noisier host
conditions.

### Multi-repository differential corpus

The composed parser matched `go-gitdiff` on `betterleaks` self-history plus
four additional local repositories (`go-git`, `go-gitdiff`,
`aho-corasick-rs`, and `ahrav-claude-plugins`). The external corpora were each
replayed with Git's default, `iso-strict`, `raw`, and `unix` date formats. All
scanner-consumed projections matched.

## Commands and results

### Recon

```text
git status --short --branch
## perf/05-fast-gitlog-parser...origin/perf/05-fast-gitlog-parser

go version
go version go1.25.6 darwin/arm64
```

### Correctness baseline

```text
go test ./sources -run '^(TestFastParseSyntheticShapes|TestFastParseGeneratedValidStreams|TestParseGitLogDateMatchesGitdiff|TestParseRangeBytes)$' -count=1
ok github.com/betterleaks/betterleaks/sources 1.610s

go test ./sources -run '^TestFastParseMatchesGitdiff$' -count=1
ok github.com/betterleaks/betterleaks/sources 1.371s
```

The second test replays this repository's real Git history through both the
fast parser and `gitdiff.Parse` and compares the scanner-consumed projection.

### Real input shape

Captured with:

```text
git log -p -U0 --full-history --all --diff-filter=tuxdb > /tmp/betterleaks-self.patch
```

The capture is 46,406,128 bytes and 1,431,079 lines: 531 rendered commits,
3,461 file entries, 14,714 hunks, 761,712 added lines, 634,530 deleted lines,
78 binary markers, and 904 new-file markers. Added lines average 30.45 bytes;
104,099 are at most 16 bytes, another 594,244 are at most 32 bytes, 37,314
are at most 64 bytes, 21,498 are at most 128 bytes, and 4,557 are longer.

### Parser benchmark baseline

Twelve independent Go test processes, each with a one-second benchmark window:

```text
for i in {1..12}; do
  go test ./sources -run '^$' \
    -bench '^(BenchmarkFastParseGitLog|BenchmarkFastParseGitLogConstructed)$' \
    -benchmem -benchtime=1s -count=1
done > /tmp/betterleaks-parser-before.txt

benchstat /tmp/betterleaks-parser-before.txt
```

`benchstat` baseline medians/confidence ranges:

| Benchmark | Time | Bytes/op | Allocs/op |
| --- | ---: | ---: | ---: |
| `FastParseGitLog/gitdiff` | 2.013 ms +/- 6% | 2.872 MiB | 25.14k |
| `FastParseGitLog/fast` | 780.9 us +/- 8% | 981.9 KiB | 11.18k |
| `FastParseGitLogConstructed` | 2.636 ms +/- 5% | 2.331 MiB | 38.92k |

The fast-parser process-level samples have a 6.53% CV after two later noisy
samples, so the final comparison must use a material effect size and paired or
interleaved execution rather than claim sensitivity to a tiny microbenchmark
change.

### Profiles and compiled-code inspection

- Fast parser CPU profile (`-benchtime=5s`): 0.816 ms/op. Runtime sleep,
  condition wait, and condition signal dominate flat time; parser `run` is
  23.09% cumulative, `parseFileDiff` 13.76%, channel send 5.28%, and date
  parsing 4.90%.
- Constructed CPU profile: 2.578 ms/op and 313.74 MB/s. Runtime sleep/wait/
  signal again dominate; `parseFileDiff` is 33.55% cumulative, parser `run`
  43.18%, channel send 18.07%, commit header 9.57%, hunk parsing 8.68%, and
  `runtime.IndexByteString` 4.49% flat.
- Constructed block profile records per-file receive/send contention; the test
  harness's final receive is reported separately and is not treated as parser
  evidence.
- Allocation profiles place heap objects at `PatchHeader`, `File`,
  `TextFragment`, `[]gitdiff.Line`, fragment-slice growth, string conversions,
  and builder growth. The 512 KiB reader dominates the short fast benchmark's
  allocation space but not the full scan.
- `go tool objdump -s 'parseFileDiff'` confirms `runtime.newobject`,
  `runtime.slicebytetostring`, write barriers, `runtime.growslice`, and
  `runtime.chansend1` in shipped arm64 code.
- `-d=ssa/check_bce/debug=1` reports many retained index/slice checks throughout
  the parser. This is follow-up evidence, not the primary design driver.

### End-to-end baseline

The binary was built once, and each timing is a fresh process scanning the live
repository with JSON report output redirected away from the timed terminal.
The first run is kept as a cold observation and excluded from the warm mean.

| Mode | Warm runs | Mean wall time | SD | CV |
| --- | ---: | ---: | ---: | ---: |
| Default (`--git-workers=0`, 10 workers) | 9 | 3.7178 s | 0.0406 s | 1.09% |
| `--git-workers=1` | 9 | 3.8567 s | 0.0877 s | 2.28% |

The default diagnostic profile sampled 75.50% of CPU in external code (the
default detector engine), 3.91% in condition signaling, and only 1.18%
cumulatively in `Git.Fragments` itself. The allocation profile is more useful:
`parseHunk` accounts for 5.75% of cumulative sampled objects and
`Git.Fragments` 16.37%. This caps expected wall-time gains on this small repo
and makes an allocation/object-lifetime improvement more plausible than a
parser instruction tweak.

Default-worker and single-worker JSON reports differ only in finding order.
After `jq -S 'sort_by(.Fingerprint)'`, both have SHA-256 digest:

```text
c700692930eecbba67062f4c22777365f3ced92cd214ae49aa54b2101a2c3b47
```

### Larger-repository end-to-end cross-check

`kingfisher` provides a different shape: 1,031 commits, 42.77 MB scanned, and
923 normalized findings. Twelve alternating baseline/final pairs produced:

| Binary | Mean | Median | SD | CV |
| --- | ---: | ---: | ---: | ---: |
| Baseline | 1.3533 s | 1.290 s | 0.1798 s | 13.29% |
| Final | 1.3208 s | 1.295 s | 0.0570 s | 4.31% |

Within-pair mean change was `-0.0325 s`/`-0.95%`; the exact sign-flip test was
not significant (`p=0.6152`). All 24 reports normalized to
`560bcf306145e01f034fd7e1ea0c2afd0f0b852f470e583ed46d896e94225feb`.
The optimization is therefore end-to-end neutral on this faster larger-repo
shape; the betterleaks-repo gain is not generalized beyond the evidence.

### Final RSS and profile diagnostic

Four clean alternating `time -l` pairs on the betterleaks scan reported mean
peak RSS of 191.8 MiB for baseline and 184.9 MiB for final (`-6.9 MiB`, about
`-3.6%`). Per-mode SD was 7.0 to 7.8 MiB, so this is a diagnostic rather than a
significance claim. It is consistent with the deterministic 4.375 MiB reader-
buffer saving at ten workers plus the smaller native record graph.

The post-change default-scan CPU profile sampled 72.82% in external detector
code. `fastLogParser.run`, `parseFileDiff`, and `parseHunk` were only 0.70%
cumulative, below useful instruction-level optimization resolution in this
profile. The allocation profile still assigns 109.86 MiB (43.02%) flat to
`strings.Builder.Write` and 115.89 MiB cumulative to `parseHunk`; this is
primarily the owned added-text payload required by asynchronous detection.
`scheduleGitFile` is 5.0 MiB flat/17.99 MiB cumulative. The bottleneck has
moved from disposable compatibility objects to required payload ownership and
detector work.

### Final validation commands

```text
go test ./... -count=1 -timeout=300s
go vet ./sources/...
go build ./...

BETTERLEAKS_TEST_PATCH=/tmp/betterleaks-self.patch \
go test ./sources \
  -run '^(TestFastParseMatchesGitdiff|TestFastParseMatchesGitdiffWithRepoLogConfig|TestFastParseSyntheticShapes|TestFastParseMatchesPatchFile|TestFastParseLongLineSpillMatchesGitdiff|TestFastParseNativeHeaderIdentityEdges|TestFastParseNativeRecordsOwnData|TestFastParseGeneratedValidStreams|TestGitFragmentsFastPathMatchesGitdiffPath|TestGitLogCmdDiffFilesChannelCompatibility|TestAsyncFastGitLogBatchesCancellationClosesBlockedProducer|TestGitFragmentsCancelsBatchProducerWithDifferentScanContext|TestConsumeFastBatchesChecksContextBetweenFiles)$' \
  -shuffle=on -count=3 -timeout=180s

BETTERLEAKS_TEST_CORPORA='/Users/ahrav/Projects/go-gitdiff:/Users/ahrav/Projects/aho-corasick-rs:/Users/ahrav/Projects/ahrav-claude-plugins:/Users/ahrav/Projects/go-git' \
BETTERLEAKS_TEST_DATE_FORMATS='default,iso-strict,raw,unix' \
go test ./sources -run '^TestFastParseMatchesGitdiffCorpus$' -count=1

go test -race ./sources \
  -run '^(TestFastParseNativeRecordsOwnData|TestGitFragmentsFastPathMatchesGitdiffPath|TestGitLogCmdDiffFilesChannelCompatibility|TestAsyncFastGitLogBatchesCancellationClosesBlockedProducer|TestGitFragmentsCancelsBatchProducerWithDifferentScanContext|TestConsumeFastBatchesChecksContextBetweenFiles)$' \
  -count=3 -timeout=180s

go test ./sources -run '^$' -fuzz '^FuzzFastParseGitLogRobustness$' -fuzztime=10s
go test ./sources -run '^$' -fuzz '^FuzzFastParseGitLogDifferential$' -fuzztime=10s
go test ./sources -run '^$' -fuzz '^FuzzFastParseGeneratedStreams$' -fuzztime=10s

git diff --check
```

All commands above passed. Full-repository `go vet ./...` was also attempted;
it stops on two unchanged pre-existing findings:

```text
cmd/generate/config/rules/minimax.go:27:9: append with no values
cmd/generate/config/rules/zai.go:34:9: append with no values
```

The changed `sources` packages pass vet, and the complete repository builds.
Linux/amd64 and Windows/amd64 test binaries for `./sources` also compile. An
independent final concurrency review repeated the focused race suite 50 times;
an independent API/compatibility review and a separate final diff review found
no blockers.

## Risk assessment and remaining ideas

- Scope is unchanged: only betterleaks-constructed default `git log -p -U0`
  streams use native records. User `--log-opts` still use `gitdiff.Parse`.
- The exported `NewGitLogCmdContext`/`DiffFilesCh` path still eagerly produces
  `*gitdiff.File` values with its historical shape. The native constructor is
  private and used only by scanner-owned call sites.
- No unsafe code, assembly, CPU-specific code, cgo, or build tags ship. The
  implementation is portable Go and keeps the synthetic >600 KiB spill path.
- A buffered batch may retain a consumer-held, queued, and producer-building
  batch at once. The batch is cancellation-safe; capacity zero was measured
  and rejected because it did not preserve throughput. Peak RSS still moved
  down in the diagnostic sample.
- The shared state machine now has target-specific assignments at header,
  file, and hunk boundaries. Differential/fuzz tests protect both targets,
  but this is the main maintenance surface added by the optimization.
- The hidden deprecated `detect` command intentionally remains on the public
  compatibility parser; the supported `git` command, ParallelGit, and hosted
  source paths use native batches.
- Three inherited edge risks remain outside this change: Git stderr scanning
  still has `bufio.Scanner`'s 64 KiB token limit, separately owned command and
  scan contexts can make `Wait` outlive scan cancellation, and error returns
  do not locally join already scheduled detector tasks. Optimized production
  call sites share one context, and final review found no new regression in
  these areas.
- The measured first-byte file classifier remains a possible follow-up if a
  future profile again makes parser instruction count material. It was not
  composed now because its mixed result was insignificant and the final full
  scan places the parser at only 0.70% cumulative CPU.
- The largest remaining parser allocation is the required owned raw added-text
  string. A future architecture would need detector backpressure plus pooled
  chunk ownership or synchronous fragment consumption to reduce it; simple
  `io.ReadAll` and custom chunk readers were already measured and lost.
- The dominant full-scan opportunity is now outside this parser: external
  detector execution and downstream fragment processing. Any next effort
  should re-profile that boundary rather than continue local parser tuning.

### Tooling notes

- `hyperfine` is not installed on `PATH` or in `$HOME/.cargo/bin`; repeated
  process timings use `/usr/bin/time`.
- The benchmark experiment sizing helper could not run because the local
  Python lacks SciPy. The execution count is therefore justified from observed
  CV and the final comparison will rely on `benchstat`, raw samples, and a
  practically meaningful threshold rather than a claimed precomputed power.
