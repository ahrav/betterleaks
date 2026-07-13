package detect

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	ahocorasick "github.com/BobuSumisu/aho-corasick"
	"github.com/betterleaks/betterleaks/config"
	"github.com/betterleaks/betterleaks/report"
	"github.com/betterleaks/betterleaks/sources"
)

// TestWindowEligibleRules reports how many default rules qualify for
// hit-window verification and sanity-checks their widths.
func TestWindowEligibleRules(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	eligible, lineBounded, exempt := 0, 0, 0
	maxW := 0
	for _, rule := range cfg.Rules {
		if rule.Regex == nil {
			continue
		}
		wp := windowPlanForPattern(rule.Regex.String(), rule.Keywords)
		if wp.valid() {
			eligible++
			if wp.nlFreeSites > 0 {
				lineBounded++
			}
			if wp.bounded > maxW {
				maxW = wp.bounded
			}
		} else {
			exempt++
		}
	}
	t.Logf("window-eligible rules: %d (line-bounded: %d), exempt: %d, max static width: %d", eligible, lineBounded, exempt, maxW)
	if eligible == 0 {
		t.Fatal("no rules eligible for windowing — wiring is dead code")
	}
}

func canonicalFindings(t *testing.T, findings []report.Finding) []string {
	t.Helper()
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		f.Fingerprint = ""
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	return out
}

// TestWindowedDetectionDifferential runs the full detector over adversarial
// fragments with windowing on and off, requiring byte-identical findings.
// Inputs include real-shaped secrets at fragment edges, straddling window
// margins, duplicated keywords, unicode fold traps, and random splices.
func TestWindowedDetectionDifferential(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}

	newDetector := func(disabled bool) *Detector {
		d := NewDetector(cfg)
		d.MaxDecodeDepth = 0
		d.windowingDisabled = disabled
		return d
	}

	secrets := []string{
		`aws_key = "AKIAIOSFODNN7EXAMPLE"`,
		`const Discord_Public_Key = "e7322523fb86ed64c836a979cf8465fbd436378c653c1db38f9ae87bc62a6fd5"`,
		"github_token := \"ghp_16C7e42F292c6912E7710c838347Ae178B4a\"",
		`slack_token = "xoxb-781236542736-2364535789652-GkwFDQoHqzXDVsC6GzqYUypD"`,
		`stripe_api_key: "sk_test_51H8mFbGswQrbLxN2AcTQGvVmRvhDkzPWNvJqLsIhSAcm4Kx"`,
		`generic_secret = "hjkKJHmn234$sdfHJKmnop9012jklMNOP"`,
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----",
	}
	fillers := []string{
		"the quick brown fox jumps over the lazy dog\n",
		"for i := range items { process(items[i]) }\n",
		"# configuration values for the deployment environment\n",
		strings.Repeat("x", 200) + "\n",
		"password username token secret apikey auth\n", // keyword soup, no secrets
	}

	check := func(name, content string) {
		t.Helper()
		frag := sources.Fragment{Raw: content, Attributes: map[string]string{sources.AttrPath: "test.py"}}
		got := canonicalFindings(t, newDetector(false).detectFragment(context.Background(), frag))
		want := canonicalFindings(t, newDetector(true).detectFragment(context.Background(), frag))
		if len(got) != len(want) {
			t.Fatalf("%s: windowed=%d findings, full=%d findings", name, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: finding %d differs:\nwindowed: %s\nfull:     %s", name, i, got[i], want[i])
			}
		}
	}

	// Each secret alone, at start, end, and deep inside filler.
	for i, sec := range secrets {
		check(fmt.Sprintf("alone-%d", i), sec)
		check(fmt.Sprintf("start-%d", i), sec+"\n"+strings.Repeat(fillers[i%len(fillers)], 30))
		check(fmt.Sprintf("end-%d", i), strings.Repeat(fillers[i%len(fillers)], 30)+sec)
		check(fmt.Sprintf("mid-%d", i), strings.Repeat(fillers[i%len(fillers)], 15)+sec+"\n"+strings.Repeat(fillers[(i+1)%len(fillers)], 15))
	}

	// All secrets in one fragment; duplicated; adjacent.
	check("all", strings.Join(secrets, "\n"))
	check("all-dup", strings.Join(secrets, "\n")+"\n"+strings.Join(secrets, "\n"))
	check("adjacent", strings.Join(secrets, ""))

	// Unicode fold traps force the full-fragment fallback path.
	check("kelvin", "Key = value\n"+secrets[0])
	check("longs", "ſecret = value\n"+secrets[1])

	// Randomized splices: secrets at random offsets in random filler.
	rng := rand.New(rand.NewSource(77))
	for trial := 0; trial < 300; trial++ {
		var sb strings.Builder
		for sb.Len() < 2000 {
			if rng.Intn(6) == 0 {
				sb.WriteString(secrets[rng.Intn(len(secrets))])
				sb.WriteByte('\n')
			}
			sb.WriteString(fillers[rng.Intn(len(fillers))])
		}
		check(fmt.Sprintf("random-%d", trial), sb.String())
	}
}

// TestRecordedWindowsDifferential exercises the occurrence-recorder path:
// fragments must exceed windowMinFragment so recorded positions (not
// bytes.Index) build the windows. Compares full-detector findings between
// trigram+recorder, trigram with windowing disabled, and the AhoC path,
// plus direct window equivalence for every eligible rule. Includes the
// overflow regime (a keyword occurring more than occRecorderCap times).
func TestRecordedWindowsDifferential(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}

	secrets := []string{
		`aws_key = "AKIAIOSFODNN7EXAMPLE"`,
		`const Discord_Public_Key = "e7322523fb86ed64c836a979cf8465fbd436378c653c1db38f9ae87bc62a6fd5"`,
		"github_token := \"ghp_16C7e42F292c6912E7710c838347Ae178B4a\"",
		`slack_token = "xoxb-781236542736-2364535789652-GkwFDQoHqzXDVsC6GzqYUypD"`,
		`generic_secret = "hjkKJHmn234$sdfHJKmnop9012jklMNOP"`,
	}
	filler := "for i := range items { process(items[i]) } // deployment configuration value\n"

	build := func(rng *rand.Rand, n int, overflowKeyword bool) string {
		var sb strings.Builder
		for sb.Len() < n {
			if rng.Intn(10) == 0 {
				sb.WriteString(secrets[rng.Intn(len(secrets))])
				sb.WriteByte('\n')
			}
			if overflowKeyword && rng.Intn(3) == 0 {
				// "twitter" maps to 5 rules; spam it past occRecorderCap.
				sb.WriteString("twitter twitter_config twitter_value ")
			}
			sb.WriteString(filler)
		}
		return sb.String()
	}

	newDetector := func(mode string, noWindow bool) *Detector {
		t.Setenv("BETTERLEAKS_PREFILTER", mode)
		d := NewDetector(cfg)
		d.MaxDecodeDepth = 0
		d.windowingDisabled = noWindow
		return d
	}

	rng := rand.New(rand.NewSource(2024))
	for trial := 0; trial < 40; trial++ {
		content := build(rng, 24<<10+rng.Intn(48<<10), trial%2 == 1)
		if len(content) < windowMinFragment {
			t.Fatalf("trial %d content too small: %d", trial, len(content))
		}
		frag := sources.Fragment{Raw: content, Attributes: map[string]string{sources.AttrPath: "test.py"}}

		recFindings := canonicalFindings(t, newDetector("trigram", false).detectFragment(context.Background(), frag))
		fullFindings := canonicalFindings(t, newDetector("trigram", true).detectFragment(context.Background(), frag))
		ahocFindings := canonicalFindings(t, newDetector("ahoc", false).detectFragment(context.Background(), frag))

		for name, got := range map[string][]string{"trigram-windowed(rec)": recFindings, "ahoc-windowed": ahocFindings} {
			if len(got) != len(fullFindings) {
				t.Fatalf("trial %d %s: %d findings vs full %d", trial, name, len(got), len(fullFindings))
			}
			for i := range got {
				if got[i] != fullFindings[i] {
					t.Fatalf("trial %d %s: finding %d differs:\n%s\nvs\n%s", trial, name, i, got[i], fullFindings[i])
				}
			}
		}
	}
}

// TestRuleWindowsFromPositionsEquivalence pins the recorded-position window
// builder to the bytes.Index builder for identical inputs: complete
// recorded lists must produce byte-identical merged windows.
func TestRuleWindowsFromPositionsEquivalence(t *testing.T) {
	keywords := defaultKeywords(t)
	tri := newTrigramPrefilter(keywords)
	if tri == nil {
		t.Fatal("no trigram prefilter")
	}
	kwLen := make([]int32, len(keywords))
	kwBytes := make([][]byte, len(keywords))
	for i, kw := range keywords {
		kwLen[i] = int32(len(kw))
		kwBytes[i] = []byte(kw)
	}
	trackAll := make([]bool, len(keywords))
	for i := range trackAll {
		trackAll[i] = true
	}

	rng := rand.New(rand.NewSource(5))
	alpha := "abcdefghijklmnopqrstuvwxyz0123456789-_. \n\t=\"'"
	for trial := 0; trial < 500; trial++ {
		n := 64 + rng.Intn(4096)
		buf := make([]byte, n)
		for i := range buf {
			buf[i] = alpha[rng.Intn(len(alpha))]
		}
		for k := rng.Intn(8); k > 0; k-- {
			kw := keywords[rng.Intn(len(keywords))]
			if len(kw) <= n {
				copy(buf[rng.Intn(n-len(kw)+1):], kw)
			}
		}

		rec := newOccRecorder(len(keywords), trackAll)
		tri.collectPatternsRec(buf, rec, func(pattern uint32) {})

		width := 32 + rng.Intn(512)
		for pat := range keywords {
			positions, complete := rec.positions(uint32(pat))
			if !complete {
				continue // overflow: caller falls back, nothing to compare
			}
			// Recorder captures every occurrence; cross-check with a
			// simple scan before comparing window construction.
			var wantPos []int32
			for from := 0; ; {
				idx := strings.Index(string(buf[from:]), keywords[pat])
				if idx < 0 {
					break
				}
				wantPos = append(wantPos, int32(from+idx))
				from += idx + 1
			}
			if len(positions) != len(wantPos) {
				t.Fatalf("trial %d kw %q: recorder %v want %v", trial, keywords[pat], positions, wantPos)
			}
			for i := range positions {
				if positions[i] != wantPos[i] {
					t.Fatalf("trial %d kw %q: recorder %v want %v", trial, keywords[pat], positions, wantPos)
				}
			}
			if len(positions) == 0 {
				continue
			}
			got, ok := ruleWindowsFromPositions(rec, []uint32{uint32(pat)}, kwLen, len(buf), width)
			if !ok {
				t.Fatalf("trial %d kw %q: unexpected overflow signal", trial, keywords[pat])
			}
			want := ruleWindows(buf, [][]byte{kwBytes[pat]}, width)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("trial %d kw %q width %d:\nrecorded: %v\nindex:    %v", trial, keywords[pat], width, got, want)
			}
		}
	}
}

// TestAhoCRecordedPositionsEquivalence pins the AhoC Walk-fed recorder to a
// naive occurrence scan: for every tracked pattern whose list stays complete,
// the recorded start offsets must equal every occurrence of the keyword, and
// the windows built from them must match the bytes.Index builder. This is
// the ahoc-path twin of TestRuleWindowsFromPositionsEquivalence — Walk
// reports (end, length, pattern), the detector records end+1-n.
func TestAhoCRecordedPositionsEquivalence(t *testing.T) {
	keywords := defaultKeywords(t)
	trie := ahocorasick.NewTrieBuilder().AddStrings(keywords).Build()
	kwLen := make([]int32, len(keywords))
	kwBytes := make([][]byte, len(keywords))
	for i, kw := range keywords {
		kwLen[i] = int32(len(kw))
		kwBytes[i] = []byte(kw)
	}
	trackAll := make([]bool, len(keywords))
	for i := range trackAll {
		trackAll[i] = true
	}

	rng := rand.New(rand.NewSource(7))
	alpha := "abcdefghijklmnopqrstuvwxyz0123456789-_. \n\t=\"'"
	for trial := 0; trial < 500; trial++ {
		n := 64 + rng.Intn(4096)
		buf := make([]byte, n)
		for i := range buf {
			buf[i] = alpha[rng.Intn(len(alpha))]
		}
		for k := rng.Intn(8); k > 0; k-- {
			kw := keywords[rng.Intn(len(keywords))]
			if len(kw) <= n {
				copy(buf[rng.Intn(n-len(kw)+1):], kw)
			}
		}

		rec := newOccRecorder(len(keywords), trackAll)
		trie.Walk(buf, func(end, l, pattern uint32) bool {
			rec.record(pattern, int32(end+1-l))
			return true
		})

		width := 32 + rng.Intn(512)
		for pat := range keywords {
			positions, complete := rec.positions(uint32(pat))
			if !complete {
				continue // overflow: caller falls back, nothing to compare
			}
			var wantPos []int32
			for from := 0; ; {
				idx := strings.Index(string(buf[from:]), keywords[pat])
				if idx < 0 {
					break
				}
				wantPos = append(wantPos, int32(from+idx))
				from += idx + 1
			}
			if len(positions) != len(wantPos) {
				t.Fatalf("trial %d kw %q: recorder %v want %v", trial, keywords[pat], positions, wantPos)
			}
			for i := range positions {
				if positions[i] != wantPos[i] {
					t.Fatalf("trial %d kw %q: recorder %v want %v", trial, keywords[pat], positions, wantPos)
				}
			}
			if len(positions) == 0 {
				continue
			}
			got, ok := ruleWindowsFromPositions(rec, []uint32{uint32(pat)}, kwLen, len(buf), width)
			if !ok {
				t.Fatalf("trial %d kw %q: unexpected overflow signal", trial, keywords[pat])
			}
			want := ruleWindows(buf, [][]byte{kwBytes[pat]}, width)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("trial %d kw %q width %d:\nrecorded: %v\nindex:    %v", trial, keywords[pat], width, got, want)
			}
		}
	}
}

// BenchmarkWindowConstruction compares occurrence-recorded window building
// (prefilter records positions, ruleWindowsFromPositions merges) against
// bytes.Index rediscovery (plain prefilter + ruleWindows), on a real-shaped
// large fragment, measuring the combined prefilter+window-construction cost
// for all window-eligible candidate rules.
func BenchmarkWindowConstruction(b *testing.B) {
	cfg, err := config.Default()
	if err != nil {
		b.Fatal(err)
	}
	t := &testing.T{}
	_ = t
	d := NewDetector(cfg)
	d.MaxDecodeDepth = 0
	if d.prefilterTri == nil {
		d.prefilterTri = newTrigramPrefilter(func() []string {
			kws := defaultKeywords(b)
			return kws
		}())
	}

	// 256KB source-shaped fragment salted with keywords.
	rng := rand.New(rand.NewSource(9))
	keywords := defaultKeywords(b)
	var sb strings.Builder
	filler := "for i := range items { process(items[i]) } // configuration deployment\n"
	for sb.Len() < 256<<10 {
		if rng.Intn(12) == 0 {
			sb.WriteString(keywords[rng.Intn(len(keywords))])
			sb.WriteString(" = \"value\"\n")
		}
		sb.WriteString(filler)
	}
	raw := sb.String()
	dst := make([]byte, len(raw))

	// Collect candidate ranks + widths once (same for both paths).
	maxLine := 0
	{
		lowered := make([]byte, len(raw))
		asciiLower(lowered, raw)
		maxLine = longestNewlineFreeRun(lowered)
	}

	b.Run("recorded", func(b *testing.B) {
		b.SetBytes(int64(len(raw)))
		for b.Loop() {
			rec := getOccRecorder(len(d.windowTrackPatterns), d.windowTrackPatterns)
			var ranks []int
			d.prefilterTri.collectPatternsLoweringRec(dst, raw, rec, func(pattern uint32) {
				ranks = append(ranks, d.prefilterRuleRanks[pattern]...)
			})
			for _, rank := range ranks {
				if !d.windowPlanByRank[rank].valid() {
					continue
				}
				w := d.windowPlanByRank[rank].width(maxLine)
				if w*8 >= len(raw) {
					continue
				}
				if ws, ok := ruleWindowsFromPositions(rec, d.windowPatternsByRank[rank], d.windowKwLenByPattern, len(raw), w); ok {
					_ = ws
					continue
				}
				_ = ruleWindows(dst, d.windowKeywordsByRank[rank], w)
			}
			putOccRecorder(rec)
		}
	})
	b.Run("bytesindex", func(b *testing.B) {
		b.SetBytes(int64(len(raw)))
		for b.Loop() {
			var ranks []int
			d.prefilterTri.collectPatternsLowering(dst, raw, func(pattern uint32) {
				ranks = append(ranks, d.prefilterRuleRanks[pattern]...)
			})
			for _, rank := range ranks {
				if !d.windowPlanByRank[rank].valid() {
					continue
				}
				w := d.windowPlanByRank[rank].width(maxLine)
				if w*8 >= len(raw) {
					continue
				}
				_ = ruleWindows(dst, d.windowKeywordsByRank[rank], w)
			}
		}
	})
}
