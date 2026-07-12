package detect

import (
	// "encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"

	"github.com/betterleaks/betterleaks/logging"
	"github.com/betterleaks/betterleaks/report"
	"github.com/betterleaks/betterleaks/sources"
	"github.com/betterleaks/betterleaks/sources/scm"
)

// samePath reports whether two file paths refer to the same location, tolerating
// OS separator differences. The file source normalizes fragment paths to forward
// slashes (filepath.ToSlash), whereas config/baseline paths keep the native
// separator, so a raw == comparison misses on Windows and the config or baseline
// file ends up being scanned against itself.
func samePath(a, b string) bool {
	return filepath.ToSlash(filepath.Clean(a)) == filepath.ToSlash(filepath.Clean(b))
}

var linkCleaner = strings.NewReplacer(
	" ", "%20",
	"%", "%25",
)

func createScmLink(platform, remoteURL string, finding report.Finding) string {
	p, _ := scm.PlatformFromString(platform)
	commitSha := finding.Attr(sources.AttrGitSHA)
	path := finding.Attr(sources.AttrPath)
	if p == scm.UnknownPlatform || p == scm.NoPlatform || commitSha == "" || path == "" {
		return ""
	}

	// Clean the path.
	filePath, _, hasInnerPath := strings.Cut(path, sources.InnerPathSeparator)
	filePath = linkCleaner.Replace(filePath)

	switch p {
	case scm.GitHubPlatform:
		link := fmt.Sprintf("%s/blob/%s/%s", remoteURL, commitSha, filePath)
		if hasInnerPath {
			return link
		}
		ext := strings.ToLower(filepath.Ext(filePath))
		if ext == ".ipynb" || ext == ".md" {
			link += "?plain=1"
		}
		if finding.StartLine != 0 {
			link += fmt.Sprintf("#L%d", finding.StartLine)
		}
		if finding.EndLine != finding.StartLine {
			link += fmt.Sprintf("-L%d", finding.EndLine)
		}
		return link
	case scm.GitLabPlatform:
		link := fmt.Sprintf("%s/blob/%s/%s", remoteURL, commitSha, filePath)
		if hasInnerPath {
			return link
		}
		if finding.StartLine != 0 {
			link += fmt.Sprintf("#L%d", finding.StartLine)
		}
		if finding.EndLine != finding.StartLine {
			link += fmt.Sprintf("-%d", finding.EndLine)
		}
		return link
	case scm.AzureDevOpsPlatform:
		link := fmt.Sprintf("%s/commit/%s?path=/%s", remoteURL, commitSha, filePath)
		// Add line information if applicable
		if hasInnerPath {
			return link
		}
		if finding.StartLine != 0 {
			link += fmt.Sprintf("&line=%d", finding.StartLine)
		}
		if finding.EndLine != finding.StartLine {
			link += fmt.Sprintf("&lineEnd=%d", finding.EndLine)
		}
		// This is a bit dirty, but Azure DevOps does not highlight the line when the lineStartColumn and lineEndColumn are not provided
		link += "&lineStartColumn=1&lineEndColumn=10000000&type=2&lineStyle=plain&_a=files"
		return link
	case scm.GiteaPlatform:
		link := fmt.Sprintf("%s/src/commit/%s/%s", remoteURL, commitSha, filePath)
		if hasInnerPath {
			return link
		}
		ext := strings.ToLower(filepath.Ext(filePath))
		if ext == ".ipynb" || ext == ".md" {
			link += "?display=source"
		}
		if finding.StartLine != 0 {
			link += fmt.Sprintf("#L%d", finding.StartLine)
		}
		if finding.EndLine != finding.StartLine {
			link += fmt.Sprintf("-L%d", finding.EndLine)
		}
		return link
	case scm.BitbucketPlatform:
		link := fmt.Sprintf("%s/src/%s/%s", remoteURL, commitSha, filePath)
		if hasInnerPath {
			return link
		}
		if finding.StartLine != 0 {
			link += fmt.Sprintf("#lines-%d", finding.StartLine)
		}
		if finding.EndLine != finding.StartLine {
			link += fmt.Sprintf(":%d", finding.EndLine)
		}
		return link
	default:
		// This should never happen.
		return ""
	}
}

// shannonEntropy calculates the entropy of data using the formula defined here:
// https://en.wiktionary.org/wiki/Shannon_entropy
// Another way to think about what this is doing is calculating the number of bits
// needed to on average encode the data. So, the higher the entropy, the more random the data, the
// more bits needed to encode that data.
func shannonEntropy(data string) (entropy float64) {
	if data == "" {
		return 0
	}

	charCounts := make(map[rune]int)
	for _, char := range data {
		charCounts[char]++
	}

	invLength := 1.0 / float64(len(data))
	for _, count := range charCounts {
		freq := float64(count) * invLength
		entropy -= freq * math.Log2(freq)
	}

	return entropy
}

// filter will dedupe and redact findings
func filter(findings []report.Finding) []report.Finding {
	// Collect every required finding's (line, secret) so we can suppress
	// standalone duplicates that are already surfaced as components.
	requiredSet := make(map[string]struct{})
	for _, f := range findings {
		for _, set := range f.RequiredSets {
			for _, comp := range set.Components {
				requiredSet[fmt.Sprintf("%d:%s", comp.StartLine, comp.Secret)] = struct{}{}
			}
		}
	}

	var retFindings []report.Finding
	for _, f := range findings {
		include := true

		// Skip findings that are already surfaced as a required component
		// of another (composite) finding in this batch.
		if _, isRequired := requiredSet[fmt.Sprintf("%d:%s", f.StartLine, f.Secret)]; isRequired {
			redactedMatch := strings.ReplaceAll(f.Match, f.Secret, "REDACTED")
			logging.Trace().Msgf("skipping %s finding (%s), already a required component of another finding", f.RuleID, redactedMatch)
			include = false
		} else if isSuppressedByHigherSpecificityFinding(f, findings) {
			include = false
		}

		if include {
			retFindings = append(retFindings, f)
		}
	}
	return retFindings
}

func isSuppressedByHigherSpecificityFinding(f report.Finding, findings []report.Finding) bool {
	for _, fPrime := range findings {
		if f.StartLine == fPrime.StartLine &&
			f.Attributes[sources.AttrGitSHA] == fPrime.Attributes[sources.AttrGitSHA] &&
			f.RuleID != fPrime.RuleID &&
			strings.Contains(fPrime.Secret, f.Secret) &&
			fPrime.RuleSpecificity > f.RuleSpecificity {
			genericMatch := strings.ReplaceAll(f.Match, f.Secret, "REDACTED")
			betterMatch := strings.ReplaceAll(fPrime.Match, fPrime.Secret, "REDACTED")
			logging.Debug().Msgf("skipping %s finding (%s), %s rule takes precedence (%s)", f.RuleID, genericMatch, fPrime.RuleID, betterMatch)
			return true
		}
		for _, set := range fPrime.RequiredSets {
			for _, comp := range set.Components {
				if f.StartLine == comp.StartLine &&
					f.RuleID != comp.RuleID &&
					strings.Contains(comp.Secret, f.Secret) &&
					comp.RuleSpecificity > f.RuleSpecificity {
					genericMatch := strings.ReplaceAll(f.Match, f.Secret, "REDACTED")
					betterMatch := strings.ReplaceAll(comp.Match, comp.Secret, "REDACTED")
					logging.Trace().Msgf("skipping %s finding (%s), %s required component takes precedence (%s)", f.RuleID, genericMatch, comp.RuleID, betterMatch)
					return true
				}
			}
		}
	}
	return false
}

func printFinding(f report.Finding, noColor bool, redact uint, legacyPrint bool) {
	if legacyPrint {
		f.PrintLegacy(noColor, redact)
		return
	}
	f.Print(noColor, redact)
}

// stripEmptyMeta removes keys whose value is an empty string or nil.
func stripEmptyMeta(m map[string]any) map[string]any {
	if len(m) == 0 {
		return m
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		if v == nil {
			continue
		}
		out[k] = v
	}
	return out
}

// lowercaseBufPool provides reusable byte buffers for lowercasing strings
// without allocating a new string via strings.ToLower each time.
var lowercaseBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, 128*1024)
		return &buf
	},
}

// getLowerBuf returns an ASCII-lowercased copy of s in a pooled byte buffer.
// Caller must call putLowerBuf when done with the returned slice.
func getLowerBuf(s string) (*[]byte, []byte) {
	bp, buf := getLowerBufRaw(len(s))
	asciiLower(buf, s)
	return bp, buf
}

// getLowerBufRaw returns an uninitialized pooled buffer of length n for
// callers that fill it themselves (e.g. the fused lowercase+scan pass).
// Caller must call putLowerBuf when done with the returned slice.
func getLowerBufRaw(n int) (*[]byte, []byte) {
	bp := lowercaseBufPool.Get().(*[]byte)
	buf := *bp
	if cap(buf) < n {
		buf = make([]byte, n)
	} else {
		buf = buf[:n]
	}
	*bp = buf
	return bp, buf
}

// SWAR lane masks for asciiLower: one 0x01 / 0x80 / 0x7F per byte of a uint64.
const (
	swarOnes = 0x0101010101010101
	swarHigh = 0x8080808080808080
	swarLow7 = 0x7f7f7f7f7f7f7f7f
)

// asciiLower writes an ASCII-lowercased copy of s into dst (len(dst) == len(s)):
// bytes 'A'..'Z' gain 0x20, all others (including any byte >= 0x80) pass
// through unchanged. Eight bytes are folded per iteration with branchless SWAR;
// see the exhaustive equivalence proof in the package tests.
//
// Per byte b, gtByte(x,hi) sets bit7 where (b & 0x7f) > hi without borrowing
// across byte boundaries. The A–Z mask is (>0x40) AND NOT (>0x5A) AND (b <
// 0x80); shifting it right by 2 turns each qualifying 0x80 into the 0x20 to add.
func asciiLower(dst []byte, s string) {
	i, n := 0, len(s)
	for ; i+8 <= n; i += 8 {
		x := leUint64(s[i:])
		notHigh := ^x & swarHigh
		gt40 := ((x & swarLow7) + (swarLow7 - swarOnes*0x40)) & swarHigh
		gt5A := ((x & swarLow7) + (swarLow7 - swarOnes*0x5A)) & swarHigh
		mask := gt40 &^ gt5A & notHigh
		putLEUint64(dst[i:], x+(mask>>2))
	}
	for ; i < n; i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			dst[i] = c + 32
		} else {
			dst[i] = c
		}
	}
}

// leUint64 reads 8 bytes of s as a little-endian uint64. s must have len >= 8.
func leUint64(s string) uint64 {
	_ = s[7]
	return uint64(s[0]) | uint64(s[1])<<8 | uint64(s[2])<<16 | uint64(s[3])<<24 |
		uint64(s[4])<<32 | uint64(s[5])<<40 | uint64(s[6])<<48 | uint64(s[7])<<56
}

// putLEUint64 writes x as little-endian bytes into b. b must have len >= 8.
func putLEUint64(b []byte, x uint64) {
	_ = b[7]
	b[0] = byte(x)
	b[1] = byte(x >> 8)
	b[2] = byte(x >> 16)
	b[3] = byte(x >> 24)
	b[4] = byte(x >> 32)
	b[5] = byte(x >> 40)
	b[6] = byte(x >> 48)
	b[7] = byte(x >> 56)
}

func putLowerBuf(bp *[]byte) {
	lowercaseBufPool.Put(bp)
}

// findNewlineIndices returns the byte offsets of all newlines in s.
// This replaces the previous regex-based approach which was expensive
// when using go-re2 (WASM overhead for a literal \n search).
//
// A flat []int of offsets is returned rather than [][]int pairs: location()
// only ever reads the newline offset, so pairing it with offset+1 allocated a
// tiny slice per newline (hundreds of thousands over a large scan) for a value
// nothing consumed.
func findNewlineIndices(s string) []int {
	indices := make([]int, 0, strings.Count(s, "\n"))
	offset := 0
	for {
		i := strings.IndexByte(s[offset:], '\n')
		if i == -1 {
			break
		}
		idx := offset + i
		indices = append(indices, idx)
		offset = idx + 1
	}
	return indices
}

// allowSignatureGate is a substring of every entry in allowSignatures
// (asserted by test), so one scan rejects the common no-signature case
// instead of one full scan per signature. Finding lines can be huge
// (minified sources), making the per-signature scans measurable.
const allowSignatureGate = "leaks:allow"

// containsAllowSignature checks if the line contains any of the allow signatures
func containsAllowSignature(line string) bool {
	if !strings.Contains(line, allowSignatureGate) {
		return false
	}
	for _, sig := range allowSignatures {
		if strings.Contains(line, sig) {
			return true
		}
	}
	return false
}

// abs returns the absolute value of an integer
func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func RedactFindings(findings []report.Finding, percent uint) {
	if percent == 0 {
		return
	}
	for i := range findings {
		findings[i].Redact(percent)
	}
}
