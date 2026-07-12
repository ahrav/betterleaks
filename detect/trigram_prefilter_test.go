package detect

import (
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	ahocorasick "github.com/BobuSumisu/aho-corasick"

	"github.com/betterleaks/betterleaks/config"
	"golang.org/x/exp/maps"
)

// collectTrigram returns the sorted distinct pattern set the trigram
// prefilter reports for input.
func collectTrigram(p *trigramPrefilter, input []byte) []int {
	seen := map[uint32]bool{}
	p.collectPatterns(input, func(pattern uint32) { seen[pattern] = true })
	out := make([]int, 0, len(seen))
	for pat := range seen {
		out = append(out, int(pat))
	}
	sort.Ints(out)
	return out
}

// collectAhoC returns the sorted distinct pattern set the production
// Aho-Corasick trie reports for input.
func collectAhoC(tr *ahocorasick.Trie, input []byte) []int {
	seen := map[uint32]bool{}
	tr.Walk(input, func(end, n, pattern uint32) bool {
		seen[pattern] = true
		return true
	})
	out := make([]int, 0, len(seen))
	for pat := range seen {
		out = append(out, int(pat))
	}
	sort.Ints(out)
	return out
}

func defaultKeywords(t testing.TB) []string {
	t.Helper()
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	keywords := maps.Keys(cfg.Keywords)
	sort.Strings(keywords)
	return keywords
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestTrigramPrefilterDifferential drives both prefilters with adversarial
// inputs — keyword-dense text, keywords at buffer boundaries, overlapping
// keywords, and random mutations — and requires identical distinct-pattern
// sets. The trigram scanner reports a strict "keyword occurs as substring"
// predicate, same as the AhoC walk.
func TestTrigramPrefilterDifferential(t *testing.T) {
	keywords := defaultKeywords(t)
	tri := newTrigramPrefilter(keywords)
	if tri == nil {
		t.Fatal("trigram prefilter unavailable for default keyword set")
	}
	trie := ahocorasick.NewTrieBuilder().AddStrings(keywords).Build()

	collectFused := func(input []byte) ([]int, []byte) {
		seen := map[uint32]bool{}
		dst := make([]byte, len(input))
		tri.collectPatternsLowering(dst, string(input), func(pattern uint32) { seen[pattern] = true })
		out := make([]int, 0, len(seen))
		for pat := range seen {
			out = append(out, int(pat))
		}
		sort.Ints(out)
		return out, dst
	}

	check := func(name string, input []byte) {
		t.Helper()
		lowered := make([]byte, len(input))
		asciiLower(lowered, string(input))
		got := collectTrigram(tri, lowered)
		want := collectAhoC(trie, lowered)
		if !equalInts(got, want) {
			t.Fatalf("%s: trigram=%v ahoc=%v", name, got, want)
		}
		gotFused, dst := collectFused(input)
		if !equalInts(gotFused, want) {
			t.Fatalf("%s: fused=%v ahoc=%v", name, gotFused, want)
		}
		if string(dst) != string(lowered) {
			t.Fatalf("%s: fused lowering mismatch: %q vs %q", name, dst, lowered)
		}
	}

	check("empty", nil)
	check("short", []byte("ab"))
	check("exactly-3", []byte("api"))

	// Every keyword alone, at start, middle, end, and truncated.
	for _, kw := range keywords {
		check("alone/"+kw, []byte(kw))
		check("embedded/"+kw, []byte("x "+kw+" y"))
		check("prefix-only/"+kw, []byte(kw[:len(kw)-1]))
		check("end/"+kw, []byte("padpadpad"+kw))
	}

	// All keywords concatenated with and without separators (overlap soup).
	check("all-sep", []byte(strings.Join(keywords, " ")))
	check("all-cat", []byte(strings.Join(keywords, "")))

	// Randomized: splice random keywords into random ASCII at random
	// positions, plus random mutations that may destroy or create hits.
	rng := rand.New(rand.NewSource(42))
	alpha := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.~{};=<'\" \n\t\x80\xff\x00"
	for trial := 0; trial < 2000; trial++ {
		n := 1 + rng.Intn(300)
		buf := make([]byte, n)
		for i := range buf {
			buf[i] = alpha[rng.Intn(len(alpha))]
		}
		for k := rng.Intn(4); k > 0; k-- {
			kw := keywords[rng.Intn(len(keywords))]
			if len(kw) <= n {
				pos := rng.Intn(n - len(kw) + 1)
				copy(buf[pos:], kw)
			}
		}
		for m := rng.Intn(3); m > 0; m-- {
			buf[rng.Intn(n)] = alpha[rng.Intn(len(alpha))]
		}
		check("random", buf)
	}
}

func BenchmarkPrefilterWalk(b *testing.B) {
	keywords := defaultKeywords(b)
	tri := newTrigramPrefilter(keywords)
	trie := ahocorasick.NewTrieBuilder().AddStrings(keywords).Build()

	// Corpus-shaped input: source-like text salted with a few keywords.
	var sb strings.Builder
	rng := rand.New(rand.NewSource(7))
	words := []string{"func", "return", "if", "err", "nil", "for", "range", "int", "string", "case", "break", "value", "count", "index", "buffer", "config", "result"}
	for sb.Len() < 256<<10 {
		sb.WriteString(words[rng.Intn(len(words))])
		if rng.Intn(40) == 0 {
			sb.WriteString(" " + keywords[rng.Intn(len(keywords))] + " ")
		}
		if rng.Intn(8) == 0 {
			sb.WriteString("\n\t")
		} else {
			sb.WriteByte(' ')
		}
	}
	input := []byte(strings.ToLower(sb.String()))

	b.Run("ahoc", func(b *testing.B) {
		b.SetBytes(int64(len(input)))
		for b.Loop() {
			trie.Walk(input, func(end, n, pattern uint32) bool { return true })
		}
	})
	b.Run("trigram", func(b *testing.B) {
		b.SetBytes(int64(len(input)))
		for b.Loop() {
			tri.collectPatterns(input, func(pattern uint32) {})
		}
	})
	// End-to-end prefilter stage comparison including the lowercase pass:
	// production does asciiLower + walk; fused does one combined pass.
	raw := string(input)
	dst := make([]byte, len(input))
	b.Run("lower+ahoc", func(b *testing.B) {
		b.SetBytes(int64(len(input)))
		for b.Loop() {
			asciiLower(dst, raw)
			trie.Walk(dst, func(end, n, pattern uint32) bool { return true })
		}
	})
	b.Run("lower+trigram", func(b *testing.B) {
		b.SetBytes(int64(len(input)))
		for b.Loop() {
			asciiLower(dst, raw)
			tri.collectPatterns(dst, func(pattern uint32) {})
		}
	})
	b.Run("fused-trigram", func(b *testing.B) {
		b.SetBytes(int64(len(input)))
		for b.Loop() {
			tri.collectPatternsLowering(dst, raw, func(pattern uint32) {})
		}
	})
}

// BenchmarkPrefilterRealCorpus compares the full prefilter stage
// (lowercase + distinct-keyword collection) on real corpus files rather
// than synthetic text — synthetic word soup under-represents trigram
// bitmap-hit density and over-represents pruning benefit.
func BenchmarkPrefilterRealCorpus(b *testing.B) {
	keywords := defaultKeywords(b)
	tri := newTrigramPrefilter(keywords)
	trie := ahocorasick.NewTrieBuilder().AddStrings(keywords).Build()

	var inputs []string
	var total int
	filepath.Walk("/local/home/ahrav/scratch/corpora/cpython", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			if info != nil && info.IsDir() && info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if total >= 8<<20 || info.Size() > 1<<20 || info.Size() == 0 {
			return nil
		}
		switch filepath.Ext(path) {
		case ".py", ".c", ".h", ".rst", ".txt":
		default:
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		inputs = append(inputs, string(data))
		total += len(data)
		return nil
	})
	b.Logf("real corpus: %d files, %d bytes", len(inputs), total)

	dst := make([]byte, 1<<20)
	b.Run("lower+ahoc", func(b *testing.B) {
		b.SetBytes(int64(total))
		for b.Loop() {
			for _, in := range inputs {
				d := dst[:len(in)]
				asciiLower(d, in)
				trie.Walk(d, func(end, n, pattern uint32) bool { return true })
			}
		}
	})
	b.Run("fused-trigram", func(b *testing.B) {
		b.SetBytes(int64(total))
		for b.Loop() {
			for _, in := range inputs {
				tri.collectPatternsLowering(dst[:len(in)], in, func(pattern uint32) {})
			}
		}
	})
}
