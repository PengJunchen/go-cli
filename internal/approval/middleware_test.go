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
	key, err := sessionKey(call(name))
	require.NoError(t, err)
	return key
}

func TestMiddlewareSatisfiesInterface(t *testing.T) {
	var _ interface{ Name() string } = NewApprovalMiddleware(&AllowAllClassifier{}, newStubStore())
}
