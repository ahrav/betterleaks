package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateCorpus(t *testing.T) {
	root := filepath.Join(t.TempDir(), "corpus")
	if err := generateCorpus(root); err != nil {
		t.Fatal(err)
	}
	if err := generateCorpus(root); err == nil {
		t.Fatal("generator overwrote an existing corpus")
	}
	info, err := os.Stat(filepath.Join(root, "sparse", "image.log"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != sparseSize {
		t.Fatalf("sparse size = %d, want %d", info.Size(), sparseSize)
	}
	archive, err := zip.OpenReader(filepath.Join(root, "archives", "nested.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if len(archive.File) != 2 {
		t.Fatalf("outer archive entries = %d, want 2", len(archive.File))
	}
	target, err := os.Readlink(filepath.Join(root, "links", "relative-link"))
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join("..", "plain", "missing-final-newline.txt") {
		t.Fatalf("symlink target = %q", target)
	}
}
