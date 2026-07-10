package sources

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/betterleaks/betterleaks/internal/gitengine"
)

func TestReferenceGitEnvironmentMatchesSharedIsolationUnderHostileOverrides(t *testing.T) {
	t.Setenv("GIT_DIFF_OPTS", "-U99")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'diff.algorithm=patience'")
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "core.deltaBaseCacheLimit")
	t.Setenv("GIT_CONFIG_VALUE_0", "1g")
	t.Setenv("GIT_CONFIG_KEY_1", "diff.algorithm")
	t.Setenv("GIT_CONFIG_VALUE_1", "patience")

	got := envByKey(referenceGitEnv())
	want := envByKey(GitConfigIsolationEnv())
	if !reflect.DeepEqual(got, want) {
		t.Fatal("reference Git environment differs from shared isolation environment")
	}
	for _, key := range []string{
		"GIT_DIFF_OPTS", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1",
	} {
		if _, inherited := got[key]; inherited {
			t.Fatalf("reference Git environment inherited %s", key)
		}
	}
	for key, value := range map[string]string{
		"GIT_CONFIG_COUNT":   "1",
		"GIT_CONFIG_KEY_0":   "core.deltaBaseCacheLimit",
		"GIT_CONFIG_VALUE_0": "128m",
	} {
		if got[key] != value {
			t.Errorf("%s = %q, want %q", key, got[key], value)
		}
	}
}

func envByKey(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, found := strings.Cut(entry, "=")
		if found {
			out[key] = value
		}
	}
	return out
}

func TestParseRawDiffTreePreservesWeirdPaths(t *testing.T) {
	oldPath := []byte("old space\tline\n\xff")
	newPath := []byte("new quote\"slash\\\x80")
	meta := []byte(":100644 100755 " + strings.Repeat("1", 40) + " " + strings.Repeat("2", 40) + " R100")
	wire := append(append(append(append(meta, 0), oldPath...), 0), newPath...)
	wire = append(wire, 0)
	records, err := parseRawDiffTree(wire, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d", len(records))
	}
	if !bytes.Equal(records[0].OldPath, oldPath) || !bytes.Equal(records[0].NewPath, newPath) {
		t.Fatalf("paths changed: old=%q new=%q", records[0].OldPath, records[0].NewPath)
	}
	clear(wire)
	if !bytes.Equal(records[0].OldPath, oldPath) || !bytes.Equal(records[0].NewPath, newPath) {
		t.Fatal("raw records alias input buffer")
	}
}

func TestGitReferenceLiveFixtures(t *testing.T) {
	t.Setenv("GIT_DIFF_OPTS", "-U99")
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			repo, commits := createReferenceFixture(t, format)
			factory := NewGitReferenceFactory()
			capabilities, err := factory.Preflight(t.Context(), repo, gitengine.ProductionProfile())
			if err != nil {
				t.Fatal(err)
			}
			wantOIDLength := uint16(20)
			if format == "sha256" {
				wantOIDLength = 32
			}
			if capabilities.OIDLength != wantOIDLength {
				t.Fatalf("OIDLength = %d", capabilities.OIDLength)
			}
			worker, err := factory.Open(t.Context(), repo, gitengine.ProductionProfile())
			if err != nil {
				t.Fatal(err)
			}
			defer worker.Close()

			request := gitengine.BatchRequest{ID: 1}
			for _, commit := range commits {
				oid, err := hex.DecodeString(commit.oid)
				if err != nil {
					t.Fatal(err)
				}
				request.Commits = append(request.Commits, oid)
			}
			var records []gitengine.Record
			result, err := worker.ScanBatch(t.Context(), request, func(record gitengine.Record) error {
				records = append(records, record)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Commits != uint64(len(commits)) {
				t.Fatalf("commit count = %d", result.Commits)
			}
			assertReferenceFixtureRecords(t, commits, records)
		})
	}
}

func TestGitReferenceBatchUsesConstantSubprocesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess-counting wrapper requires a POSIX shell")
	}
	repo, commits := createReferenceFixture(t, "sha1")
	for i := range 64 {
		runFixtureGit(t, repo, "commit", "--allow-empty", "-m", fmt.Sprintf("bulk-%d", i))
		commits = append(commits, fixtureCommit{
			name: fmt.Sprintf("bulk-%d", i),
			oid:  strings.TrimSpace(runFixtureGit(t, repo, "rev-parse", "HEAD")),
		})
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "git-counting-wrapper")
	script := "#!/bin/sh\nprintf 'x\\n' >> \"$BETTERLEAKS_GIT_COUNT_FILE\"\nexec \"$BETTERLEAKS_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	countFile := filepath.Join(t.TempDir(), "count")
	t.Setenv("BETTERLEAKS_REAL_GIT", realGit)
	t.Setenv("BETTERLEAKS_GIT_COUNT_FILE", countFile)
	t.Setenv("BETTERLEAKS_GIT_BIN", wrapper)

	factory := NewGitReferenceFactory()
	if _, err := factory.Preflight(t.Context(), repo, gitengine.ProductionProfile()); err != nil {
		t.Fatal(err)
	}
	worker, err := factory.Open(t.Context(), repo, gitengine.ProductionProfile())
	if err != nil {
		t.Fatal(err)
	}
	request := gitengine.BatchRequest{ID: 1}
	for _, commit := range commits {
		oid, err := hex.DecodeString(commit.oid)
		if err != nil {
			t.Fatal(err)
		}
		request.Commits = append(request.Commits, oid)
	}
	before := gitInvocationCount(t, countFile)
	result, err := worker.ScanBatch(t.Context(), request, func(gitengine.Record) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	after := gitInvocationCount(t, countFile)
	if got := after - before; got != 3 {
		t.Fatalf("multi-commit ScanBatch started %d Git subprocesses, want 3", got)
	}
	if result.Commits != uint64(len(commits)) {
		t.Fatalf("commits = %d, want %d", result.Commits, len(commits))
	}

	before = gitInvocationCount(t, countFile)
	empty, err := worker.ScanBatch(t.Context(), gitengine.BatchRequest{ID: 2}, func(gitengine.Record) error { return nil })
	if err != nil || empty.Commits != 0 || gitInvocationCount(t, countFile) != before {
		t.Fatalf("empty batch result=%#v error=%v subprocesses=%d", empty, err, gitInvocationCount(t, countFile)-before)
	}
	oid := request.Commits[0]
	before = gitInvocationCount(t, countFile)
	_, err = worker.ScanBatch(t.Context(), gitengine.BatchRequest{ID: 3, Commits: []gitengine.OID{oid, bytes.Clone(oid)}}, func(gitengine.Record) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "duplicate") || gitInvocationCount(t, countFile) != before {
		t.Fatalf("duplicate batch error=%v subprocesses=%d", err, gitInvocationCount(t, countFile)-before)
	}
}

func gitInvocationCount(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(data, []byte{'\n'})
}

type fixtureCommit struct {
	name string
	oid  string
}

func createReferenceFixture(t *testing.T, format string) (string, []fixtureCommit) {
	t.Helper()
	repo := t.TempDir()
	cmd := exec.Command(gitBinary(), "init", "--object-format="+format, repo)
	cmd.Env = gitConfigIsolationEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		if format == "sha256" {
			t.Skipf("Git does not support SHA-256 fixtures: %v: %s", err, out)
		}
		t.Fatalf("git init: %v: %s", err, out)
	}
	runFixtureGit(t, repo, "config", "user.name", "Fixture User")
	runFixtureGit(t, repo, "config", "user.email", "fixture@example.test")
	var commits []fixtureCommit
	commit := func(name string, allowEmpty bool) {
		t.Helper()
		args := []string{"commit", "-m", name}
		if allowEmpty {
			args = append(args, "--allow-empty")
		}
		runFixtureGit(t, repo, args...)
		commits = append(commits, fixtureCommit{name: name, oid: strings.TrimSpace(runFixtureGit(t, repo, "rev-parse", "HEAD"))})
	}

	writeFixtureFile(t, repo, "root.txt", []byte("root\n"), 0o644)
	runFixtureGit(t, repo, "add", "--all")
	commit("root", false)
	commit("empty", true)
	if err := os.Chmod(filepath.Join(repo, "root.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	runFixtureGit(t, repo, "add", "--all")
	commit("mode", false)
	runFixtureGit(t, repo, "mv", "root.txt", "renamed.txt")
	commit("rename", false)
	writeFixtureFile(t, repo, "renamed.txt", []byte("without final newline"), 0o755)
	runFixtureGit(t, repo, "add", "--all")
	commit("no-final-newline", false)
	writeFixtureFile(t, repo, "binary.dat", []byte{0, 1, 2, 3}, 0o644)
	runFixtureGit(t, repo, "add", "--all")
	commit("binary", false)
	if err := os.Remove(filepath.Join(repo, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	runFixtureGit(t, repo, "add", "--all")
	commit("delete", false)
	writeFixtureFile(t, repo, "type-change", []byte("regular\n"), 0o644)
	runFixtureGit(t, repo, "add", "--all")
	commit("type-base", false)
	if runtime.GOOS == "windows" {
		t.Skip("symlink type-change fixture requires Unix")
	}
	if err := os.Remove(filepath.Join(repo, "type-change")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(repo, "type-change")); err != nil {
		t.Fatal(err)
	}
	runFixtureGit(t, repo, "add", "--all")
	commit("type-change", false)

	weirdName := "weird space\tline\n雪"
	writeFixtureFile(t, repo, weirdName, []byte("weird\n"), 0o644)
	runFixtureGit(t, repo, "add", "--all")
	commit("weird-path", false)
	return repo, commits
}

func writeFixtureFile(t *testing.T, repo, name string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), data, mode); err != nil {
		t.Fatal(err)
	}
}

func runFixtureGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmdArgs := append([]string{"-C", repo}, args...)
	cmd := exec.CommandContext(context.Background(), gitBinary(), cmdArgs...)
	cmd.Env = append(gitConfigIsolationEnv(),
		"GIT_AUTHOR_DATE=2020-01-02T03:04:05-0700",
		"GIT_COMMITTER_DATE=2020-01-02T03:04:05-0700")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func assertReferenceFixtureRecords(t *testing.T, commits []fixtureCommit, records []gitengine.Record) {
	t.Helper()
	commitName := make(map[string]string, len(commits))
	for _, commit := range commits {
		commitName[commit.oid] = commit.name
	}
	files := make(map[string][]gitengine.FileRecord)
	hunks := make(map[string][]gitengine.HunkRecord)
	var seenCommits []string
	for _, record := range records {
		switch record.Kind {
		case gitengine.RecordCommit:
			hexOID := hex.EncodeToString(record.Commit.OID)
			seenCommits = append(seenCommits, commitName[hexOID])
		case gitengine.RecordFile:
			files[commitName[hex.EncodeToString(record.File.Commit)]] = append(files[commitName[hex.EncodeToString(record.File.Commit)]], record.File)
		case gitengine.RecordHunk:
			hunks[commitName[hex.EncodeToString(record.Hunk.Commit)]] = append(hunks[commitName[hex.EncodeToString(record.Hunk.Commit)]], record.Hunk)
		}
	}
	sort.Strings(seenCommits)
	if len(seenCommits) != len(commits) {
		t.Fatalf("seen commits = %v", seenCommits)
	}
	if len(files["empty"]) != 0 {
		t.Fatal("empty commit emitted a file")
	}
	if len(files["delete"]) != 0 {
		t.Fatal("delete escaped production filter")
	}
	if len(files["type-change"]) != 0 {
		t.Fatal("type-change escaped production filter")
	}
	if got := files["root"]; len(got) != 1 || got[0].Status != gitengine.StatusAdded {
		t.Fatalf("root files = %#v", got)
	}
	if got := files["mode"]; len(got) != 1 || got[0].Status != gitengine.StatusModified || len(hunks["mode"]) != 0 {
		t.Fatalf("mode records = %#v hunks=%#v", got, hunks["mode"])
	}
	if got := files["rename"]; len(got) != 1 || got[0].Status != gitengine.StatusRenamed || string(got[0].NewPath) != "renamed.txt" || len(hunks["rename"]) != 0 {
		t.Fatalf("rename records = %#v", got)
	}
	if got := hunks["no-final-newline"]; len(got) == 0 || !got[len(got)-1].MissingFinalNewline {
		t.Fatalf("no-final-newline hunks = %#v", got)
	}
	if got := files["binary"]; len(got) != 1 || !got[0].Binary || len(hunks["binary"]) != 0 {
		t.Fatalf("binary records = %#v hunks=%#v", got, hunks["binary"])
	}
	if got := files["weird-path"]; len(got) != 1 || !bytes.Contains(got[0].NewPath, []byte("\n雪")) {
		t.Fatalf("weird path = %#v", got)
	}
}

func ExampleNewGitReferenceFactory() {
	factory := NewGitReferenceFactory()
	fmt.Printf("%T\n", factory)
	// Output: *sources.GitReferenceFactory
}
