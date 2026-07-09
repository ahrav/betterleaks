# fastgit — building a scan-optimized `git` for betterleaks log mode

Log mode spends ~80% of its CPU inside `git log -p` subprocesses. A custom
git build cuts that git-side CPU by **~29%** with **byte-identical** output
(verified by `cmp` of full `log -p -U0 --diff-filter=tuxdb` streams on
prometheus and gitlab-foss corpora, plus findings-digest parity).

Point betterleaks at the custom binary with:

```sh
export BETTERLEAKS_GIT_BIN=/path/to/fastgit/git
```

No LD_PRELOAD is needed (zlib-ng is statically linked in). The stock system
git keeps working as the default whenever the env var is unset.

## What's in the build

| Layer | Change | git-side CPU |
|---|---|---|
| compiler | `-O3 -flto=auto` (system builds are `-O2`, no LTO) | −5% |
| zlib | statically linked [zlib-ng](https://github.com/zlib-ng/zlib-ng) in compat mode (NEON inflate) | −12% |
| PGO | profile from a whole-history `log -p -U0` run | −1% |
| xdiff | `xdiff-xxh3.patch`: `memchr` + XXH3 line hashing (below) | −15% |

## The xdiff patch

`xdl_hash_record` hashes every line of both blob versions with DJB2 — a
serial 2-ops-per-byte loop that made `xdl_prepare_ctx` the single hottest
function in whole-history scans (36% of git CPU, more than zlib inflate).

The patch replaces it with a vectorized `memchr` newline scan plus XXH3 line
hashing. On AArch64 it forces XXH3's scalar backend because the GCC NEON backend
uses intentionally unaligned 128-bit vector loads that are hardware-safe on the
target but trip UBSAN; other architectures keep XXH3's normal SIMD selection.
This is output-safe because the line hash is only an internal bucketing key:
every classifier hit is verified with `xdl_recmatch` (content equality) and the
diff machinery consumes canonical class indices, never raw hash values. Any
deterministic content hash produces byte-identical diffs as long as equal byte
strings produce equal hashes; collisions only cost time.

Audit evidence (2026-07-09 byte-exactness audit): `cmp`-identical
vanilla-vs-patched output on full prometheus (20k commits, 424 MB) and
gitlab-foss (197k commits, 7.4 GB) histories under a ~70-configuration flag
matrix (all four diff algorithms, every whitespace flag, `--color-moved`
variants, word-diff, `-W`, `--anchored`, `-m`/`--cc`, blame, merge-tree,
merge-file, range-diff, patch-id, `apply --3way`, staged diffs, textconv/CRLF
attrs, `--follow`, pickaxe); 1000 randomized fuzz repos incl. NUL/CRLF/
full-byte-range content; a degenerate build whose line hash returns constant 0
(worst-case collisions) still `cmp`-identical — empirical proof output cannot
depend on the hash function; a 38.7M-case harness proving cursor-advancement
and hashed-range equivalence incl. mmap guard-page adjacency; ASan+UBSan clean
over full prometheus history; git's own test suite (30k+ tests) passing.

## Build recipe

```sh
git clone --depth 1 --branch v2.50.1 https://github.com/git/git git-src
cd git-src

# static zlib-ng (compat mode)
git clone --depth 1 https://github.com/zlib-ng/zlib-ng
cmake -S zlib-ng -B zlib-ng/build -DZLIB_COMPAT=ON -DCMAKE_BUILD_TYPE=Release \
      -DZLIB_ENABLE_TESTS=OFF -DBUILD_SHARED_LIBS=OFF
cmake --build zlib-ng/build -j
mkdir -p zng/include zng/lib
cp zlib-ng/build/libz.a zng/lib/
cp zlib-ng/build/{zlib.h,zconf.h,zlib_name_mangling.h} zng/include/

# xdiff patch (from this directory)
curl -sLo xdiff/xxhash.h https://raw.githubusercontent.com/Cyan4973/xxHash/v0.8.2/xxhash.h
echo "be275e9db21a503c37f24683cdb4908f2370a3e35ab96e02c4ea73dc8e399c43  xdiff/xxhash.h" | sha256sum -c
patch -p1 < xdiff-xxh3.patch

# GIT_VERSION pins the version string; without it a git built from a patched
# clone reports "2.50.1.dirty", which leaks into format-patch signature
# trailers (the only output that embeds it — log/diff/blame streams do not).
FLAGS="NO_CURL=1 NO_GETTEXT=1 NO_TCLTK=1 NO_PERL=1 NO_PYTHON=1 NO_EXPAT=1 ZLIB_PATH=$PWD/zng GIT_VERSION=2.50.1"

# pass 1: instrumented
make -j CFLAGS="-O3 -fprofile-generate -I$PWD/zng/include" \
        LDFLAGS="-fprofile-generate -L$PWD/zng/lib" $FLAGS git
# representative profile: whole-history log -p on a real repo
./git -C /path/to/big/repo log -p -U0 --full-history --all --diff-filter=tuxdb >/dev/null

# pass 2: optimized (git's Makefile tracks CFLAGS in GIT-CFLAGS, so the flag
# change alone forces a full rebuild — no `make clean` needed between passes)
make -j CFLAGS="-O3 -flto=auto -fprofile-use -fprofile-correction -Wno-missing-profile -Wno-error -I$PWD/zng/include" \
        LDFLAGS="-flto=auto -fprofile-use -L$PWD/zng/lib" $FLAGS git
```

`NO_CURL` disables remote-over-http helpers; scanning only needs local
plumbing. Verify parity on your corpus before deploying:

```sh
git -C repo log -p -U0 --full-history --all --diff-filter=tuxdb > a.txt
./git-src/git -C repo log -p -U0 --full-history --all --diff-filter=tuxdb > b.txt
cmp a.txt b.txt
```

## Measured results (32-core Graviton, benchcpu2 harness)

| Corpus | stock git | custom git | Δ |
|---|---|---|---|
| prometheus (20k commits), single process user-s | 16.6 | 11.9 | −29% |
| B-med whole-app CPU-s | 43.2 | 33.7 | −22% |
| B-large (gitlab-foss 117k) whole-app CPU-s | 1518 | see results log | |
