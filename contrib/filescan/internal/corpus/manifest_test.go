package corpus

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/betterleaks/betterleaks/contrib/filescan/internal/protocol"
)

const corpusTestDigest = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func TestManifestCreateVerifyRelocateAndMutate(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "source")
	createTinyTree(t, root)
	manifestPath := filepath.Join(workspace, "artifacts", "corpus.json")
	created, err := Create(context.Background(), CreateOptions{
		Root: root, Output: manifestPath, ID: "tiny-v1",
		WorkloadClass: "mixed-test", Role: "correctness",
		Source: Source{Origin: "test-fixture", Revision: "v1", Objects: []string{"alpha.txt"}},
	})
	if err != nil {
		t.Fatalf("create manifest: %v", err)
	}
	if created.Summary.RegularFiles != 2 || created.Summary.Directories != 2 || created.Summary.Symlinks != 1 {
		t.Fatalf("unexpected summary: %#v", created.Summary)
	}

	all, err := Verify(context.Background(), manifestPath, root, "all")
	if err != nil || !all.Valid {
		t.Fatalf("verify original: valid=%t err=%v ledger=%#v", all.Valid, err, all.Validity)
	}
	if all.ObservedPortable == "" || all.ObservedLiveGuard == "" || all.ObservedEntriesHash == "" {
		t.Fatalf("all verification omitted identities: %#v", all)
	}

	relocated := filepath.Join(workspace, "relocated")
	createTinyTree(t, relocated)
	portable, err := Verify(context.Background(), manifestPath, relocated, "portable")
	if err != nil || !portable.Valid {
		t.Fatalf("portable verification after relocation: valid=%t err=%v ledger=%#v", portable.Valid, err, portable.Validity)
	}
	live, err := Verify(context.Background(), manifestPath, relocated, "live")
	if err != nil {
		t.Fatalf("live verification after relocation: %v", err)
	}
	if live.Valid || len(live.Validity) != 1 || live.Validity[0].Code != "live_guard_mismatch" {
		t.Fatalf("relocated live guard was accepted: %#v", live)
	}

	if err := os.WriteFile(filepath.Join(relocated, "alpha.txt"), []byte("changed\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	portable, err = Verify(context.Background(), manifestPath, relocated, "portable")
	if err != nil {
		t.Fatalf("verify mutated corpus: %v", err)
	}
	if portable.Valid || portable.Validity[0].Code != "portable_content_mismatch" {
		t.Fatalf("content mutation was accepted: %#v", portable)
	}
}

func TestManifestSidecarOracleAndStrictLoad(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "source")
	createTinyTree(t, root)
	manifestPath := filepath.Join(workspace, "artifacts", "corpus.json")
	_, err := Create(context.Background(), CreateOptions{
		Root: root, Output: manifestPath, ID: "tiny-v1",
		WorkloadClass: "mixed-test", Role: "calibration",
		Source: Source{Origin: "test-fixture", Revision: "v1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	identity := protocol.OutputIdentity{Count: 2, CanonicalBytes: 10, Digest: corpusTestDigest}
	metrics := protocol.ScannerMetrics{
		SchemaVersion: protocol.SchemaVersion, Candidate: "trusted-baseline",
		Targets: identity, Fragments: identity, Findings: identity,
		Errors: identity, Fallbacks: identity,
	}
	updated, err := SetOracle(context.Background(), manifestPath, metrics)
	if err != nil {
		t.Fatalf("set oracle: %v", err)
	}
	if updated.Oracle == nil || updated.Oracle.Candidate != "trusted-baseline" || updated.ExpectedOutputs != metrics.Outputs() {
		t.Fatalf("oracle was not frozen: %#v", updated)
	}
	loaded, err := Load(manifestPath)
	if err != nil || loaded.ExpectedOutputs != metrics.Outputs() {
		t.Fatalf("load frozen oracle: manifest=%#v err=%v", loaded, err)
	}

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	unknown := bytes.Replace(data, []byte(`"schema_version": 1,`), []byte(`"schema_version": 1, "unexpected": true,`), 1)
	unknownPath := filepath.Join(filepath.Dir(manifestPath), "unknown.json")
	if err := os.WriteFile(unknownPath, unknown, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(unknownPath); err == nil {
		t.Fatal("manifest with unknown field was accepted")
	}

	sidecar := entriesPath(manifestPath, loaded.EntriesFile)
	file, err := os.OpenFile(sidecar, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("tamper"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	verification, err := Verify(context.Background(), manifestPath, root, "sidecar")
	if err != nil {
		t.Fatal(err)
	}
	if verification.Valid || verification.Validity[0].Code != "entry_sidecar_mismatch" {
		t.Fatalf("tampered sidecar was accepted: %#v", verification)
	}
}

func TestManifestCreationRejectsUnsafeOrIncompleteOptions(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "source")
	createTinyTree(t, root)
	rootAlias := filepath.Join(workspace, "source-alias")
	if err := os.Symlink(root, rootAlias); err != nil {
		t.Fatal(err)
	}
	valid := CreateOptions{
		Root: root, Output: filepath.Join(workspace, "corpus.json"),
		ID: "tiny-v1", WorkloadClass: "test", Role: "calibration",
		Source: Source{Origin: "fixture", Revision: "v1"},
	}
	tests := []struct {
		name   string
		mutate func(*CreateOptions)
		want   string
	}{
		{"empty-root", func(options *CreateOptions) { options.Root = "" }, "corpus root is required"},
		{"empty-output", func(options *CreateOptions) { options.Output = "" }, "manifest output is required"},
		{"inside-root", func(options *CreateOptions) { options.Output = filepath.Join(root, "manifest.json") }, "outside"},
		{"symlink-inside-root", func(options *CreateOptions) { options.Output = filepath.Join(rootAlias, "created", "manifest.json") }, "outside"},
		{"invalid-role", func(options *CreateOptions) { options.Role = "performance" }, "role"},
		{"missing-origin", func(options *CreateOptions) { options.Source.Origin = "" }, "origin and revision"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := valid
			test.mutate(&options)
			_, err := Create(context.Background(), options)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want error containing %q", err, test.want)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(root, "created")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe output validation mutated corpus through symlink: %v", err)
	}
}

func TestForceReplacementUsesContentAddressedSidecars(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "source")
	createTinyTree(t, root)
	manifestPath := filepath.Join(workspace, "artifacts", "corpus.json")
	options := CreateOptions{
		Root: root, Output: manifestPath, ID: "tiny-v1",
		WorkloadClass: "test", Role: "calibration",
		Source: Source{Origin: "fixture", Revision: "v1"},
	}
	first, err := Create(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	oldSidecar := entriesPath(manifestPath, first.EntriesFile)
	options.Force = true
	second, err := Create(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if second.EntriesFile != first.EntriesFile {
		t.Fatalf("unchanged corpus produced a new sidecar: %q != %q", second.EntriesFile, first.EntriesFile)
	}
	if err := os.WriteFile(filepath.Join(root, "alpha.txt"), []byte("new content\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	third, err := Create(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if third.EntriesFile == first.EntriesFile {
		t.Fatal("changed corpus reused its old content-addressed sidecar")
	}
	if _, err := os.Stat(oldSidecar); err != nil {
		t.Fatalf("old sidecar needed by prior manifest snapshots was removed: %v", err)
	}
}

func createTinyTree(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "alpha.txt"), []byte("alpha\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "archive.zip"), []byte("not really a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("alpha.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
}
