package sources

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fatih/semgroup"
)

func fastBatchTestPatch(files int) string {
	var b strings.Builder
	for i := range files {
		fmt.Fprintf(&b, "diff --git a/f%03d.txt b/f%03d.txt\n", i, i)
		fmt.Fprintf(&b, "--- a/f%03d.txt\n+++ b/f%03d.txt\n", i, i)
		b.WriteString("@@ -0,0 +1 @@\n+value\n")
	}
	return b.String()
}

func TestAsyncFastGitLogBatchesCancellationClosesBlockedProducer(t *testing.T) {
	tests := []struct {
		name  string
		files int
	}{
		{name: "full-send", files: 128},
		{name: "final-partial-send", files: 65},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			batches := asyncFastGitLogBatches(ctx, strings.NewReader(fastBatchTestPatch(tt.files)), fastGitBatchSize)
			cancel()

			deadline := time.After(time.Second)
			for {
				select {
				case _, open := <-batches:
					if !open {
						return
					}
				case <-deadline:
					t.Fatal("producer stayed blocked after cancellation")
				}
			}
		})
	}
}

func TestGitFragmentsCancelsBatchProducerWithDifferentScanContext(t *testing.T) {
	commandCtx, cancelCommand := context.WithCancel(context.Background())
	defer cancelCommand()

	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error)
	close(errCh)
	gitCmd := &GitCmd{cmd: cmd, errCh: errCh}
	configureFastGitScan(commandCtx, gitCmd, strings.NewReader(fastBatchTestPatch(128)))

	scanCtx, cancelScan := context.WithCancel(context.Background())
	cancelScan()
	source := &Git{Cmd: gitCmd, Sema: semgroup.NewGroup(scanCtx, 1)}
	if err := source.Fragments(scanCtx, func(Fragment, error) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("Fragments error = %v, want context.Canceled", err)
	}

	select {
	case _, open := <-gitCmd.fastBatchesCh:
		if open {
			t.Fatal("producer emitted a batch after Fragments canceled its private parser context")
		}
	case <-time.After(time.Second):
		t.Fatal("producer survived after Fragments returned with a different canceled context")
	}
}

func TestConsumeFastBatchesChecksContextBetweenFiles(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	header := &fastGitHeader{sha: "0123456789012345678901234567890123456789"}
	batch := make([]fastGitFile, 64)
	for i := range batch {
		batch[i] = fastGitFile{header: header, newName: fmt.Sprintf("f%02d.txt", i)}
	}
	batches := make(chan []fastGitFile, 1)
	batches <- batch
	close(batches)
	errCh := make(chan error)
	close(errCh)

	var calls atomic.Int32
	source := &Git{
		Cmd:  &GitCmd{fastBatchesCh: batches, errCh: errCh},
		Sema: semgroup.NewGroup(ctx, 1),
		ShouldSkip: func(map[string]string) bool {
			if calls.Add(1) == 1 {
				cancel()
			}
			return true
		},
	}
	var wg sync.WaitGroup
	err := source.consumeFastBatches(ctx, func(Fragment, error) error { return nil }, &wg)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("consumeFastBatches error = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("prefilter calls after first file canceled context = %d, want 1", got)
	}
}
