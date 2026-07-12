package codec

import (
	"math/rand"
	"testing"
)

// TestBoringSkipInvariant asserts the skip's soundness condition: a boring
// byte must fail every dispatch condition in findEncodingMatches.
func TestBoringSkipInvariant(t *testing.T) {
	for c := 0; c < 256; c++ {
		if !isBoring[c] {
			continue
		}
		if c == '%' || c == '\\' || c == 'U' {
			t.Fatalf("anchor byte %q marked boring", byte(c))
		}
		if isB64Char[c] {
			t.Fatalf("b64 byte %q marked boring", byte(c))
		}
	}
	// And conversely, every non-boring byte is actionable by some branch.
	for c := 0; c < 256; c++ {
		if isBoring[c] {
			continue
		}
		if !isB64Char[c] && c != '%' && c != '\\' {
			t.Fatalf("byte %q not boring but no branch handles it", byte(c))
		}
	}
}

// TestFindEncodingMatchesRandomized exercises the scanner on adversarial
// random inputs mixing anchors, hex/b64 runs, and boring bytes, checking
// basic well-formedness (ordered, in-bounds, non-overlapping after filter).
func TestFindEncodingMatchesRandomized(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	alpha := []byte("%\\Uu+=abcdefABCDEF0123456789ghXYZ_/- \t\n\r.:;'\"")
	for trial := 0; trial < 3000; trial++ {
		n := rng.Intn(512)
		buf := make([]byte, n)
		for i := range buf {
			buf[i] = alpha[rng.Intn(len(alpha))]
		}
		matches := findEncodingMatches(string(buf))
		prevStart := -1
		for _, m := range matches {
			if m.start < 0 || m.end > n || m.start >= m.end {
				t.Fatalf("trial %d: out-of-bounds match %+v on %q", trial, m.startEnd, buf)
			}
			if m.start < prevStart {
				t.Fatalf("trial %d: matches out of order on %q", trial, buf)
			}
			prevStart = m.start
		}
	}
}

func BenchmarkFindEncodingMatches(b *testing.B) {
	// Source-shaped input: mostly identifiers/whitespace/punctuation, a few
	// hex and b64 runs.
	rng := rand.New(rand.NewSource(3))
	words := []string{"func", "return", "if err != nil {", "\tresult :=", "// comment about behavior", "value.Method(arg, other)", "for i := range items {"}
	var sb []byte
	for len(sb) < 256<<10 {
		sb = append(sb, words[rng.Intn(len(words))]...)
		sb = append(sb, ' ')
		if rng.Intn(50) == 0 {
			sb = append(sb, "deadbeefcafe0123456789abcdef0123456789abcdef "...)
		}
		if rng.Intn(60) == 0 {
			sb = append(sb, "bG9uZ2VyLWVuY29kZWQtc2VjcmV0LXRlc3Q= "...)
		}
		if rng.Intn(9) == 0 {
			sb = append(sb, '\n')
		}
	}
	data := string(sb)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		findEncodingMatches(data)
	}
}

func BenchmarkFindEncodingMatchesNoSkip(b *testing.B) {
	boringSkipEnabled = false
	defer func() { boringSkipEnabled = true }()
	rng := rand.New(rand.NewSource(3))
	words := []string{"func", "return", "if err != nil {", "\tresult :=", "// comment about behavior", "value.Method(arg, other)", "for i := range items {"}
	var sb []byte
	for len(sb) < 256<<10 {
		sb = append(sb, words[rng.Intn(len(words))]...)
		sb = append(sb, ' ')
		if rng.Intn(50) == 0 {
			sb = append(sb, "deadbeefcafe0123456789abcdef0123456789abcdef "...)
		}
		if rng.Intn(60) == 0 {
			sb = append(sb, "bG9uZ2VyLWVuY29kZWQtc2VjcmV0LXRlc3Q= "...)
		}
		if rng.Intn(9) == 0 {
			sb = append(sb, '\n')
		}
	}
	data := string(sb)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		findEncodingMatches(data)
	}
}
