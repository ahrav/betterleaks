package sources

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gitleaks/go-gitdiff/gitdiff"
)

// The fast parser's contract has two halves, fuzzed separately:
//
//   - FuzzFastParseGitLogRobustness: arbitrary bytes must never panic or
//     hang the parser (it consumes untrusted-shaped subprocess output).
//   - FuzzFastParseGitLogDifferential: on well-formed default-shaped
//     `git log -p -U0` streams — the only streams the parser is wired for
//     (user --log-opts route to gitdiff.Parse) — its consumed projection
//     must equal gitdiff.Parse's exactly. The stream is *constructed* from
//     fuzz-chosen primitives so every input is inside the contract, unlike
//     raw bytes where gitdiff's traditional-patch/mail fallbacks (which git
//     log never emits) would demand bug-for-bug reimplementation.
//
// Three real divergences found by earlier fuzzing are locked in by corpus
// files: SHA whitespace trimming, nil-header propagation on malformed
// header fields, and full ParsePatchDate layout coverage.

func drainFast(t *testing.T, data []byte, timeout time.Duration) []consumedView {
	t.Helper()
	done := make(chan []consumedView, 1)
	go func() {
		var views []consumedView
		err := parseFastGitLog(bytes.NewReader(data), func(f fastGitFile) error {
			if !f.isDelete {
				views = append(views, viewOfFast(f))
			}
			return nil
		})
		if err != nil {
			done <- nil
			return
		}
		done <- views
	}()
	select {
	case v := <-done:
		return v
	case <-time.After(timeout):
		t.Fatalf("fastParseGitLog hung on %d-byte input: %q", len(data), truncateForMsg(data))
		return nil
	}
}

// FuzzFastParseGitLogRobustness: no panic, no hang, on anything.
func FuzzFastParseGitLogRobustness(f *testing.F) {
	seeds := []string{
		"",
		"\n",
		"commit ",
		"commit  \n0\ndiff --git \"/p\\303\\244th.txt\"\"/p\\303\\244th.txt\"",
		"commit \n\n0\ndiff --git /f.txt /f.txt",
		"commit 0\nDate:0\ndiff --git /img.png /img.png",
		"commit 0\nDate:A\ndiff --git /f.txt /f.txt",
		"diff --git \n\n--- 0\n+++ 0\n@@ -0 +0 @@\n-\n+",
		"diff --git a/f b/f\n@@ -1,5 +1,5 @@\n+only one line\n",
		"@@ -0,0 +1000000000 @@\n+x\n",
		"diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1 +1 @@\nXinvalid op\n",
		"commit 1111111111111111111111111111111111111111\nAuthor: A <a@x>\nDate:   Mon Jan 2 15:04:05 2026 +0000\n\n    msg\n\ndiff --git a/f.txt b/f.txt\nindex 0..1 100644\n--- a/f.txt\n+++ b/f.txt\n@@ -0,0 +1 @@\n+x\n\\ No newline at end of file\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		drainFast(t, data, 10*time.Second)
	})
}

// buildLogStream deterministically constructs a well-formed default-shaped
// stream from fuzz primitives. Internal consistency (hunk counts matching
// emitted lines, canonical a/ b/ prefixes, 40-hex SHAs) is guaranteed by
// construction, so both parsers must fully agree.
func buildLogStream(nCommits uint8, pathSeed, msgSeed, lineSeed []byte, flags uint8) []byte {
	sanitize := func(seed []byte, fallback string, allowed string) string {
		var b strings.Builder
		for _, c := range seed {
			if strings.IndexByte(allowed, c) >= 0 {
				b.WriteByte(c)
			}
		}
		if b.Len() == 0 {
			return fallback
		}
		s := b.String()
		if len(s) > 60 {
			s = s[:60]
		}
		return s
	}
	pathChars := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-/"
	lineChars := "abcdefghijklmnopqrstuvwxyz \t=:\"'{}()+-\\@#$%^&*"

	path := sanitize(pathSeed, "f.txt", pathChars)
	path = strings.Trim(path, "/")
	// git never emits consecutive slashes in paths (gitdiff's cleanName
	// collapses them defensively; real trees cannot contain them).
	for strings.Contains(path, "//") {
		path = strings.ReplaceAll(path, "//", "/")
	}
	if path == "" {
		path = "f.txt"
	}
	msg := sanitize(msgSeed, "msg", lineChars)
	line := sanitize(lineSeed, "content", lineChars)

	var out strings.Builder
	commits := int(nCommits%4) + 1
	for c := 0; c < commits; c++ {
		fmt.Fprintf(&out, "commit %040d\n", c+1)
		out.WriteString("Author: A U Thor <author@example.com>\n")
		out.WriteString("Date:   Mon Jan 2 15:04:05 2026 +0000\n\n")
		fmt.Fprintf(&out, "    %s\n\n", msg)

		newFile := flags&(1<<(c%8)) != 0
		binary := flags&(1<<((c+1)%8)) != 0 && !newFile
		multiHunk := flags&(1<<((c+2)%8)) != 0
		noNewline := flags&(1<<((c+3)%8)) != 0

		fmt.Fprintf(&out, "diff --git a/%s b/%s\n", path, path)
		switch {
		case binary:
			out.WriteString("index 0000000..1111111 100644\n")
			fmt.Fprintf(&out, "Binary files a/%s and b/%s differ\n", path, path)
			continue
		case newFile:
			out.WriteString("new file mode 100644\nindex 0000000..1111111\n--- /dev/null\n")
			fmt.Fprintf(&out, "+++ b/%s\n", path)
			fmt.Fprintf(&out, "@@ -0,0 +1,2 @@\n+%s\n+%s tail\n", line, line)
		default:
			out.WriteString("index 0000000..1111111 100644\n")
			fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", path, path)
			fmt.Fprintf(&out, "@@ -1,2 +1,2 @@\n-%s old\n-gone\n+%s\n+kept\n", line, line)
			if multiHunk {
				fmt.Fprintf(&out, "@@ -10,0 +12,1 @@ ctx %s\n+%s later\n", msg, line)
			}
		}
		if noNewline {
			// Rewrite is complex; instead append a fresh single-line hunk
			// ending without a newline (its own header keeps counts exact).
			fmt.Fprintf(&out, "@@ -20,0 +25,1 @@\n+%s eof\n\\ No newline at end of file\n", line)
		}
	}
	return []byte(out.String())
}

// FuzzFastParseGitLogDifferential: full view equality with gitdiff.Parse on
// constructed well-formed streams.
func FuzzFastParseGitLogDifferential(f *testing.F) {
	f.Add(uint8(1), []byte("a/b.txt"), []byte("fix things"), []byte("secret=x"), uint8(0))
	f.Add(uint8(3), []byte("dir/sub/file.go"), []byte("multi word msg"), []byte("key: value"), uint8(0xFF))
	f.Add(uint8(2), []byte(""), []byte(""), []byte(""), uint8(0b1010))
	f.Add(uint8(4), []byte("weird-.path"), []byte("tabs\there"), []byte("+plus -minus"), uint8(0b0101))

	f.Fuzz(func(t *testing.T, n uint8, pathSeed, msgSeed, lineSeed []byte, flags uint8) {
		data := buildLogStream(n, pathSeed, msgSeed, lineSeed, flags)

		fast := drainFast(t, data, 10*time.Second)

		refCh, err := gitdiff.Parse(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("gitdiff.Parse rejected a constructed stream: %v\n%s", err, data)
		}
		var ref []consumedView
		for fl := range refCh {
			if fl.IsDelete {
				continue
			}
			ref = append(ref, viewOf(fl))
		}

		if len(ref) != len(fast) {
			t.Fatalf("file count mismatch on constructed stream: gitdiff=%d fast=%d\n%s", len(ref), len(fast), data)
		}
		for i := range ref {
			if fmt.Sprintf("%+v", ref[i]) != fmt.Sprintf("%+v", fast[i]) {
				t.Fatalf("divergence at file %d:\n gitdiff: %+v\n fast:    %+v\nstream:\n%s", i, ref[i], fast[i], data)
			}
		}
	})
}

func truncateForMsg(data []byte) string {
	if len(data) > 300 {
		return string(data[:300]) + "..."
	}
	return string(data)
}
