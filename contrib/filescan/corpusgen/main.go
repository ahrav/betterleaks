// Command corpusgen creates the deterministic sparse/unusual correctness
// corpus used by the filesystem scanner tournament.
package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const sparseSize = 64 << 20

func main() {
	out := flag.String("out", "", "new corpus directory")
	flag.Parse()
	if flag.NArg() != 0 || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: filescan-corpusgen -out NEW_DIRECTORY")
		os.Exit(2)
	}
	if err := generateCorpus(*out); err != nil {
		fmt.Fprintln(os.Stderr, "filescan-corpusgen:", err)
		os.Exit(1)
	}
}

func generateCorpus(root string) (err error) {
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("output already exists: %s", root)
		}
		return err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(root)
		}
	}()

	write := func(relative string, data []byte, mode os.FileMode) error {
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return err
		}
		return os.Chmod(path, mode)
	}
	if err := write("plain/missing-final-newline.txt", []byte("alpha\nbeta\ngamma"), 0o644); err != nil {
		return err
	}
	if err := write("plain/unicode-雪-U0001f680.txt", []byte("雪 and rocket\n"), 0o644); err != nil {
		return err
	}
	longLine := bytes.Repeat([]byte("0123456789abcdef"), 20_000)
	longLine = append(longLine, '\n')
	longLine = append(longLine, []byte("tail-without-newline")...)
	if err := write("plain/long-line.txt", longLine, 0o644); err != nil {
		return err
	}
	if err := write("permissions/unreadable.txt", []byte("permission gate\n"), 0o000); err != nil {
		return err
	}

	sparsePath := filepath.Join(root, "sparse", "image.log")
	if err := os.MkdirAll(filepath.Dir(sparsePath), 0o755); err != nil {
		return err
	}
	sparse, err := os.OpenFile(sparsePath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	if err := sparse.Truncate(sparseSize); err != nil {
		_ = sparse.Close()
		return err
	}
	if _, err := sparse.WriteAt([]byte("sparse-start\n"), 0); err != nil {
		_ = sparse.Close()
		return err
	}
	if _, err := sparse.WriteAt([]byte("sparse-end\n"), sparseSize-11); err != nil {
		_ = sparse.Close()
		return err
	}
	if err := sparse.Close(); err != nil {
		return err
	}
	if err := os.Chmod(sparsePath, 0o644); err != nil {
		return err
	}

	inner, err := zipBytes(map[string][]byte{
		"nested/missing-final.txt": []byte("inside nested archive"),
		"nested/unicode-雪.txt":     []byte("雪\n"),
	})
	if err != nil {
		return err
	}
	outer, err := zipBytes(map[string][]byte{
		"outer/long.txt":  bytes.Repeat([]byte("archive line\n"), 20_000),
		"outer/inner.zip": inner,
	})
	if err != nil {
		return err
	}
	if err := write("archives/nested.zip", outer, 0o644); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Join(root, "links"), 0o755); err != nil {
		return err
	}
	if err := os.Symlink(
		filepath.Join("..", "plain", "missing-final-newline.txt"),
		filepath.Join(root, "links", "relative-link"),
	); err != nil {
		return err
	}
	if err := os.Symlink("missing-target", filepath.Join(root, "links", "broken-link")); err != nil {
		return err
	}
	if err := write("README.txt", []byte(strings.TrimSpace(`
Deterministic correctness corpus: sparse allocation, a long logical line,
missing final newlines, Unicode paths, nested ZIPs, symlinks, and a mode-000
file. Truncation races and helper crashes are injected by tests, not frozen.
`)+"\n"), 0o644); err != nil {
		return err
	}
	complete = true
	return nil
}

func zipBytes(files map[string][]byte) ([]byte, error) {
	var output bytes.Buffer
	archive := zip.NewWriter(&output)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		entry, err := archive.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(entry, bytes.NewReader(files[name])); err != nil {
			return nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
