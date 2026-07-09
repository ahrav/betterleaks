package exprruntime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEvalPooledEnvIsolation guards the env-map pool in evalBindings: a
// recycled map still holds the previous eval's dynamic keys, which is safe
// only because every dynamic key is unconditionally overwritten before the
// VM runs. If that invariant breaks, a second eval could observe the FIRST
// eval's finding/attributes and flip its verdict — these expressions are
// crafted so any leak changes the result.
func TestEvalPooledEnvIsolation(t *testing.T) {
	rt, err := New(nil)
	require.NoError(t, err)

	t.Run("filter finding does not leak between evals", func(t *testing.T) {
		prg, err := rt.CompileFilter(`finding["secret"] == "LEAK"`, nil)
		require.NoError(t, err)

		got, err := rt.EvalFilter(prg, map[string]string{"secret": "LEAK"}, map[string]string{"k": "v"})
		require.NoError(t, err)
		require.True(t, got, "first eval must match its own finding")

		// Same pooled env, different maps: must see only the new values.
		got, err = rt.EvalFilter(prg, map[string]string{"secret": "clean"}, map[string]string{})
		require.NoError(t, err)
		assert.False(t, got, "second eval observed the first eval's finding through the pooled env")
	})

	t.Run("absent key must read as absent after a populated eval", func(t *testing.T) {
		// get(...) with a fallback distinguishes "key missing" from any
		// stale value: a leak of x=stale from eval 1 would return "stale"
		// instead of the fallback in eval 2.
		prg, err := rt.CompileFilter(`get(finding, "x", "FALLBACK") == "FALLBACK"`, nil)
		require.NoError(t, err)

		got, err := rt.EvalFilter(prg, map[string]string{"x": "stale"}, nil)
		require.NoError(t, err)
		require.False(t, got, "first eval has x; must not take the fallback")

		got, err = rt.EvalFilter(prg, map[string]string{"y": "other"}, nil)
		require.NoError(t, err)
		assert.True(t, got, "second eval's finding lacks x; a stale x leaked through the pooled env")
	})

	t.Run("prefilter attributes do not leak between evals", func(t *testing.T) {
		prg, err := rt.CompilePrefilter(`get(attributes, "path", "") == "skip.png"`)
		require.NoError(t, err)

		got, err := rt.EvalPrefilter(prg, map[string]string{"path": "skip.png"})
		require.NoError(t, err)
		require.True(t, got)

		got, err = rt.EvalPrefilter(prg, map[string]string{"other": "attr"})
		require.NoError(t, err)
		assert.False(t, got, "second eval observed the first eval's attributes through the pooled env")
	})
}
