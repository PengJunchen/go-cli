package approval

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// MockApprovalCallback is a test double for ApprovalCallback that records how
// many times it was called and returns a programmable result.
type MockApprovalCallback struct {
	mu     sync.Mutex
	calls  int
	result ApprovalResult
	err    error
}

var _ ApprovalCallback = (*MockApprovalCallback)(nil)

func (m *MockApprovalCallback) RequestApproval(_ context.Context, _ string, _ map[string]any) (ApprovalResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return m.result, m.err
}

func (m *MockApprovalCallback) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// --- InteractiveApprovalCallback tests ---

func TestInteractiveApprovalAllow(t *testing.T) {
	out := &bytes.Buffer{}
	cb := NewInteractiveApprovalCallback(strings.NewReader("y\n"), out)
	result, err := cb.RequestApproval(context.Background(), "bash", nil)
	require.NoError(t, err)
	assert.Equal(t, ApprovalAllow, result)
	assert.Contains(t, out.String(), "Allow? (y/n/a)")
}

func TestInteractiveApprovalDeny(t *testing.T) {
	out := &bytes.Buffer{}
	cb := NewInteractiveApprovalCallback(strings.NewReader("n\n"), out)
	result, err := cb.RequestApproval(context.Background(), "bash", nil)
	require.NoError(t, err)
	assert.Equal(t, ApprovalDeny, result)
}

func TestInteractiveApprovalAlwaysAllow(t *testing.T) {
	out := &bytes.Buffer{}
	cb := NewInteractiveApprovalCallback(strings.NewReader("a\n"), out)
	result, err := cb.RequestApproval(context.Background(), "bash", nil)
	require.NoError(t, err)
	assert.Equal(t, ApprovalAlwaysAllow, result)
}

func TestInteractiveApprovalInvalidThenDeny(t *testing.T) {
	out := &bytes.Buffer{}
	// "invalid" does not match y/n/a; the next read hits EOF so the call is
	// denied.
	cb := NewInteractiveApprovalCallback(strings.NewReader("invalid\n"), out)
	result, err := cb.RequestApproval(context.Background(), "bash", nil)
	require.NoError(t, err)
	assert.Equal(t, ApprovalDeny, result)
	assert.Contains(t, out.String(), "Invalid input")
}

func TestInteractiveApprovalYesWord(t *testing.T) {
	out := &bytes.Buffer{}
	cb := NewInteractiveApprovalCallback(strings.NewReader("yes\n"), out)
	result, err := cb.RequestApproval(context.Background(), "bash", nil)
	require.NoError(t, err)
	assert.Equal(t, ApprovalAllow, result)
}

func TestInteractiveApprovalNoNewline(t *testing.T) {
	out := &bytes.Buffer{}
	// Input without trailing newline: ReadString returns the data plus EOF.
	cb := NewInteractiveApprovalCallback(strings.NewReader("y"), out)
	result, err := cb.RequestApproval(context.Background(), "bash", nil)
	require.NoError(t, err)
	assert.Equal(t, ApprovalAllow, result)
}

// --- ApprovalCache tests ---

func TestApprovalCacheSetGet(t *testing.T) {
	cache := NewApprovalCache("")
	cache.Set("bash:abc123")

	allowed, ok := cache.Get("bash:abc123")
	assert.True(t, ok, "key should be found")
	assert.True(t, allowed, "key should be allowed")

	// A missing key reports not-found.
	_, ok = cache.Get("missing")
	assert.False(t, ok, "missing key should not be found")
}

func TestApprovalCacheRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")

	cache := NewApprovalCache(path)
	cache.Set("bash:abc")
	cache.Set("read_file:def")
	require.NoError(t, cache.SaveToFile(path))

	loaded := NewApprovalCache("")
	require.NoError(t, loaded.LoadFromFile(path))

	allowed, ok := loaded.Get("bash:abc")
	assert.True(t, ok)
	assert.True(t, allowed)

	allowed, ok = loaded.Get("read_file:def")
	assert.True(t, ok)
	assert.True(t, allowed)

	_, ok = loaded.Get("nonexistent")
	assert.False(t, ok)
}

func TestApprovalCacheMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does_not_exist.json")
	cache := NewApprovalCache("")
	require.NoError(t, cache.LoadFromFile(path))

	_, ok := cache.Get("anything")
	assert.False(t, ok, "cache should be empty after missing-file load")
}

func TestApprovalCacheConcurrentAccess(t *testing.T) {
	cache := NewApprovalCache("")

	var wg sync.WaitGroup
	const n = 50
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			cache.Set("key")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			cache.Get("key")
		}
	}()
	wg.Wait()
}

// --- ApprovalCache permission tests ---

func TestApprovalCacheSaveFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	cache := NewApprovalCache(path)
	cache.Set("bash:abc")

	require.NoError(t, cache.SaveToFile(path))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"saved cache file must have 0600 permissions")
}

func TestApprovalCacheSaveTightensExistingPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	// Pre-create the file with overly permissive permissions.
	require.NoError(t, os.WriteFile(path, []byte(`{"old":true}`), 0o644))

	cache := NewApprovalCache(path)
	cache.Set("bash:abc")
	require.NoError(t, cache.SaveToFile(path))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"existing file with broader permissions must be tightened to 0600")
}

func TestApprovalCacheLoadRejects0644(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"bash:abc":true}`), 0o644))

	cache := NewApprovalCache("")
	err := cache.LoadFromFile(path)
	require.Error(t, err, "0644 permissions must be rejected")
	assert.Contains(t, err.Error(), "expected 0600")
}

func TestApprovalCacheLoadRejects0666(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"bash:abc":true}`), 0o666))

	cache := NewApprovalCache("")
	err := cache.LoadFromFile(path)
	require.Error(t, err, "0666 permissions must be rejected")
	assert.Contains(t, err.Error(), "expected 0600")
}

func TestApprovalCacheLoadCorrectPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"bash:abc":true}`), 0o600))

	cache := NewApprovalCache("")
	require.NoError(t, cache.LoadFromFile(path))

	allowed, ok := cache.Get("bash:abc")
	assert.True(t, ok)
	assert.True(t, allowed)
}

func TestApprovalCacheLoadWarnsOnIncorrectPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"bash:abc":true}`), 0o644))

	lc := verify.NewLogCapturer()
	lc.Attach(t.Context())
	defer lc.Detach()

	cache := NewApprovalCache("")
	err := cache.LoadFromFile(path)
	require.Error(t, err)

	entries := lc.Entries()
	found := false
	for _, e := range entries {
		if e.Level == slog.LevelWarn && strings.Contains(e.Message, "insecure_permissions") {
			found = true
			break
		}
	}
	assert.True(t, found, "a warning log should be emitted for insecure permissions")
}

// --- Middleware with callback tests ---

func TestMiddlewareAskCallbackAllow(t *testing.T) {
	cb := &MockApprovalCallback{result: ApprovalAllow}
	mw := NewApprovalMiddleware(&countingClassifier{decision: Ask}, newStubStore(), WithCallback(cb))
	wrapped := mw.WrapToolCall(nextEcho())

	res, err := wrapped(context.Background(), call("read_file"))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, "ran:read_file", res.Output)
	assert.Equal(t, 1, cb.callCount(), "callback should be invoked once")
}

func TestMiddlewareAskCallbackDeny(t *testing.T) {
	cb := &MockApprovalCallback{result: ApprovalDeny}
	mw := NewApprovalMiddleware(&countingClassifier{decision: Ask}, newStubStore(), WithCallback(cb))

	called := false
	wrapped := mw.WrapToolCall(func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
		called = true
		return nil, nil
	})

	res, err := wrapped(context.Background(), call("read_file"))
	require.ErrorIs(t, err, ErrToolDenied)
	assert.Nil(t, res)
	assert.False(t, called, "denied tool must not be executed")
	assert.Equal(t, 1, cb.callCount())
}

func TestMiddlewareAskCallbackAlwaysAllow(t *testing.T) {
	cb := &MockApprovalCallback{result: ApprovalAlwaysAllow}
	cache := NewApprovalCache("")
	mw := NewApprovalMiddleware(&countingClassifier{decision: Ask}, newStubStore(),
		WithCallback(cb), WithCache(cache))
	wrapped := mw.WrapToolCall(nextEcho())

	res, err := wrapped(context.Background(), call("read_file"))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, "ran:read_file", res.Output)

	// The always-allow decision must be persisted to the cache.
	key, err := sessionKey(call("read_file"), PermissionDefault)
	require.NoError(t, err)
	allowed, ok := cache.Get(key)
	assert.True(t, ok, "cache should contain the key after AlwaysAllow")
	assert.True(t, allowed)
}

func TestMiddlewareAskCallbackSecondCallSkipsCallback(t *testing.T) {
	cb := &MockApprovalCallback{result: ApprovalAlwaysAllow}
	cache := NewApprovalCache("")
	mw := NewApprovalMiddleware(&countingClassifier{decision: Ask}, newStubStore(),
		WithCallback(cb), WithCache(cache))
	wrapped := mw.WrapToolCall(nextEcho())

	// First call: callback is invoked and returns AlwaysAllow.
	res, err := wrapped(context.Background(), call("read_file"))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 1, cb.callCount(), "callback invoked on first call")

	// Second call: the session cache (and approval cache) hit, so the
	// callback must not be re-invoked.
	res2, err := wrapped(context.Background(), call("read_file"))
	require.NoError(t, err)
	require.NotNil(t, res2)
	assert.Equal(t, 1, cb.callCount(), "callback must not be invoked on cached call")
}

func TestMiddlewareAskCallbackCacheHitSkipsCallback(t *testing.T) {
	cb := &MockApprovalCallback{result: ApprovalDeny}
	cache := NewApprovalCache("")

	// Pre-populate the cache so the callback should never be reached.
	key, err := sessionKey(call("read_file"), PermissionDefault)
	require.NoError(t, err)
	cache.Set(key)

	mw := NewApprovalMiddleware(&countingClassifier{decision: Ask}, newStubStore(),
		WithCallback(cb), WithCache(cache))
	wrapped := mw.WrapToolCall(nextEcho())

	res, err := wrapped(context.Background(), call("read_file"))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, "ran:read_file", res.Output)
	assert.Equal(t, 0, cb.callCount(), "cache hit must skip the callback entirely")
}

func TestMiddlewareAskNoCallbackDeniesByDefault(t *testing.T) {
	mw := NewApprovalMiddleware(&countingClassifier{decision: Ask}, newStubStore())
	called := false
	wrapped := mw.WrapToolCall(func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
		called = true
		return nil, nil
	})

	res, err := wrapped(context.Background(), call("read_file"))
	require.ErrorIs(t, err, ErrToolDenied)
	assert.Nil(t, res)
	assert.False(t, called, "Ask without callback must be denied")
}

func TestMiddlewareAskCallbackErrorDenies(t *testing.T) {
	cb := &MockApprovalCallback{err: assert.AnError}
	mw := NewApprovalMiddleware(&countingClassifier{decision: Ask}, newStubStore(), WithCallback(cb))

	called := false
	wrapped := mw.WrapToolCall(func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
		called = true
		return nil, nil
	})

	res, err := wrapped(context.Background(), call("read_file"))
	require.ErrorIs(t, err, ErrToolDenied)
	assert.Nil(t, res)
	assert.False(t, called, "callback error must result in denial")
}
