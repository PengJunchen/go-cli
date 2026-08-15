package approval

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// countingClassifier records how many times it was called and returns a fixed
// or programmable classification.
type countingClassifier struct {
	mu       sync.Mutex
	calls    int
	decision Classification
}

func (c *countingClassifier) Name() string { return "stub" }
func (c *countingClassifier) Classify(_ context.Context, _ tools.ToolCall) Classification {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.decision
}
func (c *countingClassifier) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// stubStore records Get/Set activity and returns a programmable decision.
type stubStore struct {
	mu       sync.Mutex
	data     map[string]Classification
	gets     int
	decision Classification
	found    bool
	err      error
}

func newStubStore() *stubStore {
	return &stubStore{data: map[string]Classification{}}
}
func (s *stubStore) Get(_ context.Context, key string) (Classification, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.err != nil {
		return Allow, false, s.err
	}
	if val, ok := s.data[key]; ok {
		return val, true, nil
	}
	return s.decision, s.found, nil
}
func (s *stubStore) Set(_ context.Context, key string, c Classification) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = c
	return nil
}
func (s *stubStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}
func (s *stubStore) getCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

func nextEcho() func(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	return func(_ context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		return &tools.ToolResult{Output: "ran:" + call.Name, ToolCallID: call.ID}, nil
	}
}

func TestMiddlewareAllowPassThrough(t *testing.T) {
	mw := NewApprovalMiddleware(&AllowAllClassifier{}, newStubStore())

	inner := func(_ context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		return &tools.ToolResult{Output: "ran:" + call.Name}, nil
	}
	wrapped := mw.WrapToolCall(inner)
	res, err := wrapped(context.Background(), call("read_file"))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, "ran:read_file", res.Output)
}

func TestMiddlewareDenyFirstDoesNotCallNext(t *testing.T) {
	mw := NewApprovalMiddleware(&DenyAllClassifier{}, newStubStore())
	called := false
	inner := func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
		called = true
		return nil, nil
	}
	wrapped := mw.WrapToolCall(inner)
	res, err := wrapped(context.Background(), call("read_file"))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrToolDenied)
	assert.Nil(t, res)
	assert.False(t, called, "denied tool must not be executed")
}

// TestMiddlewareNilClassifierDefaultsToDenyAll verifies that when a nil
// classifier is passed to NewApprovalMiddleware, it falls back to
// DenyAllClassifier so tool calls are denied by default (fail-safe).
func TestMiddlewareNilClassifierDefaultsToDenyAll(t *testing.T) {
	mw := NewApprovalMiddleware(nil, newStubStore())
	called := false
	inner := func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
		called = true
		return nil, nil
	}
	wrapped := mw.WrapToolCall(inner)
	res, err := wrapped(context.Background(), call("read_file"))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrToolDenied)
	assert.Nil(t, res)
	assert.False(t, called, "nil classifier must default to DenyAll")
}

func TestMiddlewareSessionCacheSkipsClassifier(t *testing.T) {
	classifier := &countingClassifier{decision: Allow}
	mw := NewApprovalMiddleware(classifier, newStubStore())

	inner := nextEcho()
	wrapped := mw.WrapToolCall(inner)

	res, err := wrapped(context.Background(), call("read_file"))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, 1, classifier.count(), "first call classifies exactly once")

	// Same args -> session cache hit; classifier not re-invoked.
	res2, err := wrapped(context.Background(), call("read_file"))
	require.NoError(t, err)
	require.NotNil(t, res2)
	assert.Equal(t, 1, classifier.count(), "cached call must not re-classify")
}

func TestMiddlewareCrossSessionStoreHitSkipsClassifier(t *testing.T) {
	store := newStubStore()
	require.NoError(t, store.Set(context.Background(), sessionKeyStr(t, "read_file"), Allow))

	classifier := &countingClassifier{decision: Deny}
	mw := NewApprovalMiddleware(classifier, store)

	wrapped := mw.WrapToolCall(nextEcho())
	res, err := wrapped(context.Background(), call("read_file"))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 0, classifier.count(), "store hit must not invoke classifier")
}

func TestMiddlewareAskResolvesToDenyByDefault(t *testing.T) {
	mw := NewApprovalMiddleware(&countingClassifier{decision: Ask}, newStubStore())
	called := false
	wrapped := mw.WrapToolCall(func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
		called = true
		return &tools.ToolResult{}, nil
	})
	res, err := wrapped(context.Background(), call("read_file"))
	require.ErrorIs(t, err, ErrToolDenied)
	assert.Nil(t, res)
	assert.False(t, called, "Ask without auto-approval must be refused")
}

func TestMiddlewareAskResolvesToAllowWithAutoApprove(t *testing.T) {
	mw := NewApprovalMiddleware(&countingClassifier{decision: Ask}, newStubStore(), WithAutoApprove(true))
	wrapped := mw.WrapToolCall(nextEcho())
	res, err := wrapped(context.Background(), call("read_file"))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, "ran:read_file", res.Output)
}

func TestMiddlewareEmitsApprovalDecisionSpan(t *testing.T) {
	exporter := mock.NewMockTraceExporter()
	tr := tracing.NewTracer("approval-trace", exporter)
	root, rootCtx := tr.Start(context.Background(), "root", tracing.SpanKindInternal)

	mw := NewApprovalMiddleware(&AllowAllClassifier{}, newStubStore())
	wrapped := mw.WrapToolCall(nextEcho())
	res, err := wrapped(rootCtx, call("read_file"))
	require.NoError(t, err)
	require.NotNil(t, res)

	root.End()
	require.Eventually(t, func() bool {
		return exporter.SpanCount() >= 2
	}, time.Second, 5*time.Millisecond, "expected approval.decision span to be exported")

	exporter.AssertSpanExists(t, "approval.decision")
	exporter.AssertSpanChain(t)

	// The decision span must be a child of the root (parent chain integrity).
	var decision tracing.SpanData
	for _, span := range exporter.Spans() {
		if span.Name == "approval.decision" {
			decision = span
			break
		}
	}
	require.NotEmpty(t, decision.SpanID)
	assert.Equal(t, root.SpanID(), decision.ParentSpanID, "approval.decision must nest under root")
	assert.Equal(t, "approval-trace", decision.TraceID)
}

func TestMiddlewareCacheMissOnDifferentArgs(t *testing.T) {
	classifier := &countingClassifier{decision: Allow}
	mw := NewApprovalMiddleware(classifier, newStubStore())
	wrapped := mw.WrapToolCall(nextEcho())

	_, err := wrapped(context.Background(), tools.ToolCall{ID: "1", Name: "read_file", Args: map[string]any{"path": "a"}})
	require.NoError(t, err)
	_, err = wrapped(context.Background(), tools.ToolCall{ID: "2", Name: "read_file", Args: map[string]any{"path": "b"}})
	require.NoError(t, err)
	assert.Equal(t, 2, classifier.count(), "different args must not hit the cache")
}

// sessionKeyStr returns the decision key for a call name to pre-seed a store.
func sessionKeyStr(t *testing.T, name string) string {
	t.Helper()
	key, err := sessionKey(call(name), PermissionDefault)
	require.NoError(t, err)
	return key
}

func TestMiddlewareSatisfiesInterface(t *testing.T) {
	var _ interface{ Name() string } = NewApprovalMiddleware(&AllowAllClassifier{}, newStubStore())
}

// fixedResolver returns the same classifier regardless of mode. It is used in
// cache-clearing tests where the classifier must be stable across mode switches
// so call counts reflect cache hits/misses, not classifier changes.
type fixedResolver struct {
	clf ApprovalClassifier
}

func (r *fixedResolver) Name() string                                { return "fixed" }
func (r *fixedResolver) Resolve(_ PermissionMode) ApprovalClassifier { return r.clf }

// AC-1: Allow cached in AutoFull mode is not hit when switched back to Default.
func TestMiddlewareModeSwitchPreventsCrossModeCacheHit(t *testing.T) {
	resolver := NewDefaultPermissionModeResolver()
	store := newStubStore()

	// Start in AutoFull mode: AllowAllClassifier allows every call.
	mw := NewApprovalMiddleware(&DenyAllClassifier{}, store,
		WithPermissionModeResolver(resolver),
		WithPermissionMode(PermissionAutoFull))

	wrapped := mw.WrapToolCall(nextEcho())

	// First call in AutoFull: allowed and cached under the "auto_full" key.
	res, err := wrapped(context.Background(), call("bash"))
	require.NoError(t, err, "AutoFull mode must allow the call")
	require.NotNil(t, res)

	// Switch to Default mode: SafetyPolicyClassifier returns Ask for "bash"
	// (not read-only, not forbidden), which resolves to Deny without a callback.
	mw.SetPermissionMode(PermissionDefault)

	// Same call in Default mode: must NOT reuse the cached Allow from AutoFull.
	_, err = wrapped(context.Background(), call("bash"))
	require.ErrorIs(t, err, ErrToolDenied, "Default mode must not reuse AutoFull's cached Allow")
}

// AC-2: Mode switch triggers session cache clear. After SetPermissionMode, a
// subsequent call must re-classify instead of hitting the session cache.
func TestMiddlewareSetPermissionModeClearsSessionCache(t *testing.T) {
	counter := &countingClassifier{decision: Allow}
	resolver := &fixedResolver{clf: counter}

	mw := NewApprovalMiddleware(&AllowAllClassifier{}, newStubStore(),
		WithPermissionModeResolver(resolver),
		WithPermissionMode(PermissionAutoFull))

	wrapped := mw.WrapToolCall(nextEcho())

	// First call: classifies (count = 1) and caches in session.
	_, err := wrapped(context.Background(), call("read_file"))
	require.NoError(t, err)
	require.Equal(t, 1, counter.count(), "first call must classify")

	// Second call: session cache hit, classifier not re-invoked.
	_, err = wrapped(context.Background(), call("read_file"))
	require.NoError(t, err)
	require.Equal(t, 1, counter.count(), "cached call must not re-classify")

	// Switch mode: clears the session cache.
	mw.SetPermissionMode(PermissionDefault)

	// Third call: cache was cleared, must re-classify.
	_, err = wrapped(context.Background(), call("read_file"))
	require.NoError(t, err)
	require.Equal(t, 2, counter.count(), "mode switch must clear session cache so call re-classifies")
}

// ClearSession directly empties the session cache so the next call must consult
// the store again instead of hitting the session cache.
func TestMiddlewareClearSession(t *testing.T) {
	counter := &countingClassifier{decision: Allow}
	resolver := &fixedResolver{clf: counter}
	store := newStubStore()

	mw := NewApprovalMiddleware(&AllowAllClassifier{}, store,
		WithPermissionModeResolver(resolver),
		WithPermissionMode(PermissionAutoFull))

	wrapped := mw.WrapToolCall(nextEcho())

	// First call: classifies, caches in session, and persists to store.
	_, err := wrapped(context.Background(), call("read_file"))
	require.NoError(t, err)
	require.Equal(t, 1, counter.count(), "first call must classify")
	require.Equal(t, 1, store.getCount(), "first call must consult store once")

	// Second call: session cache hit — neither store nor classifier consulted.
	_, err = wrapped(context.Background(), call("read_file"))
	require.NoError(t, err)
	require.Equal(t, 1, counter.count(), "cached call must not re-classify")
	require.Equal(t, 1, store.getCount(), "cached call must not consult store")

	// Clear session cache.
	mw.ClearSession()

	// Third call: session cache was cleared, must consult store again.
	_, err = wrapped(context.Background(), call("read_file"))
	require.NoError(t, err)
	require.Equal(t, 2, store.getCount(), "ClearSession must force store consultation")
	require.Equal(t, 1, counter.count(), "store hit should still skip classifier")
}

// AC-3: verify no data races between concurrent mode switches and tool calls.
func TestMiddlewareConcurrentModeSwitchAndCalls(t *testing.T) {
	resolver := NewDefaultPermissionModeResolver()
	mw := NewApprovalMiddleware(&AllowAllClassifier{}, newStubStore(),
		WithPermissionModeResolver(resolver),
		WithPermissionMode(PermissionAutoFull))

	wrapped := mw.WrapToolCall(nextEcho())

	var wg sync.WaitGroup
	modes := []PermissionMode{PermissionDefault, PermissionAutoFull, PermissionAuto, PermissionPlan}

	// Writer: rapidly switch modes.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			mw.SetPermissionMode(modes[i%len(modes)])
		}
	}()

	// Readers: rapidly make tool calls.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = wrapped(context.Background(), call("read_file"))
			}
		}()
	}

	wg.Wait()
}
