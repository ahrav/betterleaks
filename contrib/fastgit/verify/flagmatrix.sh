#!/bin/bash
# Differential: vanilla vs patched git across a wide flag matrix.
# Any byte difference is a correctness failure of the xdiff-xxh3 patch.
set -u
A=${WORKDIR:-$(mktemp -d)}
VAN=${VAN:?set VAN=/path/to/vanilla/git}
PAT=${PAT:?set PAT=/path/to/patched/git}
FULL=${FULL:-}   # optional full-recipe binary
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
FAIL=0

run_pair() {
  local repo=$1; shift
  local tag=$1; shift
  "$VAN" -C "$repo" "$@" > "$A/fm.van.out" 2>"$A/fm.van.err"; local rv=$?
  "$PAT" -C "$repo" "$@" > "$A/fm.pat.out" 2>"$A/fm.pat.err"; local rp=$?
  if [ $rv -ne $rp ] || ! cmp -s "$A/fm.van.out" "$A/fm.pat.out" || ! cmp -s "$A/fm.van.err" "$A/fm.pat.err"; then
    echo "FAIL[$tag] exit:$rv/$rp $*"
    cmp "$A/fm.van.out" "$A/fm.pat.out" 2>&1 | head -2
    FAIL=1
  else
    echo "ok[$tag] $(wc -c < "$A/fm.van.out")B $*"
  fi
}

P=${1:?usage: flagmatrix.sh /path/to/corpus-repo [second-repo]}
G=${2:-$P}

# --- diff algorithms x log -p on prometheus (full history) ---
for alg in myers minimal patience histogram; do
  run_pair $P "alg-$alg" log -p -U0 --full-history --all --diff-filter=tuxdb --diff-algorithm=$alg
done

# --- context sizes ---
for u in 1 3 10; do
  run_pair $P "ctx-U$u" log -p -U$u --full-history --all
done

# --- whitespace flags (route through xdl_hash_record_with_whitespace: must be untouched, but verify) ---
for wf in -w -b --ignore-space-at-eol --ignore-cr-at-eol --ignore-blank-lines; do
  run_pair $P "ws$wf" log -p -U0 --full-history --all $wf
done

# --- rename/copy detection, stats, formats ---
run_pair $P "find-renames" log -p -M -C --full-history --all
run_pair $P "stat" log --stat --full-history --all
run_pair $P "numstat" log --numstat --full-history --all
run_pair $P "shortstat" log --shortstat --full-history --all
run_pair $P "raw" log --raw --full-history --all
run_pair $P "word-diff" log -p --word-diff=plain --full-history --all
run_pair $P "word-diff-color" log -p --word-diff=color --full-history --all
run_pair $P "color-moved" log -p --color=always --color-moved=zebra --full-history --all
run_pair $P "color-moved-ws" log -p --color=always --color-moved=blocks --color-moved-ws=allow-indentation-change --full-history --all
run_pair $P "function-context" log -p -W --full-history --all
run_pair $P "indent-heuristic" log -p --indent-heuristic --full-history --all
run_pair $P "no-indent-heuristic" log -p --no-indent-heuristic --full-history --all
run_pair $P "anchored" log -p --anchored=func --full-history --all
run_pair $P "merges-m" log -p -m --first-parent
run_pair $P "merges-cc" log -p --cc --full-history --all
run_pair $P "dirstat" log --dirstat --full-history --all
run_pair $P "patience-U3" log -p -U3 --diff-algorithm=patience --full-history --all
run_pair $P "histogram-U3" log -p -U3 --diff-algorithm=histogram --full-history --all

# --- format-patch / show / range-diff style paths ---
run_pair $P "format-patch" format-patch --stdout -50
run_pair $P "show-head" show HEAD
# blame exercises xdiff move detection heavily
for f in $("$VAN" -C $P ls-files | head -5); do
  run_pair $P "blame-$f" blame -M -C HEAD -- "$f"
done

# --- merge-tree (in-core merge => xdl_merge => xdl_recmatch paths) ---
mapfile -t merges < <("$VAN" -C $P rev-list --merges HEAD | head -20)
for m in "${merges[@]}"; do
  p1=$("$VAN" -C $P rev-parse $m^1); p2=$("$VAN" -C $P rev-parse $m^2)
  run_pair $P "merge-tree-$m" merge-tree --write-tree --allow-unrelated-histories $p1 $p2
done

# --- gitlab-foss subset (big-repo classifier growth paths) ---
run_pair $G "gl-alg-histogram" log -p -U0 --diff-filter=tuxdb --diff-algorithm=histogram -n 3000
run_pair $G "gl-alg-patience" log -p -U0 --diff-filter=tuxdb --diff-algorithm=patience -n 3000
run_pair $G "gl-alg-minimal" log -p -U0 --diff-filter=tuxdb --diff-algorithm=minimal -n 1000
run_pair $G "gl-renames" log -p -M -C -n 2000
run_pair $G "gl-word-diff" log -p --word-diff=porcelain -n 2000
run_pair $G "gl-color-moved" log -p --color=always --color-moved=dimmed-zebra -n 2000

# --- full-recipe binary spot checks (LTO+PGO+zlib-ng stack on top of patch) ---
if [ -x "$FULL" ]; then
  PAT=$FULL
  run_pair $P "fullrecipe-default" log -p -U0 --full-history --all --diff-filter=tuxdb
  run_pair $P "fullrecipe-histogram" log -p --diff-algorithm=histogram --full-history --all
  run_pair $G "fullrecipe-gl-3000" log -p -U0 --diff-filter=tuxdb -n 3000
fi

echo "MATRIX-DONE FAIL=$FAIL"
