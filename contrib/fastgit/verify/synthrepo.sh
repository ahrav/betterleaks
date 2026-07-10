#!/bin/bash
# Adversarial synthetic-repo differential: vanilla vs patched git.
# Builds a repo full of xdiff edge cases, then compares output of every
# diff-flavored command byte-for-byte.
set -u
A=${WORKDIR:-$(mktemp -d)}
VAN=${VAN:?set VAN=/path/to/vanilla/git}
PAT=${PAT:?set PAT=/path/to/patched/git}
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
export GIT_AUTHOR_DATE='2000-01-01T00:00:00Z' GIT_COMMITTER_DATE='2000-01-01T00:00:00Z'
R=$A/synthrepo
rm -rf "$R"; mkdir -p "$R"
FAIL=0

g() { git -C "$R" "$@"; }   # repo construction uses system git (neutral)

commit() { g add -A >/dev/null && g commit -qm "$1" >/dev/null; }

g init -q
g config core.autocrlf false

# --- commit 1: baseline files ---
printf 'alpha\nbeta\ngamma\n' > "$R/plain.txt"
printf 'no trailing newline' > "$R/notrail.txt"
printf '\n\n\n\n' > "$R/blanks.txt"
printf 'a\r\nb\r\nc\r\n' > "$R/crlf.txt"
printf 'x' > "$R/onechar.txt"
: > "$R/empty.txt"
printf 'line with \xc3\xa9 unicode\nsecond \xe4\xb8\xad\xe6\x96\x87 line\n' > "$R/utf8.txt"
printf 'bad \xff\xfe utf8\n' > "$R/badutf8.txt"
head -c 100000 /dev/zero | tr '\0' 'A' > "$R/hugeline.txt"; printf '\n' >> "$R/hugeline.txt"
seq 1 50000 > "$R/manylines.txt"
printf 'text with \x00 embedded nul\nline2\n' > "$R/hasnul.bin"
mkdir -p "$R/unicode \xc3\xbc dir"; printf 'in unicode dir\n' > "$R/unicode \xc3\xbc dir/f.txt"
commit c1

# --- commit 2: mutations exercising every hash/classify path ---
printf 'alpha\nBETA\ngamma\ndelta\n' > "$R/plain.txt"                 # middle change + append
printf 'no trailing newline CHANGED' > "$R/notrail.txt"               # incomplete-line edit
printf '\n\nX\n\n\n' > "$R/blanks.txt"                                # blank-line churn
printf 'a\r\nB\r\nc\r\nd\r\n' > "$R/crlf.txt"                         # CRLF edit
printf 'y\nz' > "$R/onechar.txt"                                      # grow + incomplete
printf 'now nonempty\n' > "$R/empty.txt"
printf 'line with \xc3\xa9 unicode\nsecond line changed\n' > "$R/utf8.txt"
rm "$R/badutf8.txt"
sed -i 's/25000/CHANGED/' "$R/manylines.txt"                          # single line in 50k
printf 'text with \x00 embedded nul\nline2 changed\n' > "$R/hasnul.bin"
git -C "$R" mv "unicode \xc3\xbc dir/f.txt" renamed.txt 2>/dev/null || { mv "$R/unicode \xc3\xbc dir/f.txt" "$R/renamed.txt"; }
commit c2

# --- commit 3: identical-content dedup stress (classifier heavy) ---
{ for i in $(seq 1 5000); do printf 'dup\n'; done; printf 'tail\n'; } > "$R/dups.txt"
commit c3
{ for i in $(seq 1 4999); do printf 'dup\n'; done; printf 'mid\n'; for i in $(seq 1 4999); do printf 'dup\n'; done; } > "$R/dups.txt"
commit c4

# --- commit 5: whitespace-sensitive content (for -w/-b differentials) ---
printf 'int  main( )\t{\nreturn   0;\n}\n' > "$R/ws.c"
commit c5
printf 'int main()  {\n  return 0;\n}\n' > "$R/ws.c"
commit c6

# --- commit 7: file becoming empty, empty becoming file, mode change ---
: > "$R/plain.txt"
chmod +x "$R/onechar.txt"
commit c7

# --- merge commit with conflict-ish content ---
g checkout -qb side HEAD~3
printf 'side branch line\nbeta\ngamma\n' > "$R/plain.txt"
commit side1
g checkout -q main 2>/dev/null || g checkout -q master
g merge -q --no-edit -X ours side >/dev/null 2>&1 || true
g add -A >/dev/null 2>&1; g commit -qm merge --allow-empty >/dev/null 2>&1

run_pair() {
  local tag=$1; shift
  "$VAN" -C "$R" "$@" > "$A/sr.van.out" 2>"$A/sr.van.err"; local rv=$?
  "$PAT" -C "$R" "$@" > "$A/sr.pat.out" 2>"$A/sr.pat.err"; local rp=$?
  if [ $rv -ne $rp ] || ! cmp -s "$A/sr.van.out" "$A/sr.pat.out" || ! cmp -s "$A/sr.van.err" "$A/sr.pat.err"; then
    echo "FAIL[$tag] exit:$rv/$rp $*"; FAIL=1
  else
    echo "ok[$tag] $(wc -c < "$A/sr.van.out")B"
  fi
}

# every diff-flavored command, all algorithms, all whitespace modes
for alg in myers minimal patience histogram; do
  run_pair "log-$alg" log -p -U0 --full-history --all --diff-algorithm=$alg
  run_pair "log-$alg-U3" log -p --full-history --all --diff-algorithm=$alg
done
for wf in -w -b --ignore-space-at-eol --ignore-cr-at-eol --ignore-blank-lines; do
  run_pair "ws$wf" log -p --full-history --all $wf
  run_pair "ws$wf-hist" log -p --full-history --all $wf --diff-algorithm=histogram
done
run_pair binary log -p --full-history --all --text
run_pair binary-bin log -p --full-history --all --binary
run_pair renames log -p -M20% -C --full-history --all
run_pair word log -p --word-diff=porcelain --full-history --all
run_pair moved log -p --color=always --color-moved=blocks --full-history --all
run_pair movedws log -p --color=always --color-moved=zebra --color-moved-ws=ignore-all-space --full-history --all
run_pair stat log --stat --full-history --all
run_pair patchid log -p --full-history --all
run_pair fmtpatch format-patch --stdout --root
run_pair mergediff log -p -m --full-history --all
run_pair ccdiff log -p --cc --full-history --all
run_pair remerge log -p --remerge-diff --full-history --all
run_pair blame1 blame HEAD -- manylines.txt
run_pair blame2 blame -M -C HEAD -- plain.txt
run_pair difftree diff-tree -p -r --root HEAD
run_pair wsq log -p --ws-error-highlight=all --full-history --all
run_pair funcctx log -p -W --full-history --all
run_pair irev diff HEAD~4 HEAD
run_pair irev-w diff -w HEAD~4 HEAD
run_pair irev-patience diff --diff-algorithm=patience HEAD~4 HEAD

echo "SYNTH-DONE FAIL=$FAIL"
