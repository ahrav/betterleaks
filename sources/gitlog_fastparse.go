package sources

import (
	"bufio"
	"bytes"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gitleaks/go-gitdiff/gitdiff"
)

// fastParseGitLog is a purpose-built streaming parser for the exact stream
// betterleaks requests: `git log -p -U0 --diff-filter=tuxdb` (and the staged
// `git diff -U0` variant). It replaces gitdiff.Parse on this hot path.
//
// gitdiff.Parse is a general patch parser: it allocates a string per input
// line (bufio ReadString), a Line struct per patch line, re-joins added
// lines through a Builder per fragment, and re-parses the commit header
// with a scanner — ~40% of log-mode's in-process CPU. This parser reads
// lines as byte slices from one reusable buffer, keeps only added-line
// bytes, and emits *gitdiff.File values populated with exactly the fields
// betterleaks consumes:
//
//	File.NewName, File.IsDelete, File.IsBinary, File.PatchHeader
//	(SHA, Author, AuthorDate, Title, Body), and File.TextFragments with
//	NewPosition and a single pre-joined OpAdd Line per fragment
//	(TextFragment.Raw(OpAdd) returns exactly that string).
//
// Output equivalence with gitdiff.Parse over this stream shape is covered
// by TestFastParseMatchesGitdiff, which fuzzes both parsers against real
// repository output.
func fastParseGitLog(r io.Reader) (<-chan *gitdiff.File, error) {
	// The channel is buffered: the parser can run ahead of the consumer
	// (detector) instead of hand-off blocking per file like the unbuffered
	// channel gitdiff.Parse returns.
	out := make(chan *gitdiff.File, 64)
	p := &fastLogParser{r: bufio.NewReaderSize(r, 512<<10), out: out}
	go p.run()
	return out, nil
}

type fastLogParser struct {
	r   *bufio.Reader
	out chan *gitdiff.File

	// line is the current line, INCLUDING its trailing '\n' when present.
	// It aliases the bufio buffer (or spill) and is only valid until the
	// next readLine call.
	line  []byte
	spill []byte // long-line spill buffer, reused
	eof   bool

	header *gitdiff.PatchHeader // current commit header, shared per commit
}

// readLine advances to the next input line, spilling lines longer than the
// bufio buffer. Sets p.eof at end of stream (p.line is empty then).
func (p *fastLogParser) readLine() {
	if p.eof {
		p.line = nil
		return
	}
	line, err := p.r.ReadSlice('\n')
	if err == bufio.ErrBufferFull {
		// Rare: a line longer than the reader buffer (minified JS, etc.).
		p.spill = append(p.spill[:0], line...)
		for err == bufio.ErrBufferFull {
			line, err = p.r.ReadSlice('\n')
			p.spill = append(p.spill, line...)
		}
		p.line = p.spill
	} else {
		p.line = line
	}
	if err != nil && err != bufio.ErrBufferFull {
		p.eof = true
		if len(p.line) == 0 {
			p.line = nil
		}
	}
}

func (p *fastLogParser) run() {
	defer close(p.out)
	// gitdiff.Parse starts with a non-nil empty header and only replaces it
	// when a commit line is seen; mirror that so consumers observe the same
	// PatchHeader nil-ness on streams without commit headers.
	p.header = &gitdiff.PatchHeader{}
	p.readLine()
	for p.line != nil {
		switch {
		case bytes.HasPrefix(p.line, []byte("commit ")):
			p.parseCommitHeader()
		case bytes.HasPrefix(p.line, []byte("diff --git ")):
			p.parseFileDiff()
		default:
			p.readLine()
		}
	}
}

// chompNL returns line without its trailing newline.
func chompNL(line []byte) []byte {
	if n := len(line); n > 0 && line[n-1] == '\n' {
		return line[:n-1]
	}
	return line
}

// parseCommitHeader consumes a `commit <sha>` block: header fields, blank
// line, then the 4-space-indented message until the next diff/commit/EOF.
// Field semantics mirror gitdiff's parseHeaderPretty + scanMessageTitle/Body
// so PatchHeader.Message() output is unchanged.
func (p *fastLogParser) parseCommitHeader() {
	h := &gitdiff.PatchHeader{}
	// TrimSpace mirrors gitdiff.ParsePatchHeader, which trims the header
	// line before prefix-matching. A "commit" line with only whitespace
	// after it therefore fails gitdiff's `commit ` prefix check entirely:
	// the header is rejected and subsequent files carry a nil PatchHeader.
	// Real `git log` output always has a SHA here; this path only matters
	// for malformed streams, where the differential fuzz oracle requires
	// matching gitdiff exactly.
	rest := bytes.TrimSpace(chompNL(p.line)[len("commit "):])
	if len(rest) == 0 {
		p.header = nil
		p.readLine()
		return
	}
	if i := bytes.IndexByte(rest, ' '); i > 0 {
		h.SHA = string(rest[:i])
	} else {
		h.SHA = string(rest)
	}
	p.readLine()

	// Header fields until blank line. gitdiff.Parse drops the WHOLE header
	// (files carry nil PatchHeader) when any field fails to parse — real
	// git output never trips this, but the differential fuzz oracle
	// requires identical behavior on malformed streams.
	var hdrErr error
	for p.line != nil {
		line := chompNL(p.line)
		if len(bytes.TrimSpace(line)) == 0 {
			break
		}
		switch {
		case bytes.HasPrefix(line, []byte("Author:")):
			ident, err := gitdiff.ParsePatchIdentity(string(line[len("Author:"):]))
			if err != nil {
				hdrErr = err
			} else {
				h.Author = &ident
			}
		case bytes.HasPrefix(line, []byte("AuthorDate:")):
			d, err := gitdiff.ParsePatchDate(strings.TrimSpace(string(line[len("AuthorDate:"):])))
			if err != nil {
				hdrErr = err
			} else {
				h.AuthorDate = d
			}
		case bytes.HasPrefix(line, []byte("Date:")):
			d, err := gitdiff.ParsePatchDate(strings.TrimSpace(string(line[len("Date:"):])))
			if err != nil {
				hdrErr = err
			} else {
				h.AuthorDate = d
			}
		case bytes.HasPrefix(line, []byte("Commit:")):
			ident, err := gitdiff.ParsePatchIdentity(string(line[len("Commit:"):]))
			if err != nil {
				hdrErr = err
			} else {
				h.Committer = &ident
			}
		case bytes.HasPrefix(line, []byte("CommitDate:")):
			d, err := gitdiff.ParsePatchDate(strings.TrimSpace(string(line[len("CommitDate:"):])))
			if err != nil {
				hdrErr = err
			} else {
				h.CommitterDate = d
			}
		}
		p.readLine()
		// A new commit or diff header inside the field block ends it
		// (defensive; git always emits the blank line).
		if p.line != nil && (bytes.HasPrefix(p.line, []byte("commit ")) || bytes.HasPrefix(p.line, []byte("diff --git "))) {
			p.setHeader(h, hdrErr)
			return
		}
	}

	// Message block: indented lines until the next commit/diff header.
	// Title = non-empty lines joined by spaces up to the first blank line;
	// Body = remaining lines with indent stripped and blank-line runs
	// collapsed to one separator — matching gitdiff's scan functions.
	var title strings.Builder
	var body strings.Builder
	indent := ""
	indentSet := false
	inTitle := true
	empty := 0
	for p.line != nil {
		p.readLine()
		if p.line == nil || bytes.HasPrefix(p.line, []byte("commit ")) || bytes.HasPrefix(p.line, []byte("diff --git ")) {
			break
		}
		line := chompNL(p.line)
		trimmed := bytes.TrimSpace(line)

		if inTitle {
			if len(trimmed) == 0 {
				if title.Len() > 0 {
					inTitle = false
				}
				continue
			}
			if !indentSet {
				ws := 0
				for ws < len(line) && (line[ws] == ' ' || line[ws] == '\t') {
					ws++
				}
				indent = string(line[:ws])
				indentSet = true
			}
			if title.Len() > 0 {
				title.WriteByte(' ')
			}
			title.Write(trimmed)
			continue
		}

		// Body: TrimRight whitespace (full Unicode set — messages contain
		// NBSP and friends), strip the title's indent prefix.
		l := string(bytes.TrimRightFunc(line, unicode.IsSpace))
		l = strings.TrimPrefix(l, indent)
		if l == "" {
			empty++
			continue
		}
		if body.Len() > 0 {
			body.WriteByte('\n')
			if empty > 0 {
				body.WriteByte('\n')
			}
		}
		empty = 0
		body.WriteString(l)
	}

	h.Title = title.String()
	if h.Title != "" {
		h.Body = body.String()
	}
	p.setHeader(h, hdrErr)
}

// setHeader installs the parsed commit header, or nil when any field failed
// to parse — mirroring gitdiff.Parse, which ignores ParsePatchHeader errors
// and leaves subsequent files with a nil PatchHeader.
func (p *fastLogParser) setHeader(h *gitdiff.PatchHeader, err error) {
	if err != nil {
		p.header = nil
		return
	}
	p.header = h
}

// parseGitDefaultDate parses a Date: header value. Delegates to
// gitdiff.ParsePatchDate so all its accepted layouts (default, iso, rfc,
// unix, raw) behave identically; this runs once per commit, not per line,
// so the layout cascade is not a hot path.
func parseGitDefaultDate(s string) time.Time {
	t, err := gitdiff.ParsePatchDate(s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// parseFileDiff consumes one `diff --git` block: extended headers, then
// either hunks or a binary marker.
func (p *fastLogParser) parseFileDiff() {
	f := &gitdiff.File{PatchHeader: p.header}
	headerLine := chompNL(p.line)[len("diff --git "):]
	defaultName := parseHeaderPathPair(string(headerLine))

	p.readLine()

	// Extended header lines.
	var oldName, newName string
	hasDefaultNameFallback := false
	for p.line != nil {
		line := chompNL(p.line)
		s := string(line)
		switch {
		case strings.HasPrefix(s, "@@ -"):
			goto hunks
		case strings.HasPrefix(s, "--- "):
			oldName = parseAbName(s[4:])
			_ = oldName
		case strings.HasPrefix(s, "+++ "):
			newName = parseAbName(s[4:])
		case strings.HasPrefix(s, "new file mode "):
			f.IsNew = true
			hasDefaultNameFallback = true
		case strings.HasPrefix(s, "deleted file mode "):
			f.IsDelete = true
			hasDefaultNameFallback = true
		case strings.HasPrefix(s, "rename to "):
			f.IsRename = true
			newName = unquoteName(s[len("rename to "):])
		case strings.HasPrefix(s, "copy to "):
			f.IsCopy = true
			newName = unquoteName(s[len("copy to "):])
		case strings.HasPrefix(s, "rename from "), strings.HasPrefix(s, "copy from "),
			strings.HasPrefix(s, "old mode "), strings.HasPrefix(s, "new mode "),
			strings.HasPrefix(s, "similarity index "), strings.HasPrefix(s, "dissimilarity index "),
			strings.HasPrefix(s, "index "):
			// consumed; carries no data betterleaks uses
			hasDefaultNameFallback = hasDefaultNameFallback || strings.HasPrefix(s, "old mode ")
		case strings.HasPrefix(s, "Binary files ") && strings.HasSuffix(s, "differ"),
			s == "GIT binary patch", s == "Binary files differ":
			f.IsBinary = true
			p.readLine()
			p.finishFile(f, newName, defaultName)
			p.skipBinaryData()
			return
		default:
			// Unknown line ends the header (empty diff: mode-only change,
			// or the stream moved on to the next commit/diff).
			p.finishFile(f, newName, defaultName)
			return
		}
		p.readLine()
	}
	p.finishFile(f, newName, defaultName)
	return

hunks:
	for p.line != nil && bytes.HasPrefix(p.line, []byte("@@ -")) {
		frag := p.parseHunk()
		if frag == nil {
			break
		}
		f.TextFragments = append(f.TextFragments, frag)
	}
	_ = hasDefaultNameFallback
	p.finishFile(f, newName, defaultName)
}

// finishFile resolves the file name exactly as betterleaks consumes it and
// emits the file.
func (p *fastLogParser) finishFile(f *gitdiff.File, newName, defaultName string) {
	switch {
	case newName != "":
		f.NewName = newName
	case defaultName != "":
		f.NewName = defaultName
	}
	if f.IsDelete {
		// gitdiff sets NewName="" for deletions (parseGitHeaderNewName is
		// gated by !IsDelete, and "+++ /dev/null" never assigns). The
		// consumer only checks IsDelete, but keep the shape identical.
		f.NewName = ""
	}
	p.out <- f
}

// skipBinaryData consumes "GIT binary patch" literal sections if present.
// With betterleaks' flags git emits "Binary files ... differ" (no data), so
// this is defensive: skip base85 data lines (they never start with header
// prefixes we dispatch on).
func (p *fastLogParser) skipBinaryData() {
	for p.line != nil {
		if bytes.HasPrefix(p.line, []byte("commit ")) || bytes.HasPrefix(p.line, []byte("diff --git ")) {
			return
		}
		p.readLine()
	}
}

// parseHunk parses one @@ header and its body, returning a TextFragment
// with NewPosition set and a single OpAdd line holding the pre-joined added
// bytes (exactly what TextFragment.Raw(gitdiff.OpAdd) returns).
func (p *fastLogParser) parseHunk() *gitdiff.TextFragment {
	header := chompNL(p.line)
	// @@ -old[,n] +new[,m] @@ [comment]
	rest := header[len("@@ -"):]
	end := bytes.Index(rest, []byte(" @@"))
	if end < 0 {
		p.readLine()
		return nil
	}
	ranges := rest[:end]
	sp := bytes.Index(ranges, []byte(" +"))
	if sp < 0 {
		p.readLine()
		return nil
	}
	oldStart, oldCount := parseRangeBytes(ranges[:sp])
	newStart, newCount := parseRangeBytes(ranges[sp+2:])

	frag := &gitdiff.TextFragment{
		OldPosition: oldStart,
		OldLines:    oldCount,
		NewPosition: newStart,
		NewLines:    newCount,
	}

	var added bytes.Buffer
	oldLeft, newLeft := oldCount, newCount
	lastWasAdd := false

	p.readLine()
	for (oldLeft > 0 || newLeft > 0) && p.line != nil {
		line := p.line
		op := line[0]
		switch op {
		case '+':
			newLeft--
			added.Write(line[1:])
			lastWasAdd = true
		case '-':
			oldLeft--
			lastWasAdd = false
		case ' ', '\n':
			oldLeft--
			newLeft--
			lastWasAdd = false
		case '\\':
			// "\ No newline at end of file" for the OLD side mid-hunk;
			// doesn't consume a counter.
			p.readLine()
			continue
		default:
			// Malformed/truncated; stop consuming.
			oldLeft, newLeft = 0, 0
			continue
		}
		p.readLine()
	}

	// Trailing "\ No newline at end of file": strips the final newline of
	// the last emitted line (only affects Raw when that line was an add).
	if p.line != nil && len(p.line) >= 2 && p.line[0] == '\\' && p.line[1] == ' ' {
		if lastWasAdd {
			b := added.Bytes()
			if n := len(b); n > 0 && b[n-1] == '\n' {
				added.Truncate(n - 1)
			}
		}
		p.readLine()
	}

	frag.LinesAdded = newCount // informational; consumer doesn't read it
	if added.Len() > 0 || newCount > 0 {
		frag.Lines = []gitdiff.Line{{Op: gitdiff.OpAdd, Line: added.String()}}
	}
	return frag
}

// parseRangeBytes parses "start[,count]"; count defaults to 1.
func parseRangeBytes(b []byte) (start, count int64) {
	comma := bytes.IndexByte(b, ',')
	if comma < 0 {
		start, _ = strconv.ParseInt(string(b), 10, 64)
		return start, 1
	}
	start, _ = strconv.ParseInt(string(b[:comma]), 10, 64)
	count, _ = strconv.ParseInt(string(b[comma+1:]), 10, 64)
	return start, count
}

// parseAbName extracts the path from a "--- a/path" / "+++ b/path" value,
// handling /dev/null, quoting, and the one-component prefix strip.
func parseAbName(s string) string {
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		s = s[:i]
	}
	if s == "/dev/null" {
		return ""
	}
	name := unquoteName(s)
	// strip a/ or b/ prefix (one tree component, like gitdiff's dropPrefix=1)
	if i := strings.IndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// unquoteName undoes git's C-style path quoting when present.
func unquoteName(s string) string {
	if len(s) > 1 && s[0] == '"' {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
	}
	return s
}

// parseHeaderPathPair extracts the shared name from a `diff --git a/X b/X`
// header, mirroring gitdiff.parseGitHeaderName: it is only used as a
// fallback when no other header line names the file (mode-only changes,
// empty file creation/deletion), and returns "" when the two sides differ
// (renames name themselves via `rename to`).
func parseHeaderPathPair(header string) string {
	header = strings.TrimSuffix(header, "\n")
	if header == "" {
		return ""
	}

	var first, second string
	quote := strings.IndexByte(header, '"')
	switch {
	case quote < 0:
		first = header
	case quote > 0:
		if header[quote-1] != ' ' && header[quote-1] != '\t' {
			return ""
		}
		first = header[:quote-1]
		second = unquoteFirst(header[quote:])
	default: // quote == 0
		var n int
		first, n = unquoteFirstN(header)
		if first == "" {
			return ""
		}
		for n < len(header) && (header[n] == ' ' || header[n] == '\t') {
			n++
		}
		if n == len(header) {
			return ""
		}
		if header[n] == '"' {
			second = unquoteFirst(header[n:])
		} else {
			second = header[n:]
		}
	}

	first = trimOneComponent(first)
	if second != "" {
		if first == trimOneComponent(second) {
			return first
		}
		return ""
	}

	// Both unquoted: find a space split yielding two equal names.
	for i := 0; i < len(first)-1; i++ {
		if first[i] != ' ' && first[i] != '\t' {
			continue
		}
		second = trimOneComponent(first[i+1:])
		if name := first[:i]; name == second {
			return name
		}
	}
	return ""
}

func unquoteFirst(s string) string {
	name, _ := unquoteFirstN(s)
	return name
}

// unquoteFirstN unquotes a leading quoted segment and returns it with the
// number of bytes consumed.
func unquoteFirstN(s string) (string, int) {
	n := 1
	for ; n < len(s); n++ {
		if s[n] == '"' && s[n-1] != '\\' {
			n++
			break
		}
	}
	if n == 2 {
		return "", 0
	}
	u, err := strconv.Unquote(s[:n])
	if err != nil {
		return "", 0
	}
	return u, n
}

func trimOneComponent(name string) string {
	if i := strings.IndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return name
}
