package sources

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func envValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return e[len(prefix):], true
		}
	}
	return "", false
}

func envCount(env []string, key string) int {
	n := 0
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			n++
		}
	}
	return n
}

func TestGitConfigIsolationEnv(t *testing.T) {
	t.Run("delta-base cache config triple present", func(t *testing.T) {
		env := gitConfigIsolationEnv()
		count, ok := envValue(env, "GIT_CONFIG_COUNT")
		require.True(t, ok)
		assert.Equal(t, "1", count)
		key, _ := envValue(env, "GIT_CONFIG_KEY_0")
		assert.Equal(t, "core.deltaBaseCacheLimit", key)
		val, _ := envValue(env, "GIT_CONFIG_VALUE_0")
		assert.Equal(t, "128m", val)
	})

	t.Run("config isolation overrides applied", func(t *testing.T) {
		t.Setenv("GIT_CONFIG_GLOBAL", "/home/user/.gitconfig") // must be overridden, not duplicated
		env := gitConfigIsolationEnv()
		v, ok := envValue(env, "GIT_CONFIG_GLOBAL")
		require.True(t, ok)
		assert.NotEqual(t, "/home/user/.gitconfig", v)
		assert.Equal(t, 1, envCount(env, "GIT_CONFIG_GLOBAL"), "override must replace, not append")
		v, _ = envValue(env, "GIT_CONFIG_NOSYSTEM")
		assert.Equal(t, "1", v)
	})

	t.Run("MALLOC_ARENA_MAX set only when absent", func(t *testing.T) {
		os.Unsetenv("MALLOC_ARENA_MAX")
		env := gitConfigIsolationEnv()
		v, ok := envValue(env, "MALLOC_ARENA_MAX")
		require.True(t, ok, "arena cap must be applied when caller expressed no preference")
		assert.Equal(t, "2", v)

		t.Setenv("MALLOC_ARENA_MAX", "8")
		env = gitConfigIsolationEnv()
		v, _ = envValue(env, "MALLOC_ARENA_MAX")
		assert.Equal(t, "8", v, "caller preference must win")
		assert.Equal(t, 1, envCount(env, "MALLOC_ARENA_MAX"))
	})

	t.Run("LD_PRELOAD injected only for an existing zlib path", func(t *testing.T) {
		os.Unsetenv("LD_PRELOAD")

		lib := filepath.Join(t.TempDir(), "libz.so.1")
		require.NoError(t, os.WriteFile(lib, []byte("elf"), 0o644))
		t.Setenv("BETTERLEAKS_GIT_ZLIB", lib)
		env := gitConfigIsolationEnv()
		v, ok := envValue(env, "LD_PRELOAD")
		require.True(t, ok)
		assert.Equal(t, lib, v)

		t.Setenv("BETTERLEAKS_GIT_ZLIB", filepath.Join(t.TempDir(), "missing.so"))
		env = gitConfigIsolationEnv()
		_, ok = envValue(env, "LD_PRELOAD")
		assert.False(t, ok, "nonexistent zlib path must not be preloaded")

		t.Setenv("BETTERLEAKS_GIT_ZLIB", "")
		env = gitConfigIsolationEnv()
		_, ok = envValue(env, "LD_PRELOAD")
		assert.False(t, ok, "unset knob must not inject LD_PRELOAD")
	})
}

func TestSetEnvVar(t *testing.T) {
	t.Run("replaces existing key in place", func(t *testing.T) {
		env := []string{"A=1", "LD_PRELOAD=old.so", "B=2"}
		out := setEnvVar(env, "LD_PRELOAD", "new.so")
		v, _ := envValue(out, "LD_PRELOAD")
		assert.Equal(t, "new.so", v)
		assert.Equal(t, 1, envCount(out, "LD_PRELOAD"))
		assert.Len(t, out, 3)
	})

	t.Run("appends when absent", func(t *testing.T) {
		env := []string{"A=1"}
		out := setEnvVar(env, "LD_PRELOAD", "lib.so")
		v, ok := envValue(out, "LD_PRELOAD")
		require.True(t, ok)
		assert.Equal(t, "lib.so", v)
		assert.Len(t, out, 2)
	})

	t.Run("key prefix must not match longer keys", func(t *testing.T) {
		env := []string{"LD_PRELOAD_EXTRA=x"}
		out := setEnvVar(env, "LD_PRELOAD", "lib.so")
		v, _ := envValue(out, "LD_PRELOAD_EXTRA")
		assert.Equal(t, "x", v, "unrelated longer key clobbered")
		v, ok := envValue(out, "LD_PRELOAD")
		require.True(t, ok)
		assert.Equal(t, "lib.so", v)
	})
}
