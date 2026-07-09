package sources

import (
	"strings"
	"testing"

	"github.com/gitleaks/go-gitdiff/gitdiff"
)

var rawAddedTextSink string

func TestRawAddedTextMatchesGitdiff(t *testing.T) {
	tests := map[string]*gitdiff.TextFragment{
		"single-add": {
			Lines: []gitdiff.Line{{Op: gitdiff.OpAdd, Line: "secret=value\n"}},
		},
		"multiple-adds": {
			Lines: []gitdiff.Line{
				{Op: gitdiff.OpAdd, Line: "one\n"},
				{Op: gitdiff.OpAdd, Line: "two\n"},
			},
		},
		"mixed": {
			Lines: []gitdiff.Line{
				{Op: gitdiff.OpContext, Line: "context\n"},
				{Op: gitdiff.OpAdd, Line: "added\n"},
				{Op: gitdiff.OpDelete, Line: "deleted\n"},
			},
		},
		"no-adds": {
			Lines: []gitdiff.Line{{Op: gitdiff.OpDelete, Line: "deleted\n"}},
		},
	}

	for name, tf := range tests {
		t.Run(name, func(t *testing.T) {
			got := rawAddedText(tf)
			want := tf.Raw(gitdiff.OpAdd)
			if got != want {
				t.Fatalf("rawAddedText() = %q, want %q", got, want)
			}
		})
	}
}

func BenchmarkGitdiffRawSingleAddLine(b *testing.B) {
	payload := strings.Repeat("secret=value\n", 128)
	tf := &gitdiff.TextFragment{
		Lines: []gitdiff.Line{{Op: gitdiff.OpAdd, Line: payload}},
	}
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rawAddedTextSink = tf.Raw(gitdiff.OpAdd)
	}
}

func BenchmarkRawAddedTextSingleAddLine(b *testing.B) {
	payload := strings.Repeat("secret=value\n", 128)
	tf := &gitdiff.TextFragment{
		Lines: []gitdiff.Line{{Op: gitdiff.OpAdd, Line: payload}},
	}
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rawAddedTextSink = rawAddedText(tf)
	}
}
