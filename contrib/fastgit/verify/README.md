# fastgit verification harness

Tools used by the 2026-07-09 byte-exactness audit of `xdiff-xxh3.patch`.
Run them against a *vanilla* and a *patched* git binary built from the same
tag (pin `GIT_VERSION` so version strings match; see the build recipe).

| Tool | What it proves |
|---|---|
| `hash_semantics.c` | The rewritten `xdl_hash_record` fast path advances the cursor and hashes exactly the same byte range as the DJB2 original — 38.7M cases: exhaustive tiny buffers, CRLF/NUL/1MB-line adversarial cases, random fuzz, equal-content ⇒ equal-hash, and an mmap guard-page sweep (no SIMD overread past an unmapped page). |
| `synthrepo.sh` | Byte-identical output on a synthetic repo of xdiff edge cases (no trailing newline, NULs, CRLF, unicode paths, 50k-line files, duplicate-line stress, merges, mode changes) across ~40 diff-flavored commands. |
| `flagmatrix.sh` | Byte-identical output on real corpora across the full flag matrix: all four diff algorithms, every whitespace flag, color-moved variants, word-diff, `-W`, `--anchored`, `-m`/`--cc`, blame, merge-tree, format-patch. |
| `fuzzrepo.py` | Randomized repo histories (newline-dense/binary-ish/full-byte-range alphabets, mutations, merges) × 13 diff commands, compared vanilla-vs-patched. |

```sh
# one-shot semantics harness (point -I at the xxhash.h the recipe curls)
cc -O2 -Wall -Wextra -I/path/to/dir-with-xxhash.h -o hash_sem hash_semantics.c && ./hash_sem

# differentials (VAN/PAT = vanilla/patched git binaries)
VAN=/path/vanilla/git PAT=/path/patched/git ./synthrepo.sh
VAN=/path/vanilla/git PAT=/path/patched/git ./flagmatrix.sh /path/to/corpus-repo
VAN=/path/vanilla/git PAT=/path/patched/git python3 fuzzrepo.py 0 200
```

The audit also ran two checks not scripted here: a build whose line hash
returns constant 0 (worst-case collisions) — still byte-identical, proving
output cannot depend on the hash function — and an ASan+UBSan build over a
full 20k-commit history (clean). To repeat the const-hash check, replace the
`XXH3_64bits(...)` return in the patched `xdl_hash_record` with `return 0;`
and rerun any differential above.
