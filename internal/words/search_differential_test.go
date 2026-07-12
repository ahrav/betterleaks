package words

import (
	"math/rand"
	"testing"
)

// TestHasAnyMatchDifferential pins HasAnyMatchInList to the boolean
// projection of HasMatchInList across random and adversarial inputs.
func TestHasAnyMatchDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	alpha := "abcdefghijklmnopqrstuvwxyzABC0189-_."
	cases := []string{"", "a", "apple", "xkcdq", "applepie", "zzzzzzzz", "the-quick-brown-fox", "AKIA1234567890ABCDEF"}
	for trial := 0; trial < 20000; trial++ {
		n := rng.Intn(40)
		buf := make([]byte, n)
		for i := range buf {
			buf[i] = alpha[rng.Intn(len(alpha))]
		}
		cases = append(cases[:0], string(buf))
		for _, w := range cases {
			for _, minLen := range []int{4, 5} {
				want := len(HasMatchInList(w, minLen)) > 0
				got := HasAnyMatchInList(w, minLen)
				if got != want {
					t.Fatalf("mismatch for %q minLen=%d: got %v want %v", w, minLen, got, want)
				}
			}
		}
	}
}

func BenchmarkWordMatch(b *testing.B) {
	secrets := []string{
		"kJ8xQ2mNpL5vR9wT3yU7iO1eA6sD4fG0",
		"database-connection-string-here",
		"XyZ123abcDEF456ghiJKL789mnoPQR0",
		"supersecretpassword123",
		"9f8e7d6c5b4a39281706f5e4d3c2b1a0",
	}
	b.Run("full", func(b *testing.B) {
		for b.Loop() {
			for _, s := range secrets {
				HasMatchInList(s, 5)
			}
		}
	})
	b.Run("any", func(b *testing.B) {
		for b.Loop() {
			for _, s := range secrets {
				HasAnyMatchInList(s, 5)
			}
		}
	})
}
