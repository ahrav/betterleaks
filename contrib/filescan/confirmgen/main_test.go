package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestGenerateCorpusDeterministic(t *testing.T) {
	workspace := t.TempDir()
	spec := corpusSpec{
		Profile:         "test-profile",
		Shape:           "wide",
		SmallFiles:      17,
		Groups:          3,
		SequentialFiles: 2,
		SequentialBytes: 4_097,
		Archives:        2,
		ArchiveEntries:  3,
	}
	first := filepath.Join(workspace, "first")
	second := filepath.Join(workspace, "second")
	t.Cleanup(func() {
		makeTreeWritable(first)
		makeTreeWritable(second)
	})
	if err := generateCorpus(context.Background(), first, spec, 17, 2); err != nil {
		t.Fatal(err)
	}
	if err := generateCorpus(context.Background(), second, spec, 17, 2); err != nil {
		t.Fatal(err)
	}
	firstDigests := treeDigests(t, first)
	secondDigests := treeDigests(t, second)
	if !reflect.DeepEqual(firstDigests, secondDigests) {
		t.Fatalf("generated trees differ\nfirst:  %#v\nsecond: %#v", firstDigests, secondDigests)
	}
	if got, want := len(firstDigests), 22; got != want {
		t.Fatalf("file count = %d, want %d", got, want)
	}

	data, err := os.ReadFile(filepath.Join(first, "GENERATION.json"))
	if err != nil {
		t.Fatal(err)
	}
	var summary generationSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.GeneratedFiles != 21 {
		t.Fatalf("generated files = %d, want 21", summary.GeneratedFiles)
	}
	if summary.Profile != spec.Profile || summary.Seed != 17 || summary.GeneratorSHA256 == "" {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.Generator != "filescan-confirmgen-v2" {
		t.Fatalf("generator = %q, want v2", summary.Generator)
	}
}

func TestGeneratedModesIgnoreUmask(t *testing.T) {
	workspace := t.TempDir()
	output := filepath.Join(workspace, "corpus")
	t.Cleanup(func() { makeTreeWritable(output) })
	oldUmask := unix.Umask(0o077)
	defer unix.Umask(oldUmask)
	spec := corpusSpec{
		Profile: "mode-test", Shape: "wide", SmallFiles: 1, Groups: 1,
		SequentialFiles: 1, SequentialBytes: 17, Archives: 1, ArchiveEntries: 1,
	}
	if err := generateCorpus(context.Background(), output, spec, 19, 2); err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(output, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		want := os.FileMode(0o444)
		if entry.IsDir() {
			want = 0o555
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("mode %s = %#o, want %#o", path, got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGenerateCorpusCancellationCleansPartialTree(t *testing.T) {
	workspace := t.TempDir()
	output := filepath.Join(workspace, "canceled")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := generateCorpus(
		ctx,
		output,
		corpusSpec{Profile: "cancel-test", Shape: "wide", SmallFiles: 10, Groups: 2},
		23,
		2,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output stat error = %v, want not-exist", err)
	}
	partials, err := filepath.Glob(filepath.Join(workspace, ".canceled.partial-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(partials) != 0 {
		t.Fatalf("partial trees remain: %v", partials)
	}
}

func TestWriteRepeatedChecksCancellationBetweenChunks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "canceled.log")
	ctx := &cancelOnSecondCheckContext{done: make(chan struct{})}
	if err := writeRepeated(ctx, path, 'x', 2*sequentialBufferBytes); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if ctx.checks != 2 {
		t.Fatalf("context checks = %d, want 2", ctx.checks)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial file stat error = %v, want not-exist", err)
	}
}

type cancelOnSecondCheckContext struct {
	checks int
	done   chan struct{}
}

func (*cancelOnSecondCheckContext) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

func (ctx *cancelOnSecondCheckContext) Done() <-chan struct{} {
	return ctx.done
}

func (ctx *cancelOnSecondCheckContext) Err() error {
	ctx.checks++
	if ctx.checks < 2 {
		return nil
	}
	if ctx.checks == 2 {
		close(ctx.done)
	}
	return context.Canceled
}

func (*cancelOnSecondCheckContext) Value(any) any {
	return nil
}

func TestValidateGenerationRejectsOverflow(t *testing.T) {
	tests := []corpusSpec{
		{Profile: "small-overflow", SmallFiles: maxInt64(), Groups: 1},
		{Profile: "archive-overflow", Archives: 1, ArchiveEntries: maxInt64()},
		{Profile: "negative", ArchiveEntries: -1},
	}
	for _, spec := range tests {
		t.Run(spec.Profile, func(t *testing.T) {
			if err := validateGeneration(spec, 1); err == nil {
				t.Fatalf("validateGeneration(%+v) succeeded", spec)
			}
		})
	}
	if _, ok := checkedAddUint64(^uint64(0), 1); ok {
		t.Fatal("overflowing addition succeeded")
	}
	if _, ok := checkedMulUint64(^uint64(0), 2); ok {
		t.Fatal("overflowing multiplication succeeded")
	}
}

func makeTreeWritable(root string) {
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr == nil && entry.IsDir() {
			_ = os.Chmod(path, 0o755)
		}
		return nil
	})
}

func TestGenerateCorpusRejectsExistingOutput(t *testing.T) {
	root := t.TempDir()
	err := generateCorpus(
		context.Background(),
		root,
		corpusSpec{Profile: "test", Shape: "wide", SmallFiles: 1, Groups: 1},
		1,
		1,
	)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %v, want existing-output error", err)
	}
}

func TestDefaultConfirmationThresholds(t *testing.T) {
	sequential, err := defaultSpec("large-sequential")
	if err != nil {
		t.Fatal(err)
	}
	if sequential.SequentialBytes != 256<<30 || sequential.SequentialFiles != 32 {
		t.Fatalf("sequential spec = %+v", sequential)
	}
	packages, err := defaultSpec("package-cache")
	if err != nil {
		t.Fatal(err)
	}
	if packages.SmallFiles < 2_000_000 {
		t.Fatalf("package files = %d, want at least two million", packages.SmallFiles)
	}
}

func treeDigests(t *testing.T, root string) map[string]string {
	t.Helper()
	result := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(data)
		result[filepath.ToSlash(relative)] = hex.EncodeToString(digest[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
