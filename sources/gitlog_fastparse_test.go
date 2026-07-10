package sources

import (
	"bytes"
	"fmt"
	"maps"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/fatih/semgroup"
	"github.com/gitleaks/go-gitdiff/gitdiff"

	"github.com/betterleaks/betterleaks/sources/scm"
)

// consumedView is the projection of a *gitdiff.File that betterleaks
// actually reads (sources/git.go). Two parsers are equivalent iff they
// produce identical consumedViews in identical order.
type consumedView struct {
	NewName  string
	IsDelete bool
	IsBinary bool

	SHA         string
	Message     string
	AuthorName  string
	AuthorEmail string
	AuthorDate  string // RFC3339 or "" (zero), as git.go formats it

	Fragments []fragView
}

type fragView struct {
	NewPosition int64
	RawAdd      string
}

func viewOf(f *gitdiff.File) consumedView {
	v := consumedView{
		NewName:  f.NewName,
		IsDelete: f.IsDelete,
		IsBinary: f.IsBinary,
	}
	if f.PatchHeader != nil {
		v.SHA = f.PatchHeader.SHA
		v.Message = f.PatchHeader.Message()
		if f.PatchHeader.Author != nil {
			v.AuthorName = f.PatchHeader.Author.Name
			v.AuthorEmail = f.PatchHeader.Author.Email
		}
		if !f.PatchHeader.AuthorDate.IsZero() {
			v.AuthorDate = f.PatchHeader.AuthorDate.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
	}
	for _, tf := range f.TextFragments {
		if tf == nil {
			continue
		}
		v.Fragments = append(v.Fragments, fragView{
			NewPosition: tf.NewPosition,
			RawAdd:      tf.Raw(gitdiff.OpAdd),
		})
	}
	return v
}

func viewOfFast(f fastGitFile) consumedView {
	v := consumedView{
		NewName:  f.newName,
		IsDelete: f.isDelete,
		IsBinary: f.isBinary,
	}
	if f.header != nil {
		v.SHA = f.header.sha
		v.Message = f.header.message
		if f.header.author.valid {
			v.AuthorName = f.header.author.name
			v.AuthorEmail = f.header.author.email
		}
		if !f.header.authorDate.IsZero() {
			v.AuthorDate = f.header.authorDate.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
	}
	for _, fragment := range f.fragments {
		v.Fragments = append(v.Fragments, fragView{
			NewPosition: fragment.newPosition,
			RawAdd:      fragment.raw,
		})
	}
	return v
}

func collectViews(t *testing.T, ch <-chan *gitdiff.File) []consumedView {
	t.Helper()
	var out []consumedView
	for f := range ch {
		if f.IsDelete {
			continue // consumer skips deletes before reading anything else
		}
		out = append(out, viewOf(f))
	}
	return out
}

func collectFastViews(t *testing.T, patch []byte) []consumedView {
	t.Helper()
	var out []consumedView
	err := parseFastGitLog(bytes.NewReader(patch), func(f fastGitFile) error {
		if !f.isDelete {
			out = append(out, viewOfFast(f))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("parseFastGitLog: %v", err)
	}
	return out
}

// diffBothParsers runs the same patch bytes through gitdiff.Parse and
// fastParseGitLog and requires identical consumed projections.
func diffBothParsers(t *testing.T, patch []byte, label string) {
	t.Helper()

	refCh, err := gitdiff.Parse(bytes.NewReader(patch))
	if err != nil {
		t.Fatalf("%s: gitdiff.Parse: %v", label, err)
	}
	ref := collectViews(t, refCh)

	fastCh, err := fastParseGitLog(bytes.NewReader(patch))
	if err != nil {
		t.Fatalf("%s: fastParseGitLog: %v", label, err)
	}
	fast := collectViews(t, fastCh)
	direct := collectFastViews(t, patch)

	if len(ref) != len(fast) {
		t.Fatalf("%s: file count mismatch: gitdiff=%d fast=%d", label, len(ref), len(fast))
	}
	if len(ref) != len(direct) {
		t.Fatalf("%s: direct file count mismatch: gitdiff=%d direct=%d", label, len(ref), len(direct))
	}
	for i := range ref {
		if !reflect.DeepEqual(ref[i], fast[i]) {
			t.Errorf("%s: file %d differs:\n gitdiff: %+v\n fast:    %+v", label, i, ref[i], fast[i])
			if i > 3 {
				t.FailNow()
			}
		}
		if !reflect.DeepEqual(ref[i], direct[i]) {
			t.Errorf("%s: direct file %d differs:\n gitdiff: %+v\n direct:  %+v", label, i, ref[i], direct[i])
			if i > 3 {
				t.FailNow()
			}
		}
	}
}

// TestFastParseMatchesGitdiff runs both parsers over this repository's own
// full history patch stream — real headers, renames, binaries, unicode
// paths, no-newline markers — and requires identical consumed output.
func TestFastParseMatchesGitdiff(t *testing.T) {
	if testing.Short() {
		t.Skip("needs git history")
	}
	repo := "../"
	cmd := exec.Command("git", "-C", repo, "log", "-p", "-U0", "--full-history", "--all", "--diff-filter=tuxdb")
	cmd.Env = gitConfigIsolationEnv()
	patch, err := cmd.Output()
	if err != nil {
		t.Skipf("git log failed: %v", err)
	}
	if len(patch) == 0 {
		t.Skip("empty patch stream")
	}
	diffBothParsers(t, patch, "self-history")
}

// TestFastParseMatchesGitdiffCorpus optionally checks external corpora
// pointed to by BETTERLEAKS_TEST_CORPUS or BETTERLEAKS_TEST_CORPORA
// (filepath-list separated). BETTERLEAKS_TEST_DATE_FORMATS can be a
// comma-separated list of git --date formats; use "default" for no flag.
func TestFastParseMatchesGitdiffCorpus(t *testing.T) {
	repos := corpusReposFromEnv()
	if len(repos) == 0 {
		t.Skip("BETTERLEAKS_TEST_CORPUS/BETTERLEAKS_TEST_CORPORA not set")
	}
	dateFormats := corpusDateFormatsFromEnv()

	for _, repo := range repos {
		repo := repo
		for _, dateFormat := range dateFormats {
			dateFormat := dateFormat
			t.Run(corpusTestName(repo, dateFormat), func(t *testing.T) {
				args := []string{"-C", repo, "log", "-p", "-U0", "--full-history", "--all", "--diff-filter=tuxdb"}
				if dateFormat != "default" {
					args = append(args, "--date="+dateFormat)
				}
				cmd := exec.Command("git", args...)
				cmd.Env = gitConfigIsolationEnv()
				patch, err := cmd.Output()
				if err != nil {
					t.Fatalf("git log failed: %v", err)
				}
				diffBothParsers(t, patch, repo+" date="+dateFormat)
			})
		}
	}
}

func corpusReposFromEnv() []string {
	var repos []string
	if list := os.Getenv("BETTERLEAKS_TEST_CORPORA"); list != "" {
		repos = append(repos, filepath.SplitList(list)...)
	}
	if repo := os.Getenv("BETTERLEAKS_TEST_CORPUS"); repo != "" {
		repos = append(repos, repo)
	}

	seen := make(map[string]struct{}, len(repos))
	out := repos[:0]
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		if _, ok := seen[repo]; ok {
			continue
		}
		seen[repo] = struct{}{}
		out = append(out, repo)
	}
	return out
}

func corpusDateFormatsFromEnv() []string {
	list := os.Getenv("BETTERLEAKS_TEST_DATE_FORMATS")
	if list == "" {
		return []string{"default"}
	}
	var formats []string
	for _, part := range strings.Split(list, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			formats = append(formats, part)
		}
	}
	if len(formats) == 0 {
		return []string{"default"}
	}
	return formats
}

func corpusTestName(repo, dateFormat string) string {
	name := filepath.Base(filepath.Clean(repo))
	if name == "." || name == string(filepath.Separator) {
		name = "corpus"
	}
	return name + "/" + dateFormat
}

func TestFastParseMatchesGitdiffWithRepoLogConfig(t *testing.T) {
	repo := t.TempDir()
	runFastParseGit(t, repo, "init")
	runFastParseGit(t, repo, "config", "user.name", "A User")
	runFastParseGit(t, repo, "config", "user.email", "a@example.com")
	runFastParseGit(t, repo, "config", "log.date", "iso-strict")
	runFastParseGit(t, repo, "config", "log.decorate", "true")
	runFastParseGit(t, repo, "config", "core.quotePath", "false")

	path := filepath.Join(repo, "päth with space.txt")
	if err := os.WriteFile(path, []byte("secret\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runFastParseGit(t, repo, "add", ".")
	runFastParseGit(t, repo, "commit", "-m", "configured log")

	cmd := exec.Command("git", "-C", repo, "log", "-p", "-U0", "--full-history", "--all", "--diff-filter=tuxdb")
	cmd.Env = append(gitConfigIsolationEnv(),
		"GIT_AUTHOR_DATE=2026-01-02T15:04:05+00:00",
		"GIT_COMMITTER_DATE=2026-01-02T15:04:05+00:00",
	)
	patch, err := cmd.Output()
	if err != nil {
		t.Fatalf("git log failed: %v", err)
	}
	diffBothParsers(t, patch, "repo-log-config")
}

func runFastParseGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(gitConfigIsolationEnv(),
		"GIT_AUTHOR_DATE=2026-01-02T15:04:05+00:00",
		"GIT_COMMITTER_DATE=2026-01-02T15:04:05+00:00",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// TestFastParseSyntheticShapes exercises tricky stream shapes directly.
func TestFastParseSyntheticShapes(t *testing.T) {
	cases := map[string]string{
		"empty-commit-no-diff":          "commit 1111111111111111111111111111111111111111\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    subject only\n",
		"no-newline-at-eof-add":         "commit 2222222222222222222222222222222222222222\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/f.txt b/f.txt\nindex 0000000..1111111 100644\n--- a/f.txt\n+++ b/f.txt\n@@ -0,0 +1 @@\n+no trailing newline\n\\ No newline at end of file\n",
		"no-newline-old-side":           "commit 3333333333333333333333333333333333333333\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/f.txt b/f.txt\nindex 0000000..1111111 100644\n--- a/f.txt\n+++ b/f.txt\n@@ -1 +1 @@\n-old line\n\\ No newline at end of file\n+new line\n",
		"quoted-unicode-path":           "commit 4444444444444444444444444444444444444444\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git \"a/p\\303\\244th.txt\" \"b/p\\303\\244th.txt\"\nnew file mode 100644\nindex 0000000..1111111\n--- /dev/null\n+++ \"b/p\\303\\244th.txt\"\n@@ -0,0 +1 @@\n+content\n",
		"binary-file":                   "commit 5555555555555555555555555555555555555555\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/img.png b/img.png\nnew file mode 100644\nindex 0000000..1111111\nBinary files /dev/null and b/img.png differ\n",
		"mode-only-change":              "commit 6666666666666666666666666666666666666666\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/script.sh b/script.sh\nold mode 100644\nnew mode 100755\n",
		"multi-hunk":                    "commit 7777777777777777777777777777777777777777\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    title\n    \n    body line 1\n    body line 2\n\ndiff --git a/m.txt b/m.txt\nindex 0000000..1111111 100644\n--- a/m.txt\n+++ b/m.txt\n@@ -1 +1,2 @@\n-a\n+b\n+c\n@@ -10,0 +12 @@ func ctx() {\n+d\n",
		"rename-with-edit":              "commit 8888888888888888888888888888888888888888\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/old.txt b/new.txt\nsimilarity index 90%\nrename from old.txt\nrename to new.txt\nindex 0000000..1111111 100644\n--- a/old.txt\n+++ b/new.txt\n@@ -1 +1 @@\n-x\n+y\n",
		"empty-line-context":            "commit 9999999999999999999999999999999999999999\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/f.txt b/f.txt\nindex 0000000..1111111 100644\n--- a/f.txt\n+++ b/f.txt\n@@ -1,2 +1,2 @@\n\n+added after empty context\n-removed\n",
		"leading-blank-message":         "commit aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    \n    body with no gitdiff title\n\ndiff --git a/f.txt b/f.txt\nindex 0000000..1111111 100644\n--- a/f.txt\n+++ b/f.txt\n@@ -0,0 +1 @@\n+content\n",
		"unicode-title-indent":          "commit bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    \u00a0title\n    \n    \u00a0body line\n\ndiff --git a/f.txt b/f.txt\nindex 0000000..1111111 100644\n--- a/f.txt\n+++ b/f.txt\n@@ -0,0 +1 @@\n+content\n",
		"rename-old-new-keywords":       "commit cccccccccccccccccccccccccccccccccccccccc\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/old.txt b/new.txt\nsimilarity index 90%\nrename old old.txt\nrename new new.txt\nindex 0000000..1111111 100644\n--- a/old.txt\n+++ b/new.txt\n@@ -1 +1 @@\n-x\n+y\n",
		"files-differ-marker":           "commit dddddddddddddddddddddddddddddddddddddddd\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/bin.dat b/bin.dat\nindex 0000000..1111111 100644\nFiles differ\n",
		"unquoted-space-path-mode-only": "commit eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/dir/space name.txt b/dir/space name.txt\nold mode 100644\nnew mode 100755\n",
		"iso-date-format":               "commit 1010101010101010101010101010101010101010\nAuthor: A <a@x>\nDate:   2026-01-02 15:04:05 +0000\n\n    msg\n\ndiff --git a/date.txt b/date.txt\nindex 0000000..1111111 100644\n--- a/date.txt\n+++ b/date.txt\n@@ -0,0 +1 @@\n+content\n",
		"raw-date-format":               "commit 2020202020202020202020202020202020202020\nAuthor: A <a@x>\nDate:   1767366245 +0000\n\n    msg\n\ndiff --git a/date.txt b/date.txt\nindex 0000000..1111111 100644\n--- a/date.txt\n+++ b/date.txt\n@@ -0,0 +1 @@\n+content\n",
		"copy-with-edit":                "commit 3030303030303030303030303030303030303030\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/base.txt b/copy.txt\nsimilarity index 80%\ncopy from base.txt\ncopy to copy.txt\nindex 0000000..1111111 100644\n--- a/base.txt\n+++ b/copy.txt\n@@ -1 +1 @@\n-base\n+copy\n",
		"deleted-file-with-hunk":        "commit 4040404040404040404040404040404040404040\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/deleted.txt b/deleted.txt\ndeleted file mode 100644\nindex 1111111..0000000\n--- a/deleted.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-gone\n",
		"empty-add-line-and-crlf":       "commit 5050505050505050505050505050505050505050\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/crlf.txt b/crlf.txt\nindex 0000000..1111111 100644\n--- a/crlf.txt\n+++ b/crlf.txt\n@@ -0,0 +1,2 @@\n+\r\n+\n",
		"eof-without-final-newline":     "commit 6060606060606060606060606060606060606060\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/eof.txt b/eof.txt\nindex 0000000..1111111 100644\n--- a/eof.txt\n+++ b/eof.txt\n@@ -0,0 +1 @@\n+tail",
		"staged-diff-no-commit-header":  "diff --git a/staged.txt b/staged.txt\nindex 0000000..1111111 100644\n--- a/staged.txt\n+++ b/staged.txt\n@@ -0,0 +1 @@\n+content\n",
		"invalid-date-nils-header":      "commit 7070707070707070707070707070707070707070\nAuthor: A <a@x>\nDate:   2026-01-02T15:04:05Z\n\n    msg\n\ndiff --git a/invalid-date.txt b/invalid-date.txt\nindex 0000000..1111111 100644\n--- a/invalid-date.txt\n+++ b/invalid-date.txt\n@@ -0,0 +1 @@\n+content\n",
		"invalid-identity-nils-header":  "commit 8080808080808080808080808080808080808080\nAuthor: A <unterminated\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/invalid-ident.txt b/invalid-ident.txt\nindex 0000000..1111111 100644\n--- a/invalid-ident.txt\n+++ b/invalid-ident.txt\n@@ -0,0 +1 @@\n+content\n",
		"plus-name-with-tab-timestamp":  "commit 9090909090909090909090909090909090909090\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/timestamp.txt b/timestamp.txt\nindex 0000000..1111111 100644\n--- a/timestamp.txt\t2026-01-02 15:04:05 +0000\n+++ b/timestamp.txt\t2026-01-02 15:04:05 +0000\n@@ -0,0 +1 @@\n+content\n",
		"empty-new-file":                "commit a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/empty.txt b/empty.txt\nnew file mode 100644\nindex 0000000..e69de29\n",
		"empty-deleted-file":            "commit b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/empty-deleted.txt b/empty-deleted.txt\ndeleted file mode 100644\nindex e69de29..0000000\n",
	}
	for name, patch := range cases {
		t.Run(name, func(t *testing.T) {
			diffBothParsers(t, []byte(patch), name)
		})
	}
}

// TestFastParseSmallWindows forces hunks and lines to straddle window
// refills (and spill entirely) by shrinking the read window far below
// line and hunk sizes, then requires identical consumed output on BOTH
// the native and compat paths. This is the aliasing torture test for the
// window reader + arena accumulation: window bytes are only valid until
// the next refill, so payload copies must happen before compaction/spill.
func TestFastParseSmallWindows(t *testing.T) {
	windows := []int{16, 37, 64, 253, 4096}

	check := func(t *testing.T, patch []byte, label string) {
		t.Helper()
		refCh, err := gitdiff.Parse(bytes.NewReader(patch))
		if err != nil {
			t.Fatalf("%s: gitdiff.Parse: %v", label, err)
		}
		ref := collectViews(t, refCh)
		for _, w := range windows {
			var native []consumedView
			err := parseFastGitLogSized(bytes.NewReader(patch), w, func(f fastGitFile) error {
				if !f.isDelete {
					native = append(native, viewOfFast(f))
				}
				return nil
			})
			if err != nil {
				t.Fatalf("%s(win=%d): native: %v", label, w, err)
			}
			var compat []consumedView
			err = parseFastGitLogCompatSized(bytes.NewReader(patch), w, func(f *gitdiff.File) error {
				if !f.IsDelete {
					compat = append(compat, viewOf(f))
				}
				return nil
			})
			if err != nil {
				t.Fatalf("%s(win=%d): compat: %v", label, w, err)
			}
			if len(ref) != len(native) || len(ref) != len(compat) {
				t.Fatalf("%s(win=%d): file count mismatch: gitdiff=%d native=%d compat=%d",
					label, w, len(ref), len(native), len(compat))
			}
			for i := range ref {
				if !reflect.DeepEqual(ref[i], native[i]) {
					t.Fatalf("%s(win=%d): native file %d differs:\n gitdiff: %+v\n native:  %+v", label, w, i, ref[i], native[i])
				}
				if !reflect.DeepEqual(ref[i], compat[i]) {
					t.Fatalf("%s(win=%d): compat file %d differs:\n gitdiff: %+v\n compat:  %+v", label, w, i, ref[i], compat[i])
				}
			}
		}
	}

	for seed := int64(0); seed < 300; seed++ {
		rng := rand.New(rand.NewSource(seed))
		check(t, generatedPatchStream(rng, 1+rng.Intn(4)), fmt.Sprintf("seed-%d", seed))
	}

	// A hunk much larger than the smallest windows, with long lines.
	var b strings.Builder
	b.WriteString("commit ffffffffffffffffffffffffffffffffffffffff\n")
	b.WriteString("Author: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    big hunk\n\n")
	b.WriteString("diff --git a/big.txt b/big.txt\nindex 0000000..1111111 100644\n--- a/big.txt\n+++ b/big.txt\n")
	fmt.Fprintf(&b, "@@ -0,0 +1,%d @@\n", 200)
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "+line %d %s\n", i, strings.Repeat("x", i%97))
	}
	b.WriteString("\\ No newline at end of file\n")
	check(t, []byte(b.String()), "big-hunk")
}

// TestFastParseMatchesPatchFile checks a pre-captured patch stream file
// (BETTERLEAKS_TEST_PATCH) — used to bisect mismatches on huge corpora.
func TestFastParseMatchesPatchFile(t *testing.T) {
	path := os.Getenv("BETTERLEAKS_TEST_PATCH")
	if path == "" {
		t.Skip("BETTERLEAKS_TEST_PATCH not set")
	}
	patch, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	diffBothParsers(t, patch, "patchfile")
}

func TestFastParseLongLineSpillMatchesGitdiff(t *testing.T) {
	longLine := strings.Repeat("x", 600<<10)
	patch := "commit ffffffffffffffffffffffffffffffffffffffff\n" +
		"Author: A <a@x>\n" +
		"Date:   Mon Jan 2 15:04:05 2026 +0000\n\n" +
		"    long line\n\n" +
		"diff --git a/long.txt b/long.txt\n" +
		"index 0000000..1111111 100644\n" +
		"--- a/long.txt\n" +
		"+++ b/long.txt\n" +
		"@@ -0,0 +1 @@\n" +
		"+" + longLine + "\n"
	diffBothParsers(t, []byte(patch), "long-line-spill")
}

func TestFastParseNativeHeaderIdentityEdges(t *testing.T) {
	cases := map[string]string{
		"invalid-author":    "Author: A <unterminated\nDate:   Mon Jan 2 15:04:05 2026 +0000\n",
		"invalid-committer": "Author: A <a@x>\nCommit: C <unterminated\nAuthorDate: Mon Jan 2 15:04:05 2026 +0000\nCommitDate: Mon Jan 2 15:04:05 2026 +0000\n",
		"missing-angles":    "Author: no-address\nDate:   Mon Jan 2 15:04:05 2026 +0000\n",
		"unicode-identity":  "Author: Å User <å@example.com>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n",
	}
	for name, fields := range cases {
		patch := "commit aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" + fields +
			"\n    title\n\ndiff --git a/file.txt b/file.txt\n--- a/file.txt\n+++ b/file.txt\n@@ -0,0 +1 @@\n+value\n"
		t.Run(name, func(t *testing.T) {
			diffBothParsers(t, []byte(patch), name)
		})
	}
}

func TestFastParseNativeRecordsOwnData(t *testing.T) {
	longA := strings.Repeat("a", 600<<10)
	longB := strings.Repeat("b", 700<<10)
	patch := "commit aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" +
		"Author: First Author <first@example.com>\n" +
		"Date:   Mon Jan 2 15:04:05 2026 +0000\n\n" +
		"    first message\n\n" +
		"diff --git a/first.txt b/first.txt\n" +
		"--- a/first.txt\n+++ b/first.txt\n@@ -0,0 +1 @@\n+" + longA + "\n" +
		"commit bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n" +
		"Author: Second Author <second@example.com>\n" +
		"Date:   Tue Feb 3 16:05:06 2026 +0000\n\n" +
		"    second message\n\n" +
		"diff --git a/second.txt b/second.txt\n" +
		"--- a/second.txt\n+++ b/second.txt\n@@ -0,0 +1 @@\n+" + longB + "\n"

	var files []fastGitFile
	err := parseFastGitLog(strings.NewReader(patch), func(f fastGitFile) error {
		files = append(files, f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("file count = %d, want 2", len(files))
	}

	checks := []struct {
		file      fastGitFile
		wantName  string
		wantSHA   string
		wantAuth  string
		wantRaw   string
		wantStart int64
	}{
		{files[0], "first.txt", strings.Repeat("a", 40), "First Author", longA + "\n", 1},
		{files[1], "second.txt", strings.Repeat("b", 40), "Second Author", longB + "\n", 1},
	}
	for _, check := range checks {
		if check.file.newName != check.wantName {
			t.Errorf("name = %q, want %q", check.file.newName, check.wantName)
		}
		if check.file.header == nil || check.file.header.sha != check.wantSHA {
			t.Errorf("header SHA = %v, want %q", check.file.header, check.wantSHA)
		}
		if check.file.header == nil || !check.file.header.author.valid || check.file.header.author.name != check.wantAuth {
			t.Errorf("author = %v, want %q", check.file.header, check.wantAuth)
		}
		if len(check.file.fragments) != 1 {
			t.Errorf("fragment count = %d, want 1", len(check.file.fragments))
			continue
		}
		if check.file.fragments[0].raw != check.wantRaw {
			t.Errorf("raw data was overwritten: len = %d, want %d", len(check.file.fragments[0].raw), len(check.wantRaw))
		}
		if check.file.fragments[0].newPosition != check.wantStart {
			t.Errorf("new position = %d, want %d", check.file.fragments[0].newPosition, check.wantStart)
		}
	}
}

type gitFragmentView struct {
	raw        string
	startLine  int
	attributes map[string]string
}

func TestGitFragmentsFastPathMatchesGitdiffPath(t *testing.T) {
	repo := t.TempDir()
	runFastParseGit(t, repo, "init")
	runFastParseGit(t, repo, "config", "user.name", "A User")
	runFastParseGit(t, repo, "config", "user.email", "a@example.com")

	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("keep.txt", []byte("base\n"))
	write("skip.txt", []byte("skip base\n"))
	write("binary.dat", []byte{0, 1, 2, 3, 4})
	runFastParseGit(t, repo, "add", ".")
	runFastParseGit(t, repo, "commit", "-m", "initial")

	write("keep.txt", []byte("base\nsecret one\nsecret two\n"))
	write("skip.txt", []byte("skip base\nsecret skipped\n"))
	runFastParseGit(t, repo, "add", ".")
	runFastParseGit(t, repo, "commit", "-m", "update")
	runFastParseGit(t, repo, "rm", "keep.txt")
	runFastParseGit(t, repo, "commit", "-m", "delete")

	ctx := t.Context()
	generalCmd, err := newGitLogScanCmdContext(ctx, repo, "--full-history --all --diff-filter=tuxdb")
	if err != nil {
		t.Fatal(err)
	}
	general := collectGitFragmentViews(t, &Git{
		Cmd:        generalCmd,
		ShouldSkip: func(attrs map[string]string) bool { return attrs[AttrPath] == "skip.txt" },
		Platform:   scm.GitHubPlatform,
		RemoteURL:  "https://example.com/acme/repo",
		Sema:       semgroup.NewGroup(ctx, 4),
	})

	cmd, err := newGitLogScanCmdContext(ctx, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	got := collectGitFragmentViews(t, &Git{
		Cmd:        cmd,
		ShouldSkip: func(attrs map[string]string) bool { return attrs[AttrPath] == "skip.txt" },
		Platform:   scm.GitHubPlatform,
		RemoteURL:  "https://example.com/acme/repo",
		Sema:       semgroup.NewGroup(ctx, 4),
	})
	if !reflect.DeepEqual(got, general) {
		t.Fatalf("native fragments differ from gitdiff path:\n got: %#v\n gitdiff: %#v", got, general)
	}
}

func TestGitLogCmdDiffFilesChannelCompatibility(t *testing.T) {
	repo := t.TempDir()
	runFastParseGit(t, repo, "init")
	runFastParseGit(t, repo, "config", "user.name", "A User")
	runFastParseGit(t, repo, "config", "user.email", "a@example.com")
	for i := range 70 {
		name := filepath.Join(repo, fmt.Sprintf("file-%02d.txt", i))
		if err := os.WriteFile(name, []byte("content\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	runFastParseGit(t, repo, "add", ".")
	runFastParseGit(t, repo, "commit", "-m", "many files")

	cmd, err := NewGitLogCmdContext(t.Context(), repo, "")
	if err != nil {
		t.Fatal(err)
	}
	first := cmd.DiffFilesCh()
	if second := cmd.DiffFilesCh(); first != second {
		t.Fatal("DiffFilesCh returned a different channel on its second call")
	}
	files := 0
	for range first {
		files++
	}
	if files != 70 {
		t.Fatalf("file count = %d, want 70", files)
	}
	for err := range cmd.ErrCh() {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func collectGitFragmentViews(t *testing.T, source *Git) []gitFragmentView {
	t.Helper()
	var mu sync.Mutex
	var views []gitFragmentView
	err := source.Fragments(t.Context(), func(fragment Fragment, err error) error {
		if err != nil {
			return err
		}
		mu.Lock()
		views = append(views, gitFragmentView{
			raw:        fragment.Raw,
			startLine:  fragment.StartLine,
			attributes: maps.Clone(fragment.Attributes),
		})
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(views, func(i, j int) bool {
		left := views[i].attributes[AttrGitSHA] + "\x00" + views[i].attributes[AttrPath] + "\x00" + strconv.Itoa(views[i].startLine) + "\x00" + views[i].raw
		right := views[j].attributes[AttrGitSHA] + "\x00" + views[j].attributes[AttrPath] + "\x00" + strconv.Itoa(views[j].startLine) + "\x00" + views[j].raw
		return left < right
	})
	return views
}

func TestFastParseGeneratedValidStreams(t *testing.T) {
	const seeds = 5000
	for seed := int64(0); seed < seeds; seed++ {
		rng := rand.New(rand.NewSource(seed))
		patch := generatedPatchStream(rng, 1+rng.Intn(6))
		diffBothParsers(t, patch, fmt.Sprintf("generated-seed-%d", seed))
	}
}

func FuzzFastParseGeneratedStreams(f *testing.F) {
	for _, seed := range []int64{0, 1, 2, 3, 4, 5, 17, 29, 101, 255, 1024, 4096} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, seed int64) {
		rng := rand.New(rand.NewSource(seed))
		patch := generatedPatchStream(rng, 1+rng.Intn(8))
		diffBothParsers(t, patch, fmt.Sprintf("fuzz-seed-%d", seed))
	})
}

var fastParseBenchmarkSink int

func BenchmarkFastParseGitLog(b *testing.B) {
	rng := rand.New(rand.NewSource(0x5eed))
	patch := generatedPatchStream(rng, 300)
	b.SetBytes(int64(len(patch)))

	b.Run("gitdiff", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			ch, err := gitdiff.Parse(bytes.NewReader(patch))
			if err != nil {
				b.Fatal(err)
			}
			fastParseBenchmarkSink += consumeParsedFiles(ch)
		}
	})

	b.Run("fast", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			ch, err := fastParseGitLog(bytes.NewReader(patch))
			if err != nil {
				b.Fatal(err)
			}
			fastParseBenchmarkSink += consumeParsedFiles(ch)
		}
	})

	b.Run("native-sync", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			n := 0
			err := parseFastGitLog(bytes.NewReader(patch), func(f fastGitFile) error {
				n += consumeFastFile(f)
				return nil
			})
			if err != nil {
				b.Fatal(err)
			}
			fastParseBenchmarkSink += n
		}
	})

	b.Run("native-async", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			n := 0
			for f := range asyncFastFiles(patch) {
				n += consumeFastFile(f)
			}
			fastParseBenchmarkSink += n
		}
	})

	for _, batchSize := range []int{4, 16, 64, 128, 256} {
		b.Run(fmt.Sprintf("native-batch-%d", batchSize), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				fastParseBenchmarkSink += consumeFastBatches(asyncFastGitLogBatches(b.Context(), bytes.NewReader(patch), batchSize))
			}
		})
	}

}

func consumeParsedFiles(ch <-chan *gitdiff.File) int {
	n := 0
	for f := range ch {
		if f.IsDelete {
			continue
		}
		n += len(f.NewName)
		if f.PatchHeader != nil {
			n += len(f.PatchHeader.SHA)
			n += len(f.PatchHeader.Message())
		}
		for _, tf := range f.TextFragments {
			if tf != nil {
				n += int(tf.NewPosition)
				n += len(tf.Raw(gitdiff.OpAdd))
			}
		}
	}
	return n
}

func consumeFastFile(f fastGitFile) int {
	if f.isDelete {
		return 0
	}
	n := len(f.newName)
	if f.header != nil {
		n += len(f.header.sha)
		n += len(f.header.message)
	}
	for _, fragment := range f.fragments {
		n += int(fragment.newPosition)
		n += len(fragment.raw)
	}
	return n
}

func asyncFastFiles(patch []byte) <-chan fastGitFile {
	out := make(chan fastGitFile, 64)
	go func() {
		defer close(out)
		_ = parseFastGitLog(bytes.NewReader(patch), func(f fastGitFile) error {
			out <- f
			return nil
		})
	}()
	return out
}

func consumeFastBatches(ch <-chan []fastGitFile) int {
	n := 0
	for batch := range ch {
		for _, f := range batch {
			n += consumeFastFile(f)
		}
	}
	return n
}

func generatedPatchStream(rng *rand.Rand, commits int) []byte {
	var b strings.Builder
	for i := 0; i < commits; i++ {
		b.WriteString(generatedCommitHeader(rng, i))
		files := rng.Intn(5)
		for j := 0; j < files; j++ {
			b.WriteString(generatedFileDiff(rng, i, j))
		}
	}
	return []byte(b.String())
}

func generatedCommitHeader(rng *rand.Rand, idx int) string {
	sha := fmt.Sprintf("%040x", idx+1)
	dates := []string{
		"Mon Jan 2 15:04:05 2026 +0000",
		"2026-01-02 15:04:05 +0000",
		"2026-01-02T15:04:05+00:00",
		"Fri, 2 Jan 2026 15:04:05 +0000",
		"2026-01-02",
		"1767366245",
		"1767366245 +0000",
	}
	date := dates[rng.Intn(len(dates))]
	var b strings.Builder
	fmt.Fprintf(&b, "commit %s\n", sha)
	switch rng.Intn(3) {
	case 0:
		b.WriteString("Author: A User <a@example.com>\n")
		fmt.Fprintf(&b, "Date:   %s\n", date)
	case 1:
		b.WriteString("Author: A User <a@example.com>\n")
		b.WriteString("Commit: C User <c@example.com>\n")
		fmt.Fprintf(&b, "AuthorDate: %s\n", date)
		fmt.Fprintf(&b, "CommitDate: %s\n", date)
	default:
		b.WriteString("Author: Space Name <space@example.com>\n")
		fmt.Fprintf(&b, "Date:   %s\n", date)
	}
	b.WriteByte('\n')
	switch rng.Intn(6) {
	case 0:
		b.WriteString("    generated title\n")
	case 1:
		b.WriteString("    generated title\n    continued title\n")
	case 2:
		b.WriteString("    generated title\n    \n    body one\n    \n    \n    body two\n")
	case 3:
		b.WriteString("    \n    body without title\n")
	case 4:
		b.WriteString("    \u00a0unicode title\n    \n    \u00a0unicode body\n")
	default:
		b.WriteString("    trailing whitespace title   \n    \n    body with trailing whitespace\t \n")
	}
	return b.String()
}

func generatedFileDiff(rng *rand.Rand, commitIdx, fileIdx int) string {
	base := generatedPath(rng, commitIdx, fileIdx)
	next := base
	if rng.Intn(5) == 0 {
		next = generatedPath(rng, commitIdx+17, fileIdx+31)
	}
	switch rng.Intn(8) {
	case 0:
		return generatedModeOnlyDiff(base)
	case 1:
		return generatedBinaryDiff(base, rng.Intn(3))
	case 2:
		return generatedRenameDiff(base, next, rng.Intn(2) == 0)
	case 3:
		return generatedCopyDiff(base, next)
	case 4:
		return generatedNewFileDiff(base, rng)
	default:
		return generatedTextDiff(base, rng)
	}
}

func generatedPath(rng *rand.Rand, commitIdx, fileIdx int) string {
	paths := []string{
		fmt.Sprintf("file-%d-%d.txt", commitIdx, fileIdx),
		fmt.Sprintf("dir/space name %d %d.txt", commitIdx, fileIdx),
		fmt.Sprintf("dir/tab-%d-%d.txt", commitIdx, fileIdx),
		fmt.Sprintf("unicode/p\u00e4th-%d-%d.txt", commitIdx, fileIdx),
		fmt.Sprintf("symbols/[brackets]-%d-%d.go", commitIdx, fileIdx),
	}
	return paths[rng.Intn(len(paths))]
}

func generatedModeOnlyDiff(path string) string {
	return fmt.Sprintf("diff --git %s %s\nold mode 100644\nnew mode 100755\n", gitPath(path, "a"), gitPath(path, "b"))
}

func generatedBinaryDiff(path string, marker int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git %s %s\n", gitPath(path, "a"), gitPath(path, "b"))
	b.WriteString("index 0000000..1111111 100644\n")
	switch marker {
	case 0:
		fmt.Fprintf(&b, "Binary files %s and %s differ\n", gitPath(path, "a"), gitPath(path, "b"))
	case 1:
		b.WriteString("Binary files differ\n")
	default:
		b.WriteString("Files differ\n")
	}
	return b.String()
}

func generatedRenameDiff(oldPath, newPath string, oldKeyword bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git %s %s\n", gitPath(oldPath, "a"), gitPath(newPath, "b"))
	b.WriteString("similarity index 90%\n")
	if oldKeyword {
		fmt.Fprintf(&b, "rename old %s\nrename new %s\n", gitName(oldPath), gitName(newPath))
	} else {
		fmt.Fprintf(&b, "rename from %s\nrename to %s\n", gitName(oldPath), gitName(newPath))
	}
	b.WriteString("index 0000000..1111111 100644\n")
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", gitPath(oldPath, "a"), gitPath(newPath, "b"))
	b.WriteString("@@ -1 +1 @@\n-old\n+new\n")
	return b.String()
}

func generatedCopyDiff(oldPath, newPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git %s %s\n", gitPath(oldPath, "a"), gitPath(newPath, "b"))
	b.WriteString("similarity index 80%\n")
	fmt.Fprintf(&b, "copy from %s\ncopy to %s\n", gitName(oldPath), gitName(newPath))
	b.WriteString("index 0000000..1111111 100644\n")
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", gitPath(oldPath, "a"), gitPath(newPath, "b"))
	b.WriteString("@@ -1 +1 @@\n-old\n+copy\n")
	return b.String()
}

func generatedNewFileDiff(path string, rng *rand.Rand) string {
	lines := 1 + rng.Intn(4)
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git %s %s\n", gitPath(path, "a"), gitPath(path, "b"))
	b.WriteString("new file mode 100644\nindex 0000000..1111111\n--- /dev/null\n")
	fmt.Fprintf(&b, "+++ %s\n@@ -0,0 +1,%d @@\n", gitPath(path, "b"), lines)
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, "+added %d\n", i)
	}
	if rng.Intn(5) == 0 {
		b.WriteString("\\ No newline at end of file\n")
	}
	return b.String()
}

func generatedTextDiff(path string, rng *rand.Rand) string {
	oldLines := 1 + rng.Intn(3)
	newLines := 1 + rng.Intn(4)
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git %s %s\n", gitPath(path, "a"), gitPath(path, "b"))
	b.WriteString("index 0000000..1111111 100644\n")
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", gitPath(path, "a"), gitPath(path, "b"))
	fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@ optional comment\n", 1+rng.Intn(20), oldLines, 1+rng.Intn(20), newLines)
	for i := 0; i < oldLines; i++ {
		fmt.Fprintf(&b, "-old %d\n", i)
		if i == oldLines-1 && rng.Intn(7) == 0 {
			b.WriteString("\\ No newline at end of file\n")
		}
	}
	for i := 0; i < newLines; i++ {
		fmt.Fprintf(&b, "+new %d\n", i)
	}
	if rng.Intn(7) == 0 {
		b.WriteString("\\ No newline at end of file\n")
	}
	return b.String()
}

func gitPath(path, prefix string) string {
	name := prefix + "/" + path
	if strings.ContainsAny(name, "\t\n\"\\") || strings.Contains(name, "\u00e4") {
		return strconv.Quote(name)
	}
	return name
}

func gitName(path string) string {
	if strings.ContainsAny(path, "\t\n\"\\") || strings.Contains(path, "\u00e4") {
		return strconv.Quote(path)
	}
	return path
}

func TestParseRangeBytes(t *testing.T) {
	tests := []struct {
		in        string
		wantStart int64
		wantCount int64
	}{
		{"1", 1, 1},
		{"1,2", 1, 2},
		{"0,0", 0, 0},
		{"12,1", 12, 1},
		{"-5,+7", -5, 7},
		{"bad,3", 0, 3},
		{"3,bad", 3, 0},
		{"", 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			start, count := parseRangeBytes([]byte(tt.in))
			if start != tt.wantStart || count != tt.wantCount {
				t.Fatalf("parseRangeBytes(%q) = (%d, %d), want (%d, %d)", tt.in, start, count, tt.wantStart, tt.wantCount)
			}
		})
	}
}

func TestParseGitLogDateMatchesGitdiff(t *testing.T) {
	tests := []string{
		"Thu Apr 9 01:07:06 2020 -0700",
		"Fri Apr 9 01:07:06 2020 -0700",
		"Thu Apr  9 01:07:06 2020 -0700",
		"Thu Apr 09 01:07:06 2020 -0700",
		"Thu Apr 9 01:07:06 2020",
		"2020-04-09 01:07:06 -0700",
		"2020-04-09T01:07:06-07:00",
		"Thu, 9 Apr 2020 01:07:06 -0700",
		"2020-04-09",
		"1586419626 -0700",
		"1586419626",
		"Thu Apr 31 01:07:06 2020 -0700",
		"4/9/2020 01:07:06 PDT",
		"",
	}

	for _, tt := range tests {
		t.Run(tt, func(t *testing.T) {
			got, gotErr := parseGitLogDate(tt)
			want, wantErr := gitdiff.ParsePatchDate(tt)
			if (gotErr != nil) != (wantErr != nil) {
				t.Fatalf("parseGitLogDate(%q) error = %v, want error = %v", tt, gotErr, wantErr)
			}
			if gotErr == nil && !got.Equal(want) {
				t.Fatalf("parseGitLogDate(%q) = %v, want %v", tt, got, want)
			}
		})
	}
}

func BenchmarkParseRangeBytes(b *testing.B) {
	inputs := [][]byte{
		[]byte("1"),
		[]byte("1,2"),
		[]byte("12,1"),
		[]byte("100000,2500"),
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		in := inputs[i%len(inputs)]
		start, count := parseRangeBytes(in)
		if start == -1 || count == -1 {
			b.Fatal("unreachable")
		}
	}
}

func BenchmarkFastParseGitLogConstructed(b *testing.B) {
	unit := buildLogStream(3, []byte("dir/sub/file.go"), []byte("multi word message"), []byte("secret = value"), 0xff)
	patch := bytes.Repeat(unit, 512)
	b.SetBytes(int64(len(patch)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ch, err := fastParseGitLog(bytes.NewReader(patch))
		if err != nil {
			b.Fatal(err)
		}
		files := 0
		for range ch {
			files++
		}
		if files == 0 {
			b.Fatal("no files parsed")
		}
	}
}

func BenchmarkFastParseGitLogConstructedComparison(b *testing.B) {
	unit := buildLogStream(3, []byte("dir/sub/file.go"), []byte("multi word message"), []byte("secret = value"), 0xff)
	patch := bytes.Repeat(unit, 512)
	b.SetBytes(int64(len(patch)))

	b.Run("adapter", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			ch, err := fastParseGitLog(bytes.NewReader(patch))
			if err != nil {
				b.Fatal(err)
			}
			fastParseBenchmarkSink += consumeParsedFiles(ch)
		}
	})

	b.Run("native-sync", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			n := 0
			err := parseFastGitLog(bytes.NewReader(patch), func(f fastGitFile) error {
				n += consumeFastFile(f)
				return nil
			})
			if err != nil {
				b.Fatal(err)
			}
			fastParseBenchmarkSink += n
		}
	})

	b.Run("native-async", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			n := 0
			for f := range asyncFastFiles(patch) {
				n += consumeFastFile(f)
			}
			fastParseBenchmarkSink += n
		}
	})

	for _, batchSize := range []int{4, 16, 64, 128, 256} {
		b.Run(fmt.Sprintf("native-batch-%d", batchSize), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				fastParseBenchmarkSink += consumeFastBatches(asyncFastGitLogBatches(b.Context(), bytes.NewReader(patch), batchSize))
			}
		})
	}

}

func BenchmarkFastParseGitLogPatch(b *testing.B) {
	path := os.Getenv("BETTERLEAKS_TEST_PATCH")
	if path == "" {
		b.Skip("BETTERLEAKS_TEST_PATCH not set")
	}
	patch, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}

	b.Run("adapter", func(b *testing.B) {
		b.SetBytes(int64(len(patch)))
		b.ReportAllocs()
		for b.Loop() {
			ch, err := fastParseGitLog(bytes.NewReader(patch))
			if err != nil {
				b.Fatal(err)
			}
			fastParseBenchmarkSink += consumeParsedFiles(ch)
		}
	})

	b.Run("native-sync", func(b *testing.B) {
		b.SetBytes(int64(len(patch)))
		b.ReportAllocs()
		for b.Loop() {
			n := 0
			err := parseFastGitLog(bytes.NewReader(patch), func(f fastGitFile) error {
				n += consumeFastFile(f)
				return nil
			})
			if err != nil {
				b.Fatal(err)
			}
			fastParseBenchmarkSink += n
		}
	})

	b.Run("native-async", func(b *testing.B) {
		b.SetBytes(int64(len(patch)))
		b.ReportAllocs()
		for b.Loop() {
			n := 0
			for f := range asyncFastFiles(patch) {
				n += consumeFastFile(f)
			}
			fastParseBenchmarkSink += n
		}
	})

	for _, batchSize := range []int{4, 16, 64, 128, 256} {
		b.Run(fmt.Sprintf("native-batch-%d", batchSize), func(b *testing.B) {
			b.SetBytes(int64(len(patch)))
			b.ReportAllocs()
			for b.Loop() {
				fastParseBenchmarkSink += consumeFastBatches(asyncFastGitLogBatches(b.Context(), bytes.NewReader(patch), batchSize))
			}
		})
	}

}
