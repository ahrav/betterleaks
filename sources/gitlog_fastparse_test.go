package sources

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitleaks/go-gitdiff/gitdiff"
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

	if len(ref) != len(fast) {
		t.Fatalf("%s: file count mismatch: gitdiff=%d fast=%d", label, len(ref), len(fast))
	}
	for i := range ref {
		if fmt.Sprintf("%+v", ref[i]) != fmt.Sprintf("%+v", fast[i]) {
			t.Errorf("%s: file %d differs:\n gitdiff: %+v\n fast:    %+v", label, i, ref[i], fast[i])
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

// TestFastParseSyntheticShapes exercises tricky stream shapes directly.
func TestFastParseSyntheticShapes(t *testing.T) {
	cases := map[string]string{
		"empty-commit-no-diff":  "commit 1111111111111111111111111111111111111111\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    subject only\n",
		"no-newline-at-eof-add": "commit 2222222222222222222222222222222222222222\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/f.txt b/f.txt\nindex 0000000..1111111 100644\n--- a/f.txt\n+++ b/f.txt\n@@ -0,0 +1 @@\n+no trailing newline\n\\ No newline at end of file\n",
		"no-newline-old-side":   "commit 3333333333333333333333333333333333333333\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/f.txt b/f.txt\nindex 0000000..1111111 100644\n--- a/f.txt\n+++ b/f.txt\n@@ -1 +1 @@\n-old line\n\\ No newline at end of file\n+new line\n",
		"quoted-unicode-path":   "commit 4444444444444444444444444444444444444444\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git \"a/p\\303\\244th.txt\" \"b/p\\303\\244th.txt\"\nnew file mode 100644\nindex 0000000..1111111\n--- /dev/null\n+++ \"b/p\\303\\244th.txt\"\n@@ -0,0 +1 @@\n+content\n",
		"binary-file":           "commit 5555555555555555555555555555555555555555\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/img.png b/img.png\nnew file mode 100644\nindex 0000000..1111111\nBinary files /dev/null and b/img.png differ\n",
		"mode-only-change":      "commit 6666666666666666666666666666666666666666\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/script.sh b/script.sh\nold mode 100644\nnew mode 100755\n",
		"multi-hunk":            "commit 7777777777777777777777777777777777777777\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    title\n    \n    body line 1\n    body line 2\n\ndiff --git a/m.txt b/m.txt\nindex 0000000..1111111 100644\n--- a/m.txt\n+++ b/m.txt\n@@ -1 +1,2 @@\n-a\n+b\n+c\n@@ -10,0 +12 @@ func ctx() {\n+d\n",
		"rename-with-edit":      "commit 8888888888888888888888888888888888888888\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/old.txt b/new.txt\nsimilarity index 90%\nrename from old.txt\nrename to new.txt\nindex 0000000..1111111 100644\n--- a/old.txt\n+++ b/new.txt\n@@ -1 +1 @@\n-x\n+y\n",
		"empty-line-context":    "commit 9999999999999999999999999999999999999999\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/f.txt b/f.txt\nindex 0000000..1111111 100644\n--- a/f.txt\n+++ b/f.txt\n@@ -1,2 +1,2 @@\n\n+added after empty context\n-removed\n",
	}
	for name, patch := range cases {
		t.Run(name, func(t *testing.T) {
			diffBothParsers(t, []byte(patch), name)
		})
	}
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
