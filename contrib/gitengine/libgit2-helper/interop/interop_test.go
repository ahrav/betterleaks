package interop_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/betterleaks/betterleaks/internal/gitengine"
	"github.com/betterleaks/betterleaks/sources"
)

func processFactory(t *testing.T, helper string) *gitengine.ProcessFactory {
	t.Helper()
	process, err := gitengine.NewProcessFactory(gitengine.ProcessConfig{
		Command: func(repoPath string, profile gitengine.ScanProfile) *exec.Cmd {
			args := []string{"--repo", repoPath}
			if profile.AllStatuses {
				args = append(args, "--all-statuses")
			}
			if profile.FindCopies {
				args = append(args, "--find-copies")
			}
			return exec.Command(helper, args...)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return process
}

func runGit(t *testing.T, repo string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test Author", "GIT_AUTHOR_EMAIL=author@example.test",
		"GIT_COMMITTER_NAME=Test Committer", "GIT_COMMITTER_EMAIL=committer@example.test",
		"GIT_AUTHOR_DATE=2001-02-03T04:05:06-0700",
		"GIT_COMMITTER_DATE=2001-02-03T04:05:06-0700")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

func commit(t *testing.T, repo, message string) gitengine.OID {
	t.Helper()
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "--allow-empty", "-q", "-m", message)
	hexOID := string(runGit(t, repo, "rev-parse", "HEAD"))
	oid, err := hex.DecodeString(hexOID[:len(hexOID)-1])
	if err != nil {
		t.Fatal(err)
	}
	return oid
}

func commitEmptyEmail(t *testing.T, repo string) gitengine.OID {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "commit", "--allow-empty", "-q", "-m", "empty email")
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Empty Email Author", "GIT_AUTHOR_EMAIL=",
		"GIT_COMMITTER_NAME=Empty Email Committer", "GIT_COMMITTER_EMAIL=",
		"GIT_AUTHOR_DATE=2001-02-03T04:05:06-0700",
		"GIT_COMMITTER_DATE=2001-02-03T04:05:06-0700")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("empty-email commit: %v\n%s", err, out)
	}
	hexOID := string(runGit(t, repo, "rev-parse", "HEAD"))
	oid, err := hex.DecodeString(hexOID[:len(hexOID)-1])
	if err != nil {
		t.Fatal(err)
	}
	return oid
}

func commitWithoutAuthor(t *testing.T, repo string) gitengine.OID {
	t.Helper()
	tree := bytes.TrimSpace(runGit(t, repo, "write-tree"))
	parent := bytes.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	raw := append([]byte("tree "), tree...)
	raw = append(raw, []byte("\nparent ")...)
	raw = append(raw, parent...)
	raw = append(raw, []byte("\ncommitter Test Committer <committer@example.test> 981173106 -0700\n\nmissing author\n")...)
	cmd := exec.Command("git", "-C", repo, "hash-object", "--literally", "-t", "commit", "-w", "--stdin")
	cmd.Stdin = bytes.NewReader(raw)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("write missing-author commit: %v\n%s", err, out)
	}
	oid, err := hex.DecodeString(string(bytes.TrimSpace(out)))
	if err != nil {
		t.Fatal(err)
	}
	return oid
}

func fixture(t *testing.T) (string, []gitengine.OID) {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, ".mailmap"), []byte("Mapped Author <mapped@example.test> Test Author <author@example.test>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oids := []gitengine.OID{commit(t, repo, "root title\n\nroot body")}
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oids = append(oids, commit(t, repo, "modify"))
	oids = append(oids, commit(t, repo, "empty commit"))
	if err := os.Chmod(filepath.Join(repo, "file.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	oids = append(oids, commit(t, repo, "mode"))
	if err := os.Rename(filepath.Join(repo, "file.txt"), filepath.Join(repo, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	oids = append(oids, commit(t, repo, "rename"))
	var renameSource []byte
	for i := range 10 {
		renameSource = append(renameSource, []byte(fmt.Sprintf("rename-shared-%02d-", i))...)
		renameSource = append(renameSource, bytes.Repeat([]byte{byte('A' + i)}, 45)...)
		renameSource = append(renameSource, '\n')
	}
	if err := os.WriteFile(filepath.Join(repo, "edited-source.txt"), renameSource, 0o644); err != nil {
		t.Fatal(err)
	}
	oids = append(oids, commit(t, repo, "edited rename source"))
	if err := os.Remove(filepath.Join(repo, "edited-source.txt")); err != nil {
		t.Fatal(err)
	}
	lines := bytes.SplitAfter(renameSource, []byte{'\n'})
	editedTarget := bytes.Join(lines[:6], nil)
	for i := range 4 {
		editedTarget = append(editedTarget, []byte(fmt.Sprintf("rename-new-%02d-", i))...)
		editedTarget = append(editedTarget, bytes.Repeat([]byte{'Z'}, 48)...)
		editedTarget = append(editedTarget, '\n')
	}
	if err := os.WriteFile(filepath.Join(repo, "edited-target.txt"), editedTarget, 0o644); err != nil {
		t.Fatal(err)
	}
	oids = append(oids, commit(t, repo, "edited rename target"))
	if err := os.WriteFile(filepath.Join(repo, "partial-source.txt"), []byte("a\ntail"), 0o644); err != nil {
		t.Fatal(err)
	}
	oids = append(oids, commit(t, repo, "partial similarity source"))
	if err := os.Remove(filepath.Join(repo, "partial-source.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "partial-target.txt"), []byte("a\ntail changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	oids = append(oids, commit(t, repo, "partial similarity target"))
	if err := os.WriteFile(filepath.Join(repo, "binary.bin"), []byte{'a', 0, 'b', '\n'}, 0o644); err != nil {
		t.Fatal(err)
	}
	oids = append(oids, commit(t, repo, "binary"))
	if err := os.Rename(filepath.Join(repo, "binary.bin"), filepath.Join(repo, "renamed-binary.bin")); err != nil {
		t.Fatal(err)
	}
	oids = append(oids, commit(t, repo, "binary rename"))
	if err := os.WriteFile(filepath.Join(repo, "no-final.txt"), []byte("last line"), 0o644); err != nil {
		t.Fatal(err)
	}
	oids = append(oids, commit(t, repo, "no final"))
	oids = append(oids, commitEmptyEmail(t, repo))
	oids = append(oids, commit(t, repo, "tab title\n\nConflicts:\n\tpath/to/file"))
	oids = append(oids, commit(t, repo, "issues counter updated \u0085\nbuttons are disabled"))
	return repo, oids
}

func collect(t *testing.T, factory gitengine.Factory, repo string, request gitengine.BatchRequest) []gitengine.Record {
	t.Helper()
	ctx := context.Background()
	if _, err := factory.Preflight(ctx, repo, gitengine.ProductionProfile()); err != nil {
		t.Fatal(err)
	}
	worker, err := factory.Open(ctx, repo, gitengine.ProductionProfile())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := worker.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	var records []gitengine.Record
	if _, err := worker.ScanBatch(ctx, request, func(record gitengine.Record) error {
		records = append(records, record)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return records
}

func TestMatchesStockGitReferenceOnCoreFixture(t *testing.T) {
	helper, err := filepath.Abs(filepath.Join("..", "betterleaks-libgit2-engine"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(helper); err != nil {
		t.Skip("build helper with make first")
	}
	repo, oids := fixture(t)
	request := gitengine.BatchRequest{ID: 1, Commits: oids}
	reference := collect(t, sources.NewGitReferenceFactory(), repo, request)
	process := processFactory(t, helper)
	candidate := collect(t, process, repo, request)
	if err := gitengine.CompareMultisets(reference, candidate); err != nil {
		t.Fatalf("libgit2 differs from stock Git: %v\nreference=%#v\ncandidate=%#v", err, reference, candidate)
	}
}

func TestMissingAuthorMatchesStockGitReference(t *testing.T) {
	helper, err := filepath.Abs(filepath.Join("..", "betterleaks-libgit2-engine"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(helper); err != nil {
		t.Skip("build helper with make first")
	}
	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	_ = commit(t, repo, "parent")
	oid := commitWithoutAuthor(t, repo)
	request := gitengine.BatchRequest{ID: 2, Commits: []gitengine.OID{oid}}
	reference := collect(t, sources.NewGitReferenceFactory(), repo, request)
	if len(reference) != 1 || reference[0].Kind != gitengine.RecordCommit {
		t.Fatalf("stock Git records = %#v, want one commit", reference)
	}
	metadata := reference[0].Commit
	if metadata.HasAuthor || metadata.HasAuthorTime || len(metadata.AuthorName) != 0 ||
		len(metadata.AuthorEmail) != 0 || metadata.AuthorUnixSeconds != 0 ||
		metadata.AuthorUTCOffsetMinutes != 0 {
		t.Fatalf("stock Git missing-author projection = %#v", metadata)
	}
	candidate := collect(t, processFactory(t, helper), repo, request)
	if err := gitengine.CompareMultisets(reference, candidate); err != nil {
		t.Fatalf("libgit2 missing-author projection differs from stock Git: %v\nreference=%#v\ncandidate=%#v", err, reference, candidate)
	}
}

func TestTextconvPreflightIsTypedUnsupported(t *testing.T) {
	helper, err := filepath.Abs(filepath.Join("..", "betterleaks-libgit2-engine"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(helper); err != nil {
		t.Skip("build helper with make first")
	}
	repo, _ := fixture(t)
	runGit(t, repo, "config", "diff.demo.textconv", "cat")
	_, err = processFactory(t, helper).Preflight(context.Background(), repo, gitengine.ProductionProfile())
	if !errors.Is(err, gitengine.ErrUnsupported) {
		t.Fatalf("preflight error = %v, want ErrUnsupported", err)
	}
}
