package exprruntime

import (
	"sync"
	"testing"

	tiktoken "github.com/pkoukk/tiktoken-go"
)

// Compiled filter/prefilter programs are cached per Runtime and shared by
// every scan worker, so EvalFilter/EvalPrefilter on the SAME Program value
// run concurrently during scans. These tests pin that contract: they fail
// under -race (and can fail functionally: a racing eval used to reset the
// shared tokenizer mid-evaluation, flipping failsTokenEfficiency verdicts
// and making scan findings nondeterministic).

func testTokenizer(t *testing.T) *tiktoken.Tiktoken {
	t.Helper()
	tke, err := tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		t.Skipf("tokenizer unavailable: %v", err)
	}
	return tke
}

func TestEvalFilterConcurrentSharedProgram(t *testing.T) {
	rt, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	tke := testTokenizer(t)

	// Lazy provider mirrors Detector.Tokenizer: rule filters compile with a
	// nil tokenizer and resolve it through the provider at eval time.
	rt.SetTokenizerProvider(func() *tiktoken.Tiktoken { return tke })

	prg, err := rt.CompileFilter(`filter.failsTokenEfficiency(finding["secret"])`, nil)
	if err != nil {
		t.Fatal(err)
	}

	// "sk-test" is short with a real-word hit: deterministically true.
	finding := map[string]string{"secret": "sk-test"}

	const workers = 32
	const evals = 200
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < evals; i++ {
				got, err := rt.EvalFilter(prg, finding, nil)
				if err != nil {
					t.Errorf("EvalFilter: %v", err)
					return
				}
				if !got {
					// A racing eval that resets the shared tokenizer makes
					// failsTokenEfficiency bail out with false.
					t.Error("EvalFilter verdict flipped under concurrency")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestEvalPrefilterConcurrentSharedProgram(t *testing.T) {
	rt, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}

	prg, err := rt.CompilePrefilter(`matchesAny(get(attributes, "path", ""), ["\\.png$"])`)
	if err != nil {
		t.Fatal(err)
	}

	const workers = 32
	const evals = 500
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			attrs := map[string]string{"path": "img/logo.png"}
			if w%2 == 1 {
				attrs["path"] = "src/main.go"
			}
			want := w%2 == 0
			for i := 0; i < evals; i++ {
				got, err := rt.EvalPrefilter(prg, attrs)
				if err != nil {
					t.Errorf("EvalPrefilter: %v", err)
					return
				}
				if got != want {
					t.Errorf("EvalPrefilter = %v, want %v", got, want)
					return
				}
			}
		}(w)
	}
	wg.Wait()
}
