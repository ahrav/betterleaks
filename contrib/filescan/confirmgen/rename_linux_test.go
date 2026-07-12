//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRenameNoReplacePreservesExistingDirectory(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "temporary")
	newPath := filepath.Join(root, "existing")
	if err := os.Mkdir(oldPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(newPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplace(oldPath, newPath); !errors.Is(err, os.ErrExist) {
		t.Fatalf("error = %v, want exist", err)
	}
	for _, path := range []string{oldPath, newPath} {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Fatalf("stat %s = %+v, %v", path, info, err)
		}
	}
}

func TestCommitCorpusReportsCommittedSyncFailure(t *testing.T) {
	root := t.TempDir()
	temporary := filepath.Join(root, "temporary")
	output := filepath.Join(root, "output")
	if err := os.Mkdir(temporary, 0o700); err != nil {
		t.Fatal(err)
	}
	committed, err := commitCorpus(temporary, output, filepath.Join(root, "missing-parent"))
	if !committed || !errors.Is(err, errOutputCommitted) {
		t.Fatalf("committed = %t, error = %v", committed, err)
	}
	if info, statErr := os.Stat(output); statErr != nil || !info.IsDir() {
		t.Fatalf("output stat = %+v, %v", info, statErr)
	}
	if _, statErr := os.Stat(temporary); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("temporary stat error = %v, want not-exist", statErr)
	}
}

func TestRemoveTreeHandlesSealedDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sealed")
	child := filepath.Join(root, "child")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "file"), []byte("content"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(child, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := removeTree(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root stat error = %v, want not-exist", err)
	}
}
