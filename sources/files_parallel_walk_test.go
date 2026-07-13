package sources

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// collectParallel gathers the targets scanTargetsParallel emits (emission
// order is nondeterministic). Run with -race: the collector's mutex is the
// only synchronization the emit callback is promised.
func collectParallel(t *testing.T, s *Files) []ScanTarget {
	t.Helper()
	var (
		mu  sync.Mutex
		out []ScanTarget
	)
	err := s.scanTargetsParallel(context.Background(), func(st ScanTarget) {
		mu.Lock()
		out = append(out, st)
		mu.Unlock()
	})
	require.NoError(t, err)
	return out
}

// collectSerial gathers targets from the serial WalkDir-based scanTargets —
// the reference implementation the parallel walk must match.
func collectSerial(t *testing.T, s *Files) []ScanTarget {
	t.Helper()
	var out []ScanTarget
	err := s.scanTargets(context.Background(), func(st ScanTarget, err error) error {
		require.NoError(t, err)
		out = append(out, st)
		return nil
	})
	require.NoError(t, err)
	return out
}

func sortTargets(ts []ScanTarget) []string {
	keys := make([]string, len(ts))
	for i, st := range ts {
		keys[i] = st.Path + "\x00" + st.Symlink
	}
	sort.Strings(keys)
	return keys
}

// buildWalkTree creates a directory tree exercising every rule the walkers
// share: nested dirs (depth 4), regular files, empty files, an over-size
// file, an empty dir, a symlink to a file, a symlink to a dir, a deep chain,
// and (when not root) an unreadable subdir.
func buildWalkTree(t *testing.T) (root string, unreadable string) {
	t.Helper()
	root = t.TempDir()

	mk := func(rel string, size int) {
		t.Helper()
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, make([]byte, size), 0o644))
	}

	mk("top.txt", 10)
	mk("a/one.txt", 20)
	mk("a/b/two.txt", 30)
	mk("a/b/c/three.txt", 40)
	mk("a/b/c/d/four.txt", 50)
	mk("empty.txt", 0)  // skipped: empty
	mk("big.bin", 4096) // skipped when MaxFileSize < 4096
	require.NoError(t, os.MkdirAll(filepath.Join(root, "emptydir"), 0o755))

	// Deep chain (depth 12).
	deep := root
	for i := 0; i < 12; i++ {
		deep = filepath.Join(deep, fmt.Sprintf("deep%d", i))
	}
	require.NoError(t, os.MkdirAll(deep, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(deep, "leaf.txt"), []byte("leaf"), 0o644))

	// Symlinks: one to a file, one to a directory. Windows requires
	// elevation for symlink creation; the equivalence property is
	// platform-independent, so skip the symlink shapes there rather than
	// the whole test.
	if runtime.GOOS != "windows" {
		require.NoError(t, os.Symlink(filepath.Join(root, "top.txt"), filepath.Join(root, "link-to-file")))
		require.NoError(t, os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "link-to-dir")))
	}

	// Unreadable subdir (restored in cleanup). Meaningless as root.
	unreadable = filepath.Join(root, "locked")
	require.NoError(t, os.MkdirAll(unreadable, 0o755))
	mk("locked/hidden.txt", 10)
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		require.NoError(t, os.Chmod(unreadable, 0o000))
		t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })
	}

	return root, unreadable
}

// TestScanTargetsParallelEquivalence asserts the parallel walker emits
// exactly the target set of the serial filepath.WalkDir implementation, over
// every skip-rule combination. Run under -race (go test -race ./sources/):
// the walker pool is hand-rolled condvar concurrency where premature exit or
// deadlock under racing pushes is the classic failure mode.
func TestScanTargetsParallelEquivalence(t *testing.T) {
	root, _ := buildWalkTree(t)

	cases := map[string]*Files{
		"defaults":          {Path: root},
		"follow-symlinks":   {Path: root, FollowSymlinks: true},
		"max-file-size":     {Path: root, MaxFileSize: 1024},
		"symlinks-and-size": {Path: root, FollowSymlinks: true, MaxFileSize: 1024},
		"skip-subtree": {Path: root, ShouldSkip: func(attrs map[string]string) bool {
			p := attrs[AttrPath]
			return p == filepath.Join(root, "a", "b") || filepath.Base(p) == "one.txt"
		}},
	}

	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			want := sortTargets(collectSerial(t, s))
			got := sortTargets(collectParallel(t, s))
			assert.Equal(t, want, got, "parallel walk target set diverges from filepath.WalkDir reference")
		})
	}
}

// TestScanTargetsDeferredStat pins the deferred-metadata contract: on the
// default path (no filescan experiment, no max-size gate) the walkers skip
// the per-entry lstat, so empty files are emitted (they are filtered at
// read time by yielding zero fragments) and Size stays zero. Any size
// consumer (MaxFileSize here; FileScan metrics elsewhere) restores the
// stat and its empty/too-large gates.
func TestScanTargetsDeferredStat(t *testing.T) {
	root, _ := buildWalkTree(t)
	empty := filepath.Join(root, "empty.txt")

	pathsOf := func(ts []ScanTarget) map[string]ScanTarget {
		m := make(map[string]ScanTarget, len(ts))
		for _, st := range ts {
			m[st.Path] = st
		}
		return m
	}

	t.Run("default emits empty files with zero size", func(t *testing.T) {
		s := &Files{Path: root}
		for walker, targets := range map[string][]ScanTarget{
			"parallel": collectParallel(t, s),
			"serial":   collectSerial(t, s),
		} {
			got := pathsOf(targets)
			st, ok := got[empty]
			assert.True(t, ok, "%s: empty file must be emitted when the stat is deferred", walker)
			assert.Zero(t, st.Size, "%s: deferred stat must leave Size zero", walker)
		}
	})

	t.Run("max-file-size restores stat and gates", func(t *testing.T) {
		s := &Files{Path: root, MaxFileSize: 1024}
		for walker, targets := range map[string][]ScanTarget{
			"parallel": collectParallel(t, s),
			"serial":   collectSerial(t, s),
		} {
			got := pathsOf(targets)
			_, ok := got[empty]
			assert.False(t, ok, "%s: empty file must be skipped when stat is required", walker)
			_, ok = got[filepath.Join(root, "big.bin")]
			assert.False(t, ok, "%s: over-size file must be skipped", walker)
			st, ok := got[filepath.Join(root, "top.txt")]
			require.True(t, ok, "%s: regular file must be emitted", walker)
			assert.Equal(t, int64(10), st.Size, "%s: stat mode must populate Size", walker)
		}
	})
}

// TestScanTargetsParallelEdgeRoots covers the root shapes that bypass or
// stress the worker pool: single-file root, missing root, empty dir root.
func TestScanTargetsParallelEdgeRoots(t *testing.T) {
	t.Run("single-file root", func(t *testing.T) {
		dir := t.TempDir()
		file := filepath.Join(dir, "only.txt")
		require.NoError(t, os.WriteFile(file, []byte("data"), 0o644))
		s := &Files{Path: file}
		got := collectParallel(t, s)
		require.Len(t, got, 1)
		assert.Equal(t, file, got[0].Path)
	})

	t.Run("missing root is a no-op, not an error", func(t *testing.T) {
		s := &Files{Path: filepath.Join(t.TempDir(), "does-not-exist")}
		assert.Empty(t, collectParallel(t, s))
	})

	t.Run("empty dir root", func(t *testing.T) {
		s := &Files{Path: t.TempDir()}
		assert.Empty(t, collectParallel(t, s))
	})

	t.Run("wide flat root saturates the pool", func(t *testing.T) {
		dir := t.TempDir()
		// More entries than walkers so the queue drains and refills while
		// workers race on the condvar; equivalence still must hold.
		for i := 0; i < runtime.NumCPU()*8; i++ {
			sub := filepath.Join(dir, fmt.Sprintf("d%03d", i))
			require.NoError(t, os.MkdirAll(sub, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(sub, "f.txt"), []byte("x"), 0o644))
		}
		s := &Files{Path: dir}
		want := sortTargets(collectSerial(t, s))
		got := sortTargets(collectParallel(t, s))
		assert.Equal(t, want, got)
	})
}
