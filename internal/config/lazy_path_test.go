package config

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigPathLazyEvaluated verifies that the lazyPath defers the resolve
// function until Get is called for the first time.
func TestConfigPathLazyEvaluated(t *testing.T) {
	t.Parallel()

	var calls int32
	lp := newLazyPath(func() string {
		atomic.AddInt32(&calls, 1)
		return "/resolved/path"
	})

	// Before Get, the resolver must not have run.
	assert.Equal(t, int32(0), atomic.LoadInt32(&calls), "resolve must not run before Get")

	val := lp.Get()
	assert.Equal(t, "/resolved/path", val)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "resolve must run exactly once after Get")
}

// TestConfigPathCachedAfterFirstEval verifies that the lazyPath caches the
// resolved value and does not invoke the resolver on subsequent Get calls.
func TestConfigPathCachedAfterFirstEval(t *testing.T) {
	t.Parallel()

	var calls int32
	lp := newLazyPath(func() string {
		atomic.AddInt32(&calls, 1)
		return "/cached/path"
	})

	// First Get resolves.
	v1 := lp.Get()
	require.Equal(t, "/cached/path", v1)

	// Subsequent Gets return the cached value without re-resolving.
	for i := 0; i < 10; i++ {
		v := lp.Get()
		assert.Equal(t, "/cached/path", v)
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "resolve must run only once")
}

// TestResolveHistoryPathUsesConfigValue verifies that resolveHistoryPath
// returns the configured path when one is provided.
func TestResolveHistoryPathUsesConfigValue(t *testing.T) {
	t.Parallel()

	lp := resolveHistoryPath("/custom/history.jsonl")
	assert.Equal(t, "/custom/history.jsonl", lp.Get())
}

// TestResolveHistoryPathFallsBackToHome verifies that resolveHistoryPath
// falls back to the home directory when no path is configured.
// Note: t.Parallel() omitted because t.Setenv mutates process environment.
func TestResolveHistoryPathFallsBackToHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)

	lp := resolveHistoryPath("")
	val := lp.Get()
	assert.Contains(t, val, ".go-cli")
	assert.Contains(t, val, "history.jsonl")
}
