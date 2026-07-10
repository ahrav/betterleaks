//go:build gitengine_gix

package gixhelper_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/betterleaks/betterleaks/internal/gitengine"
	"github.com/betterleaks/betterleaks/sources"
)

// TestGixMatchesStockReference is an explicit interoperability gate. Run it
// after a release build with BETTERLEAKS_GIX_HELPER pointing at that binary.
func TestGixMatchesStockReference(t *testing.T) {
	helper := os.Getenv("BETTERLEAKS_GIX_HELPER")
	if helper == "" {
		t.Fatal("BETTERLEAKS_GIX_HELPER must point at a built gix helper")
	}
	repo := t.TempDir()
	git(t, repo, "init", "--quiet")
	git(t, repo, "config", "user.name", "Test User")
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "core.fileMode", "true")

	write(t, filepath.Join(repo, "file.txt"), []byte("one\n"), 0o644)
	write(t, filepath.Join(repo, "long-tail.txt"), append([]byte("old prefix\n"), bytes.Repeat([]byte("common tail\n"), 600)...), 0o644)
	root := commit(t, repo, "root")
	write(t, filepath.Join(repo, "file.txt"), []byte("one\ntwo"), 0o644)
	write(t, filepath.Join(repo, "long-tail.txt"), append([]byte("new prefix\n"), bytes.Repeat([]byte("common tail\n"), 600)...), 0o644)
	modify := commit(t, repo, "modify")
	git(t, repo, "mv", "file.txt", "renamed.txt")
	rename := commit(t, repo, "rename")
	git(t, repo, "mv", "renamed.txt", "edited.txt")
	write(t, filepath.Join(repo, "edited.txt"), []byte("one\ntwo\nthree\n"), 0o644)
	editedRename := commit(t, repo, "edited rename")
	write(t, filepath.Join(repo, ".gitattributes"), []byte("binary.dat -diff\ntext.dat diff\n"), 0o644)
	write(t, filepath.Join(repo, "binary.dat"), []byte{'a', 0, 'b', '\n'}, 0o644)
	write(t, filepath.Join(repo, "text.dat"), []byte{'x', 0, 'y', '\n'}, 0o644)
	binary := commit(t, repo, "binary")
	git(t, repo, "commit", "--quiet", "--allow-empty", "-m", "empty")
	empty := revParse(t, repo, "HEAD")
	write(t, filepath.Join(repo, ".mailmap"), []byte("Mapped <mapped@example.com> <TEST@EXAMPLE.COM>\n"), 0o644)

	commits := []gitengine.OID{decode(t, root), decode(t, modify), decode(t, rename), decode(t, editedRename), decode(t, binary), decode(t, empty)}
	request := gitengine.BatchRequest{ID: 41, Commits: commits}
	profile := gitengine.ProductionProfile()

	reference := sources.NewGitReferenceFactory()
	if _, err := reference.Preflight(t.Context(), repo, profile); err != nil {
		t.Fatal(err)
	}
	referenceWorker, err := reference.Open(t.Context(), repo, profile)
	if err != nil {
		t.Fatal(err)
	}
	defer referenceWorker.Close()

	processFactory, err := gitengine.NewProcessFactory(gitengine.ProcessConfig{
		Command: func(repoPath string, _ gitengine.ScanProfile) *exec.Cmd {
			return exec.Command(helper, repoPath)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := processFactory.Preflight(t.Context(), repo, profile); err != nil {
		t.Fatal(err)
	}
	gixWorker, err := processFactory.Open(t.Context(), repo, profile)
	if err != nil {
		t.Fatal(err)
	}
	defer gixWorker.Close()

	want := collect(t, t.Context(), referenceWorker, request)
	got := collect(t, t.Context(), gixWorker, request)
	if err := gitengine.CompareMultisets(got, want); err != nil {
		t.Fatalf("gix differs from canonical Git: %v\ngot=%#v\nwant=%#v", err, got, want)
	}
	if len(got) == 0 {
		t.Fatal("interoperability fixture emitted no records")
	}
}

func TestGixMissingAuthorMatchesStockReference(t *testing.T) {
	helper := os.Getenv("BETTERLEAKS_GIX_HELPER")
	if helper == "" {
		t.Fatal("BETTERLEAKS_GIX_HELPER must point at a built gix helper")
	}
	repo := t.TempDir()
	git(t, repo, "init", "--quiet")
	git(t, repo, "config", "user.name", "Test User")
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "commit", "--quiet", "--allow-empty", "-m", "parent")
	oid := missingAuthorCommit(t, repo)
	request := gitengine.BatchRequest{ID: 42, Commits: []gitengine.OID{oid}}
	profile := gitengine.ProductionProfile()

	referenceFactory := sources.NewGitReferenceFactory()
	if _, err := referenceFactory.Preflight(t.Context(), repo, profile); err != nil {
		t.Fatal(err)
	}
	referenceWorker, err := referenceFactory.Open(t.Context(), repo, profile)
	if err != nil {
		t.Fatal(err)
	}
	defer referenceWorker.Close()

	processFactory, err := gitengine.NewProcessFactory(gitengine.ProcessConfig{
		Command: func(repoPath string, _ gitengine.ScanProfile) *exec.Cmd {
			return exec.Command(helper, repoPath)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := processFactory.Preflight(t.Context(), repo, profile); err != nil {
		t.Fatal(err)
	}
	gixWorker, err := processFactory.Open(t.Context(), repo, profile)
	if err != nil {
		t.Fatal(err)
	}
	defer gixWorker.Close()

	want := collect(t, t.Context(), referenceWorker, request)
	if len(want) != 1 || want[0].Kind != gitengine.RecordCommit {
		t.Fatalf("stock Git records = %#v, want one commit", want)
	}
	metadata := want[0].Commit
	if metadata.HasAuthor || metadata.HasAuthorTime || len(metadata.AuthorName) != 0 ||
		len(metadata.AuthorEmail) != 0 || metadata.AuthorUnixSeconds != 0 ||
		metadata.AuthorUTCOffsetMinutes != 0 {
		t.Fatalf("stock Git missing-author projection = %#v", metadata)
	}
	got := collect(t, t.Context(), gixWorker, request)
	if err := gitengine.CompareMultisets(got, want); err != nil {
		t.Fatalf("gix missing-author projection differs from stock Git: %v\ngot=%#v\nwant=%#v", err, got, want)
	}
}

func collect(t *testing.T, ctx context.Context, worker gitengine.Worker, request gitengine.BatchRequest) []gitengine.Record {
	t.Helper()
	var records []gitengine.Record
	result, err := worker.ScanBatch(ctx, request, func(record gitengine.Record) error {
		records = append(records, record)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Commits != uint64(len(request.Commits)) {
		t.Fatalf("commit count = %d, want %d", result.Commits, len(request.Commits))
	}
	return records
}

func commit(t *testing.T, repo, message string) string {
	t.Helper()
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "--quiet", "-m", message)
	return revParse(t, repo, "HEAD")
}

func missingAuthorCommit(t *testing.T, repo string) gitengine.OID {
	t.Helper()
	tree := bytes.TrimSpace(git(t, repo, "write-tree"))
	parent := bytes.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	raw := append([]byte("tree "), tree...)
	raw = append(raw, []byte("\nparent ")...)
	raw = append(raw, parent...)
	raw = append(raw, []byte("\ncommitter Test Committer <committer@example.test> 981173106 -0700\n\nmissing author\n")...)
	cmd := exec.Command("git", "-C", repo, "hash-object", "--literally", "-t", "commit", "-w", "--stdin")
	cmd.Stdin = bytes.NewReader(raw)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("write missing-author commit: %v: %s", err, out)
	}
	oid, err := hex.DecodeString(string(bytes.TrimSpace(out)))
	if err != nil {
		t.Fatal(err)
	}
	return oid
}

func revParse(t *testing.T, repo, revision string) string {
	t.Helper()
	out := git(t, repo, "rev-parse", revision)
	return string(bytes.TrimSpace(out))
}

func git(t *testing.T, repo string, args ...string) []byte {
	t.Helper()
	commandArgs := append([]string{"-C", repo}, args...)
	cmd := exec.Command("git", commandArgs...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_DATE=2026-01-02T03:04:05+0000",
		"GIT_COMMITTER_DATE=2026-01-02T03:04:05+0000",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return out
}

func write(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

func decode(t *testing.T, value string) gitengine.OID {
	t.Helper()
	oid, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return oid
}
