package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/betterleaks/betterleaks/contrib/filescan/internal/corpus"
	"github.com/betterleaks/betterleaks/contrib/filescan/internal/protocol"
)

func TestManifestCLIWorkflow(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "corpus")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(root, "input.txt")
	if err := os.WriteFile(input, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(workspace, "artifacts", "manifest.json")
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{
		"filescan-manifest", "create",
		"-root", root, "-out", manifestPath, "-id", "tiny-v1",
		"-class", "tiny", "-role", "correctness",
		"-origin", "test-fixture", "-revision", "v1",
	}, bytes.NewReader(nil), &stdout, &stderr); err != nil {
		t.Fatalf("create: %v; stderr=%s", err, stderr.String())
	}
	var created corpus.Manifest
	if err := json.Unmarshal(stdout.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID != "tiny-v1" {
		t.Fatalf("unexpected manifest output: %#v", created)
	}

	stdout.Reset()
	stderr.Reset()
	if err := run(context.Background(), []string{
		"filescan-manifest", "verify", "-manifest", manifestPath,
		"-root", root, "-mode", "all",
	}, bytes.NewReader(nil), &stdout, &stderr); err != nil {
		t.Fatalf("verify: %v; stderr=%s", err, stderr.String())
	}

	identity := protocol.OutputIdentity{
		Count: 1, CanonicalBytes: 8,
		Digest: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
	}
	metrics := protocol.ScannerMetrics{
		SchemaVersion: protocol.SchemaVersion, Candidate: "baseline",
		Targets: identity, Fragments: identity, Findings: identity,
		Errors: identity, Fallbacks: identity,
	}
	metricsJSON, err := json.Marshal(metrics)
	if err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if err := run(context.Background(), []string{
		"filescan-manifest", "oracle", "-manifest", manifestPath,
	}, bytes.NewReader(metricsJSON), &stdout, &stderr); err != nil {
		t.Fatalf("oracle: %v; stderr=%s", err, stderr.String())
	}
	loaded, err := corpus.Load(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Oracle == nil || loaded.ExpectedOutputs != metrics.Outputs() {
		t.Fatalf("oracle not persisted: %#v", loaded)
	}

	if err := os.WriteFile(input, []byte("mutated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	err = run(context.Background(), []string{
		"filescan-manifest", "verify", "-manifest", manifestPath,
		"-root", root, "-mode", "portable",
	}, bytes.NewReader(nil), &stdout, &stderr)
	if !errors.Is(err, errVerificationFailed) {
		t.Fatalf("mutated corpus returned %v, want verification failure", err)
	}
}
