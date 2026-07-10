package main

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/betterleaks/betterleaks/internal/gitengine"
)

func TestIsolatedGitCommandRejectsHostileAmbientDiffConfig(t *testing.T) {
	t.Setenv("GIT_DIFF_OPTS", "-U99")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'diff.algorithm=patience'")
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "core.deltaBaseCacheLimit")
	t.Setenv("GIT_CONFIG_VALUE_0", "1g")
	t.Setenv("GIT_CONFIG_KEY_1", "diff.algorithm")
	t.Setenv("GIT_CONFIG_VALUE_1", "patience")

	cmd := isolatedGitCommand("git", "-C", "repo", "rev-list", "--all")
	env := make(map[string]string, len(cmd.Env))
	for _, item := range cmd.Env {
		key, value, found := strings.Cut(item, "=")
		if found {
			env[key] = value
		}
	}
	for _, key := range []string{"GIT_DIFF_OPTS", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1"} {
		if _, inherited := env[key]; inherited {
			t.Fatalf("isolated command inherited %s", key)
		}
	}
	for key, want := range map[string]string{
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_COUNT":    "1",
		"GIT_CONFIG_KEY_0":    "core.deltaBaseCacheLimit",
		"GIT_CONFIG_VALUE_0":  "128m",
	} {
		if env[key] != want {
			t.Errorf("%s = %q, want %q", key, env[key], want)
		}
	}
}

func TestGixHelperCommandSeparatesPureAndGitFallbackModes(t *testing.T) {
	t.Setenv("BETTERLEAKS_GIX_AMBIGUOUS_RENAME_FALLBACK", "1")
	for _, tc := range []struct {
		name     string
		fallback bool
		want     []string
	}{
		{name: "pure gix", want: []string{"gix-helper", "--repo", "repository"}},
		{
			name:     "Git rename fallback",
			fallback: true,
			want: []string{
				"gix-helper", "--repo", "repository", "--ambiguous-rename-fallback",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := gixHelperCommand("gix-helper", "repository", tc.fallback)
			if !reflect.DeepEqual(cmd.Args, tc.want) {
				t.Fatalf("argv = %#v, want %#v", cmd.Args, tc.want)
			}
			for _, entry := range cmd.Env {
				key, _, _ := strings.Cut(entry, "=")
				if key == "BETTERLEAKS_GIX_AMBIGUOUS_RENAME_FALLBACK" {
					t.Fatal("gix helper inherited legacy ambient fallback activation")
				}
			}
		})
	}
}

func TestCommitSummaryIsOrderIndependentAndRetainsMultiplicity(t *testing.T) {
	a := gitengine.Record{Kind: gitengine.RecordCommit, Commit: gitengine.CommitRecord{OID: bytes.Repeat([]byte{1}, 20)}}
	b := gitengine.Record{Kind: gitengine.RecordFile, File: gitengine.FileRecord{
		Commit: bytes.Repeat([]byte{1}, 20), Status: gitengine.StatusAdded,
		NewOID: bytes.Repeat([]byte{2}, 20), NewPath: []byte("path"),
	}}
	var left, right, duplicate commitSummary
	for _, record := range []gitengine.Record{a, b} {
		if err := left.add(record); err != nil {
			t.Fatal(err)
		}
	}
	for _, record := range []gitengine.Record{b, a} {
		if err := right.add(record); err != nil {
			t.Fatal(err)
		}
	}
	for _, record := range []gitengine.Record{a, b, b} {
		if err := duplicate.add(record); err != nil {
			t.Fatal(err)
		}
	}
	if left != right {
		t.Fatal("summary depends on record order")
	}
	if left == duplicate {
		t.Fatal("summary hid duplicate multiplicity")
	}
}

func TestBuildBatchesCoversEveryCommitOnce(t *testing.T) {
	commits := make([]gitengine.OID, 1041)
	for i := range commits {
		commits[i] = gitengine.OID{byte(i >> 8), byte(i)}
	}
	seen := make(map[string]bool, len(commits))
	for _, batch := range buildBatches(commits, 4, 256) {
		if len(batch) > 256 {
			t.Fatalf("batch length = %d, want at most 256", len(batch))
		}
		for _, oid := range batch {
			if seen[string(oid)] {
				t.Fatalf("duplicate %x", oid)
			}
			seen[string(oid)] = true
		}
	}
	if len(seen) != len(commits) {
		t.Fatalf("coverage = %d, want %d", len(seen), len(commits))
	}
}

func TestRecordOIDSelectsActiveVariant(t *testing.T) {
	oid := gitengine.OID(bytes.Repeat([]byte{3}, 20))
	records := []gitengine.Record{
		{Kind: gitengine.RecordCommit, Commit: gitengine.CommitRecord{OID: oid}},
		{Kind: gitengine.RecordFile, File: gitengine.FileRecord{Commit: oid, Status: gitengine.StatusModified}},
		{Kind: gitengine.RecordHunk, Hunk: gitengine.HunkRecord{Commit: oid}},
	}
	for _, record := range records {
		got, err := recordOID(record)
		if err != nil || !bytes.Equal(got, oid) {
			t.Fatalf("recordOID kind %d = %x, %v", record.Kind, got, err)
		}
	}
}
