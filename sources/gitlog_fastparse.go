package sources

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unsafe"

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
	go func() {
		defer close(out)
		_ = parseFastGitLogCompat(r, func(f *gitdiff.File) error {
			out <- f
			return nil
		})
	}()
	return out, nil
}

// fastGitFragment is the parser-native hunk projection. It owns raw and may
// outlive subsequent reads from the parser's reusable input buffer.
type fastGitFragment struct {
	newPosition         int64
	raw                 string
	missingFinalNewline bool
}

type fastGitIdentity struct {
	name  string
	email string
	valid bool
}

// fastGitHeader is the scanner-consumed commit projection. Native scan paths
// do not construct gitdiff header or identity objects, and message is joined
// once per commit instead of once per file.
type fastGitHeader struct {
	sha        string
	message    string
	author     fastGitIdentity
	authorDate time.Time
}

// fastGitFile contains the fields needed by Git.Fragments without the
// temporary gitdiff File/TextFragment/Line object graph.
type fastGitFile struct {
	header    *fastGitHeader
	newName   string
	fragments []fastGitFragment

	isDelete bool
	isBinary bool
}

// fastParseWindowSize is the read-window size. Measured on real corpora:
// 256KB is ~5% slower (more refills/compactions), 1MB is neutral — 512KB
// is the knee. Each concurrent parser owns one window.
const fastParseWindowSize = 512 << 10

func parseFastGitLog(r io.Reader, emit func(fastGitFile) error) error {
	return parseFastGitLogSized(r, fastParseWindowSize, emit)
}

// parseFastGitLogSized is parseFastGitLog with an explicit window size,
// exposed so tests can force lines and hunks to straddle refill and spill
// boundaries.
func parseFastGitLogSized(r io.Reader, bufSize int, emit func(fastGitFile) error) error {
	p := fastLogParser{
		r:    r,
		buf:  make([]byte, bufSize),
		emit: emit,
	}
	return p.run()
}

// parseFastGitLogRecords exposes commit boundaries as well as files for the
// canonical Git reference adapter. It deliberately reuses the production
// commit-header and patch parser so the two paths cannot drift semantically.
func parseFastGitLogRecords(r io.Reader, emitHeader func(*fastGitHeader) error, emitFile func(fastGitFile) error) error {
	p := fastLogParser{
		r:          r,
		buf:        make([]byte, fastParseWindowSize),
		emit:       emitFile,
		emitHeader: emitHeader,
	}
	return p.run()
}

// parseFastGitLogCompat uses the same parser state machine as the native
// scanner path, but assembles gitdiff objects as each file and hunk is parsed.
// This avoids retaining a second native fragment graph solely to reconstruct
// the public compatibility representation after the file is complete.
func parseFastGitLogCompat(r io.Reader, emit func(*gitdiff.File) error) error {
	return parseFastGitLogCompatSized(r, fastParseWindowSize, emit)
}

func parseFastGitLogCompatSized(r io.Reader, bufSize int, emit func(*gitdiff.File) error) error {
	p := fastLogParser{
		r:          r,
		buf:        make([]byte, bufSize),
		emitCompat: emit,
	}
	return p.run()
}

const fastGitBatchSize = 64

func asyncFastGitLogBatches(ctx context.Context, r io.Reader, batchSize int) <-chan []fastGitFile {
	if batchSize < 1 {
		batchSize = 1
	}
	channelSize := max(1, 64/batchSize)
	out := make(chan []fastGitFile, channelSize)
	go func() {
		defer close(out)
		batch := make([]fastGitFile, 0, batchSize)
		err := parseFastGitLog(r, func(f fastGitFile) error {
			batch = append(batch, f)
			if len(batch) == cap(batch) {
				if err := sendFastGitBatch(ctx, out, batch); err != nil {
					return err
				}
				batch = make([]fastGitFile, 0, batchSize)
			}
			return nil
		})
		if err == nil && len(batch) > 0 {
			_ = sendFastGitBatch(ctx, out, batch)
		}
	}()
	return out
}

func sendFastGitBatch(ctx context.Context, out chan<- []fastGitFile, batch []fastGitFile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case out <- batch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type fastLogParser struct {
	r           io.Reader
	emit        func(fastGitFile) error
	emitCompat  func(*gitdiff.File) error
	emitHeader  func(*fastGitHeader) error
	callbackErr error

	// buf is the read window: buf[pos:end] holds buffered, unconsumed
	// bytes. Refilled in place (readLineSlow) once the window is drained.
	buf     []byte
	pos     int
	end     int
	readErr error // sticky; the underlying reader is never called after it

	// line is the current line, INCLUDING its trailing '\n' when present.
	// It aliases buf (or spill) and is only valid until the next readLine
	// call: every consumer must copy what it keeps before advancing.
	line  []byte
	spill []byte // long-line spill buffer, reused

	// arena accumulates added-line payloads. Each hunk's payloads are
	// appended contiguously and the emitted raw string aliases that range
	// via unsafe.String, so added bytes are copied exactly once (payload ->
	// arena) instead of twice (payload -> builder -> string).
	//
	// SAFETY: aliasing a string over arena bytes is sound because chunks
	// are append-only and never recycled: a grow copies the current hunk's
	// prefix to a fresh chunk and abandons the old one, which stays alive
	// (and unmodified) for exactly as long as any emitted string references
	// it. Nothing ever writes to arena[:len(arena)].
	arena []byte

	// hdrBuf keeps a copy of the current `diff --git` header payload so
	// the fallback name parse can be deferred to finishFile (where it runs
	// only when no later header line named the file). Reused per file.
	hdrBuf []byte

	header       *fastGitHeader       // current native commit header, shared per commit
	compatHeader *gitdiff.PatchHeader // compatibility commit header, shared per commit
}

// arenaChunkSize is the default allocation unit for the added-text arena.
// Hunks larger than this get an exact-size private chunk.
const arenaChunkSize = 64 << 10

// readLine advances to the next input line. p.line is nil at end of
// stream. The hot path is a single newline scan over the buffered window;
// refill, end-of-stream, and window-overflow live in readLineSlow.
//
// (Two rejected alternatives, both measured slower on arm64: batch newline
// pre-indexing per refill (+4-19% — second pass over the window doubles
// cache traffic) and an inline 32-byte SWAR pre-scan before IndexByte
// (+12% — the vectorized IndexByte prologue is already cheaper than
// scalar SWAR at this ~30-byte average line length).)
func (p *fastLogParser) readLine() {
	if i := bytes.IndexByte(p.buf[p.pos:p.end], '\n'); i >= 0 {
		p.line = p.buf[p.pos : p.pos+i+1]
		p.pos += i + 1
		return
	}
	p.readLineSlow()
}

// readLineSlow handles the cold paths: compacting the window tail to the
// front, refilling from the reader, the final unterminated line, and lines
// longer than the window (spilled to a reusable side buffer).
func (p *fastLogParser) readLineSlow() {
	if p.readErr != nil {
		if p.pos < p.end {
			// Final line without trailing newline.
			p.line = p.buf[p.pos:p.end]
			p.pos = p.end
		} else {
			p.line = nil
		}
		return
	}

	// Compact the partial line (no newline in buf[pos:end]) to the front.
	n := p.end - p.pos
	if n > 0 && p.pos > 0 {
		copy(p.buf, p.buf[p.pos:p.end])
	}
	p.pos, p.end = 0, n

	emptyReads := 0
	for {
		if p.end == len(p.buf) {
			// Rare: a line longer than the window (minified JS, etc.).
			p.readLineSpill()
			return
		}
		m, err := p.r.Read(p.buf[p.end:])
		if m == 0 && err == nil {
			// Misbehaving reader; mirror bufio's give-up guard.
			if emptyReads++; emptyReads >= 100 {
				err = io.ErrNoProgress
			}
		}
		if m > 0 {
			// Only the newly arrived bytes need scanning; everything
			// before p.end was already checked.
			if i := bytes.IndexByte(p.buf[p.end:p.end+m], '\n'); i >= 0 {
				nl := p.end + i
				p.end += m
				p.line = p.buf[p.pos : nl+1]
				p.pos = nl + 1
				return
			}
			p.end += m
		}
		if err != nil {
			p.readErr = err
			if p.pos < p.end {
				p.line = p.buf[p.pos:p.end]
				p.pos = p.end
			} else {
				p.line = nil
			}
			return
		}
	}
}

// readLineSpill accumulates a longer-than-window line into p.spill.
func (p *fastLogParser) readLineSpill() {
	p.spill = append(p.spill[:0], p.buf[:p.end]...)
	p.pos, p.end = 0, 0
	emptyReads := 0
	for {
		m, err := p.r.Read(p.buf)
		if m == 0 && err == nil {
			if emptyReads++; emptyReads >= 100 {
				err = io.ErrNoProgress
			}
		}
		if m > 0 {
			if i := bytes.IndexByte(p.buf[:m], '\n'); i >= 0 {
				p.spill = append(p.spill, p.buf[:i+1]...)
				p.pos, p.end = i+1, m
				p.line = p.spill
				return
			}
			p.spill = append(p.spill, p.buf[:m]...)
		}
		if err != nil {
			p.readErr = err
			p.line = p.spill // non-empty: holds at least the window bytes
			return
		}
	}
}

func (p *fastLogParser) run() error {
	// gitdiff.Parse starts with a non-nil empty header and only replaces it
	// when a commit line is seen; mirror that so consumers observe the same
	// PatchHeader nil-ness on streams without commit headers.
	if p.emitCompat != nil {
		p.compatHeader = &gitdiff.PatchHeader{}
	} else {
		p.header = &fastGitHeader{}
	}
	p.readLine()
	for p.line != nil {
		switch {
		case bytes.HasPrefix(p.line, []byte("commit ")):
			p.parseCommitHeader()
			if p.callbackErr != nil {
				return p.callbackErr
			}
		case bytes.HasPrefix(p.line, []byte("diff --git ")):
			if err := p.parseFileDiff(); err != nil {
				return err
			}
		default:
			p.readLine()
		}
	}
	return nil
}

// chompNL returns line without its trailing newline.
func chompNL(line []byte) []byte {
	if n := len(line); n > 0 && line[n-1] == '\n' {
		return line[:n-1]
	}
	return line
}

var errCommitHeaderTooLong = errors.New("commit header line exceeds gitdiff scanner token limit")

// parseCommitHeader consumes a `commit <sha>` block: header fields, blank
// line, then the 4-space-indented message until the next diff/commit/EOF.
// Field semantics mirror gitdiff's parseHeaderPretty + scanMessageTitle/Body
// so PatchHeader.Message() output is unchanged.
func (p *fastLogParser) parseCommitHeader() {
	compat := p.emitCompat != nil
	var h *fastGitHeader
	var compatHeader *gitdiff.PatchHeader
	if compat {
		compatHeader = &gitdiff.PatchHeader{}
	} else {
		h = &fastGitHeader{}
	}
	// TrimSpace mirrors gitdiff.ParsePatchHeader, which trims the header
	// line before prefix-matching. A "commit" line with only whitespace
	// after it therefore fails gitdiff's `commit ` prefix check entirely:
	// the header is rejected and subsequent files carry a nil PatchHeader.
	// Real `git log` output always has a SHA here; this path only matters
	// for malformed streams, where the differential fuzz oracle requires
	// matching gitdiff exactly.
	rest := bytes.TrimSpace(chompNL(p.line)[len("commit "):])
	if len(rest) == 0 {
		p.setHeader(nil, nil, nil)
		p.readLine()
		return
	}
	var sha string
	if i := bytes.IndexByte(rest, ' '); i > 0 {
		sha = string(rest[:i])
	} else {
		sha = string(rest)
	}
	if compat {
		compatHeader.SHA = sha
	} else {
		h.sha = sha
	}
	p.readLine()

	// Header fields until blank line. gitdiff.Parse drops the WHOLE header
	// (files carry nil PatchHeader) when any field fails to parse — real
	// git output never trips this, but the differential fuzz oracle
	// requires identical behavior on malformed streams.
	var hdrErr error
	for p.line != nil {
		line := chompNL(p.line)
		if len(line) > bufio.MaxScanTokenSize {
			hdrErr = errCommitHeaderTooLong
		}
		if len(bytes.TrimSpace(line)) == 0 {
			break
		}
		switch {
		case bytes.HasPrefix(line, []byte("Author:")):
			if compat {
				ident, err := gitdiff.ParsePatchIdentity(string(line[len("Author:"):]))
				if err != nil {
					hdrErr = err
				} else {
					compatHeader.Author = &ident
				}
			} else {
				ident, err := parseFastGitIdentity(line[len("Author:"):])
				if err != nil {
					hdrErr = err
				} else {
					h.author = ident
				}
			}
		case bytes.HasPrefix(line, []byte("AuthorDate:")):
			d, err := parseGitLogDate(strings.TrimSpace(string(line[len("AuthorDate:"):])))
			if err != nil {
				hdrErr = err
			} else if compat {
				compatHeader.AuthorDate = d
			} else {
				h.authorDate = d
			}
		case bytes.HasPrefix(line, []byte("Date:")):
			d, err := parseGitLogDate(strings.TrimSpace(string(line[len("Date:"):])))
			if err != nil {
				hdrErr = err
			} else if compat {
				compatHeader.AuthorDate = d
			} else {
				h.authorDate = d
			}
		case bytes.HasPrefix(line, []byte("Commit:")):
			if compat {
				ident, err := gitdiff.ParsePatchIdentity(string(line[len("Commit:"):]))
				if err != nil {
					hdrErr = err
				} else {
					compatHeader.Committer = &ident
				}
			} else {
				_, err := parseFastGitIdentity(line[len("Commit:"):])
				if err != nil {
					hdrErr = err
				}
			}
		case bytes.HasPrefix(line, []byte("CommitDate:")):
			d, err := parseGitLogDate(strings.TrimSpace(string(line[len("CommitDate:"):])))
			if err != nil {
				hdrErr = err
			} else if compat {
				compatHeader.CommitterDate = d
			}
		}
		p.readLine()
		// A new commit or diff header inside the field block ends it
		// (defensive; git always emits the blank line).
		if p.line != nil && (bytes.HasPrefix(p.line, []byte("commit ")) || bytes.HasPrefix(p.line, []byte("diff --git "))) {
			p.setHeader(h, compatHeader, hdrErr)
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
	skipBody := false
	empty := 0
	for p.line != nil {
		p.readLine()
		if p.line == nil || bytes.HasPrefix(p.line, []byte("commit ")) || bytes.HasPrefix(p.line, []byte("diff --git ")) {
			break
		}
		line := chompNL(p.line)
		if len(line) > bufio.MaxScanTokenSize {
			hdrErr = errCommitHeaderTooLong
		}
		trimmed := bytes.TrimSpace(line)

		if inTitle {
			if len(trimmed) == 0 {
				skipBody = title.Len() == 0
				inTitle = false
				continue
			}
			if !indentSet {
				lineStr := string(line)
				if start := strings.IndexFunc(lineStr, func(c rune) bool { return !unicode.IsSpace(c) }); start > 0 {
					indent = lineStr[:start]
				}
				indentSet = true
			}
			if title.Len() > 0 {
				title.WriteByte(' ')
			}
			title.Write(trimmed)
			continue
		}
		if skipBody {
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

	titleString := title.String()
	if compat {
		compatHeader.Title = titleString
		if titleString != "" {
			compatHeader.Body = body.String()
		}
	} else {
		h.message = titleString
		if titleString != "" {
			if bodyString := body.String(); bodyString != "" {
				h.message = titleString + "\n\n" + bodyString
			}
		}
	}
	p.setHeader(h, compatHeader, hdrErr)
}

func parseFastGitIdentity(line []byte) (fastGitIdentity, error) {
	identity, err := gitdiff.ParsePatchIdentity(string(line))
	if err != nil {
		return fastGitIdentity{}, err
	}
	return fastGitIdentity{
		name:  identity.Name,
		email: identity.Email,
		valid: true,
	}, nil
}

// setHeader installs the parsed commit header, or nil when any field failed
// to parse — mirroring gitdiff.Parse, which ignores ParsePatchHeader errors
// and leaves subsequent files with a nil PatchHeader.
func (p *fastLogParser) setHeader(h *fastGitHeader, compatHeader *gitdiff.PatchHeader, err error) {
	if err != nil {
		p.header = nil
		p.compatHeader = nil
		return
	}
	p.header = h
	p.compatHeader = compatHeader
	if h != nil && p.emitHeader != nil {
		p.callbackErr = p.emitHeader(h)
	}
}

func parseGitLogDate(s string) (time.Time, error) {
	if t, ok := parseGitDefaultDate(s); ok {
		return t, nil
	}
	return gitdiff.ParsePatchDate(s)
}

func parseGitDefaultDate(s string) (time.Time, bool) {
	// Git's default pretty date is "Mon Jan 2 15:04:05 2006 -0700".
	// Go's parser also accepts space- and zero-padded days for this layout;
	// keep the same shape here and leave uncommon layouts to gitdiff.
	if len(s) < 29 || len(s) > 31 || s[3] != ' ' || s[7] != ' ' {
		return time.Time{}, false
	}

	month, ok := parseGitMonth(s[4:7])
	if !ok {
		return time.Time{}, false
	}

	pos := 8
	if s[pos] == ' ' {
		pos++
	}
	if pos >= len(s) || !isASCIIDigit(s[pos]) {
		return time.Time{}, false
	}
	day := int(s[pos] - '0')
	pos++
	if pos < len(s) && isASCIIDigit(s[pos]) {
		day = day*10 + int(s[pos]-'0')
		pos++
	}
	if pos >= len(s) || s[pos] != ' ' {
		return time.Time{}, false
	}
	pos++

	if pos+19 != len(s) || s[pos+2] != ':' || s[pos+5] != ':' || s[pos+8] != ' ' || s[pos+13] != ' ' {
		return time.Time{}, false
	}
	hour, ok := parseTwoDigits(s[pos:])
	if !ok {
		return time.Time{}, false
	}
	minute, ok := parseTwoDigits(s[pos+3:])
	if !ok {
		return time.Time{}, false
	}
	second, ok := parseTwoDigits(s[pos+6:])
	if !ok {
		return time.Time{}, false
	}
	year, ok := parseFourDigits(s[pos+9:])
	if !ok {
		return time.Time{}, false
	}
	zoneSign := s[pos+14]
	if zoneSign != '+' && zoneSign != '-' {
		return time.Time{}, false
	}
	zoneHour, ok := parseTwoDigits(s[pos+15:])
	if !ok {
		return time.Time{}, false
	}
	zoneMinute, ok := parseTwoDigits(s[pos+17:])
	if !ok {
		return time.Time{}, false
	}

	if day < 1 || day > daysInMonth(year, month) || hour > 23 || minute > 59 || second > 59 || zoneHour > 23 || zoneMinute > 59 {
		return time.Time{}, false
	}
	offset := zoneHour*3600 + zoneMinute*60
	if zoneSign == '-' {
		offset = -offset
	}
	return time.Date(year, time.Month(month), day, hour, minute, second, 0, time.FixedZone("", offset)), true
}

func parseGitMonth(s string) (int, bool) {
	switch s {
	case "Jan":
		return 1, true
	case "Feb":
		return 2, true
	case "Mar":
		return 3, true
	case "Apr":
		return 4, true
	case "May":
		return 5, true
	case "Jun":
		return 6, true
	case "Jul":
		return 7, true
	case "Aug":
		return 8, true
	case "Sep":
		return 9, true
	case "Oct":
		return 10, true
	case "Nov":
		return 11, true
	case "Dec":
		return 12, true
	default:
		return 0, false
	}
}

func parseTwoDigits(s string) (int, bool) {
	if len(s) < 2 || !isASCIIDigit(s[0]) || !isASCIIDigit(s[1]) {
		return 0, false
	}
	return int(s[0]-'0')*10 + int(s[1]-'0'), true
}

func parseFourDigits(s string) (int, bool) {
	if len(s) < 4 || !isASCIIDigit(s[0]) || !isASCIIDigit(s[1]) || !isASCIIDigit(s[2]) || !isASCIIDigit(s[3]) {
		return 0, false
	}
	return int(s[0]-'0')*1000 + int(s[1]-'0')*100 + int(s[2]-'0')*10 + int(s[3]-'0'), true
}

func isASCIIDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func daysInMonth(year, month int) int {
	switch month {
	case 4, 6, 9, 11:
		return 30
	case 2:
		if year%4 == 0 && (year%100 != 0 || year%400 == 0) {
			return 29
		}
		return 28
	default:
		return 31
	}
}

// parseFileDiff consumes one `diff --git` block: extended headers, then
// either hunks or a binary marker.
func (p *fastLogParser) parseFileDiff() error {
	var f fastGitFile
	var compatFile *gitdiff.File
	if p.emitCompat != nil {
		compatFile = &gitdiff.File{PatchHeader: p.compatHeader}
	} else {
		f.header = p.header
	}
	// Defer the fallback-name parse (and its string conversion) to
	// finishFile: on real streams a "+++"/"rename to"/"copy to" line names
	// the file, and the header copy is much cheaper than parsing it.
	p.hdrBuf = append(p.hdrBuf[:0], chompNL(p.line)[len("diff --git "):]...)

	p.readLine()

	// Extended header lines.
	var newName string
	for p.line != nil {
		line := chompNL(p.line)
		switch {
		case bytes.HasPrefix(line, []byte("@@ -")):
			goto hunks
		case bytes.HasPrefix(line, []byte("--- ")):
			// consumed; betterleaks only uses NewName
		case bytes.HasPrefix(line, []byte("+++ ")):
			newName = parseAbName(string(line[4:]))
		case bytes.HasPrefix(line, []byte("new file mode ")):
			if compatFile != nil {
				compatFile.IsNew = true
			}
		case bytes.HasPrefix(line, []byte("deleted file mode ")):
			if compatFile != nil {
				compatFile.IsDelete = true
			} else {
				f.isDelete = true
			}
		case bytes.HasPrefix(line, []byte("rename to ")):
			if compatFile != nil {
				compatFile.IsRename = true
			}
			newName = unquoteName(string(line[len("rename to "):]))
		case bytes.HasPrefix(line, []byte("rename new ")):
			if compatFile != nil {
				compatFile.IsRename = true
			}
			newName = unquoteName(string(line[len("rename new "):]))
		case bytes.HasPrefix(line, []byte("copy to ")):
			if compatFile != nil {
				compatFile.IsCopy = true
			}
			newName = unquoteName(string(line[len("copy to "):]))
		case bytes.HasPrefix(line, []byte("rename from ")), bytes.HasPrefix(line, []byte("rename old ")),
			bytes.HasPrefix(line, []byte("copy from ")),
			bytes.HasPrefix(line, []byte("old mode ")), bytes.HasPrefix(line, []byte("new mode ")),
			bytes.HasPrefix(line, []byte("similarity index ")), bytes.HasPrefix(line, []byte("dissimilarity index ")),
			bytes.HasPrefix(line, []byte("index ")):
			// consumed; carries no data betterleaks uses
		case bytes.HasPrefix(line, []byte("Binary files ")) && bytes.HasSuffix(line, []byte("differ")),
			bytes.Equal(line, []byte("GIT binary patch")), bytes.Equal(line, []byte("Binary files differ")),
			bytes.Equal(line, []byte("Files differ")):
			if compatFile != nil {
				compatFile.IsBinary = true
			} else {
				f.isBinary = true
			}
			p.readLine()
			err := p.finishFile(f, compatFile, newName)
			p.skipBinaryData()
			return err
		default:
			// Unknown line ends the header (empty diff: mode-only change,
			// or the stream moved on to the next commit/diff).
			return p.finishFile(f, compatFile, newName)
		}
		p.readLine()
	}
	return p.finishFile(f, compatFile, newName)

hunks:
	for p.line != nil && bytes.HasPrefix(p.line, []byte("@@ -")) {
		if !p.parseHunk(&f, compatFile) {
			break
		}
	}
	return p.finishFile(f, compatFile, newName)
}

// finishFile resolves the file name exactly as betterleaks consumes it and
// emits the file. The `diff --git` fallback name (stashed in hdrBuf) is
// parsed only when no later header line named the file.
func (p *fastLogParser) finishFile(f fastGitFile, compatFile *gitdiff.File, newName string) error {
	resolvedName := newName
	if resolvedName == "" {
		resolvedName = parseHeaderPathPair(string(p.hdrBuf))
	}
	if compatFile != nil {
		compatFile.NewName = resolvedName
		if compatFile.IsDelete {
			compatFile.NewName = ""
		}
		return p.emitCompat(compatFile)
	}
	f.newName = resolvedName
	if f.isDelete {
		// gitdiff sets NewName="" for deletions (parseGitHeaderNewName is
		// gated by !IsDelete, and "+++ /dev/null" never assigns). The
		// consumer only checks IsDelete, but keep the shape identical.
		f.newName = ""
	}
	return p.emit(f)
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

// parseHunk parses one @@ header and its body, appending directly to the
// selected native or compatibility file representation.
func (p *fastLogParser) parseHunk(f *fastGitFile, compatFile *gitdiff.File) bool {
	header := chompNL(p.line)
	// @@ -old[,n] +new[,m] @@ [comment]
	rest := header[len("@@ -"):]
	end := bytes.Index(rest, []byte(" @@"))
	if end < 0 {
		p.readLine()
		return false
	}
	ranges := rest[:end]
	sp := bytes.Index(ranges, []byte(" +"))
	if sp < 0 {
		p.readLine()
		return false
	}
	oldStart, oldCount := parseRangeBytes(ranges[:sp])
	newStart, newCount := parseRangeBytes(ranges[sp+2:])

	// Added-line payloads are appended WITH their trailing newline into the
	// shared arena; the only case where the joined string must not end in
	// '\n' is a trailing "\ No newline at end of file" marker for the new
	// side, handled after the loop by stripping the final byte.
	hunkStart := len(p.arena)
	oldLeft, newLeft := oldCount, newCount
	lastWasAdd := false

	p.readLine()
	// The body loop consumes ~98% of all input lines, so its state (the
	// current line and the window cursor) lives in locals: this keeps the
	// per-line slice-header update out of the heap-resident parser struct
	// (store + write barrier measured ~12% of parse time) and readLine's
	// fast path inline (its cost exceeds the inlining budget). Locals are
	// synced with the struct around every slow-path call.
	line, buf, pos := p.line, p.buf, p.pos
	for (oldLeft > 0 || newLeft > 0) && line != nil {
		switch line[0] {
		case '+':
			newLeft--
			payload := line[1:]
			if len(p.arena)+len(payload) > cap(p.arena) {
				hunkStart = p.growArena(hunkStart, len(payload))
			}
			p.arena = append(p.arena, payload...)
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
			// consumes no counter — falls to the shared line advance.
		default:
			// Malformed/truncated; stop consuming (loop condition fails).
			oldLeft, newLeft = 0, 0
			continue
		}
		// Inlined readLine fast path over the local cursor.
		if i := bytes.IndexByte(buf[pos:p.end], '\n'); i >= 0 {
			line = buf[pos : pos+i+1]
			pos += i + 1
		} else {
			p.pos = pos
			p.readLineSlow()
			line, buf, pos = p.line, p.buf, p.pos
		}
	}
	p.line, p.pos = line, pos

	// Trailing "\ No newline at end of file": strips the final newline of
	// the last emitted line (only affects Raw when that line was an add).
	// Shrinking the arena is safe: the dropped byte was never exposed.
	missingFinalNewline := false
	if p.line != nil && len(p.line) >= 2 && p.line[0] == '\\' && p.line[1] == ' ' {
		if n := len(p.arena); lastWasAdd && n > hunkStart && p.arena[n-1] == '\n' {
			p.arena = p.arena[:n-1]
			missingFinalNewline = true
		}
		p.readLine()
	}

	hunkLen := len(p.arena) - hunkStart
	hasLines := hunkLen > 0 || newCount > 0
	var raw string
	if hunkLen > 0 {
		// Alias the string over the arena range — the strings.Builder
		// trick. Sound because the range [hunkStart, len(arena)) is
		// append-only and this parser never rewrites exposed arena bytes
		// (growArena moves to a fresh chunk instead of recycling).
		raw = unsafe.String(&p.arena[hunkStart], hunkLen)
	}
	if compatFile != nil {
		fragment := &gitdiff.TextFragment{
			OldPosition: oldStart,
			OldLines:    oldCount,
			NewPosition: newStart,
			NewLines:    newCount,
			LinesAdded:  newCount,
		}
		if hasLines {
			fragment.Lines = []gitdiff.Line{{Op: gitdiff.OpAdd, Line: raw}}
		}
		compatFile.TextFragments = append(compatFile.TextFragments, fragment)
	} else {
		f.fragments = append(f.fragments, fastGitFragment{
			newPosition:         newStart,
			raw:                 raw,
			missingFinalNewline: missingFinalNewline,
		})
	}
	return true
}

// growArena starts a fresh arena chunk, carrying over the current hunk's
// prefix. The old chunk is abandoned, NOT recycled: emitted strings alias
// it, so it must stay unmodified for as long as they live (the GC frees it
// once the last aliasing string drops). Copying only the current hunk's
// prefix (not the whole chunk) keeps growth cost proportional to the hunk.
// Returns the hunk's start offset in the new chunk (always 0).
func (p *fastLogParser) growArena(hunkStart, need int) int {
	hunkLen := len(p.arena) - hunkStart
	size := arenaChunkSize
	// Oversized hunk: private chunk with 2x headroom (append's own policy
	// tapers to ~1.25x, which costs ~5x the final size in ramp garbage on
	// multi-MB hunks).
	if total := hunkLen + need; total > size/2 {
		size = 2 * total
	}
	fresh := make([]byte, hunkLen, size)
	copy(fresh, p.arena[hunkStart:])
	p.arena = fresh
	return 0
}

// parseRangeBytes parses "start[,count]"; count defaults to 1.
func parseRangeBytes(b []byte) (start, count int64) {
	comma := bytes.IndexByte(b, ',')
	if comma < 0 {
		return parseInt64Bytes(b), 1
	}
	return parseInt64Bytes(b[:comma]), parseInt64Bytes(b[comma+1:])
}

func parseInt64Bytes(b []byte) int64 {
	if len(b) == 0 {
		return 0
	}
	neg := false
	i := 0
	switch b[0] {
	case '-':
		neg = true
		i = 1
	case '+':
		i = 1
	}
	if i == len(b) {
		return 0
	}
	if len(b)-i > 18 {
		n, _ := strconv.ParseInt(string(b), 10, 64)
		return n
	}
	var n int64
	for ; i < len(b); i++ {
		c := b[i]
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	if neg {
		return -n
	}
	return n
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

// ParseGitLogStreamForTest runs the fast log-stream parser over a captured
// patch stream and returns all files. Test-only entry point for golden
// digest fixtures; production consumers use the channel wiring in
// startGitLogCmd.
func ParseGitLogStreamForTest(patch []byte) ([]*gitdiff.File, error) {
	ch, err := fastParseGitLog(bytes.NewReader(patch))
	if err != nil {
		return nil, err
	}
	var files []*gitdiff.File
	for f := range ch {
		files = append(files, f)
	}
	return files, nil
}
