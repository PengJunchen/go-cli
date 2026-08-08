package tools

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMutationRespectsContextCancellation verifies that applySafe returns the
// context error without invoking the handler when the context is already
// cancelled.
func TestMutationRespectsContextCancellation(t *testing.T) {
	handlerCalled := false
	q := &DefaultFileMutationQueue{
		handler: func(ctx context.Context, m FileMutation) (*ToolResult, error) {
			handlerCalled = true
			return &ToolResult{Output: "should not reach"}, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tr, err := q.applySafe(ctx, FileMutation{FilePath: "x.txt", Operation: "write"})
	require.Error(t, err)
	assert.Nil(t, tr)
	assert.False(t, handlerCalled, "handler must not run when context is cancelled")
	assert.Contains(t, err.Error(), "context canceled")
}

// TestMutationApplySafeNilContextPanics is a sanity check that a nil context
// (which should never happen in practice) is caught by ctx.Err() rather than
// causing a nil-pointer dereference inside applySafe. context.Context(nil).Err()
// would panic, so applySafe's caller must always pass a non-nil context. This
// test documents that contract by ensuring a normal background context works.
func TestMutationApplySafeBackgroundContext(t *testing.T) {
	q := &DefaultFileMutationQueue{
		handler: func(ctx context.Context, m FileMutation) (*ToolResult, error) {
			return &ToolResult{Output: "ok"}, nil
		},
	}

	tr, err := q.applySafe(context.Background(), FileMutation{FilePath: "x.txt", Operation: "write"})
	require.NoError(t, err)
	require.NotNil(t, tr)
	assert.Equal(t, "ok", tr.Output)
}

// TestMutationQueuePassesContextToHandler verifies that the context supplied to
// Enqueue is carried through queuedMutation → startWorker → applySafe → apply
// and reaches the handler. This confirms the ctx field wiring.
func TestMutationQueuePassesContextToHandler(t *testing.T) {
	type ctxKey struct{}
	marker := ctxKey{}

	ctxReceived := make(chan context.Context, 1)
	q := NewDefaultFileMutationQueue(WithMutationHandler(func(ctx context.Context, m FileMutation) (*ToolResult, error) {
		ctxReceived <- ctx
		return &ToolResult{Output: "done"}, nil
	}))
	defer func() { _ = q.(*DefaultFileMutationQueue).Close() }()

	myCtx := context.WithValue(context.Background(), marker, "hello")
	resCh, err := q.Enqueue(myCtx, FileMutation{
		FilePath:  "test_ctx.txt",
		Operation: "write",
		Content:   "data",
	})
	require.NoError(t, err)

	select {
	case gotCtx := <-ctxReceived:
		assert.Equal(t, "hello", gotCtx.Value(marker))
	case <-time.After(2 * time.Second):
		t.Fatal("handler was not called within timeout")
	}

	select {
	case res := <-resCh:
		assert.True(t, res.Success)
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive result within timeout")
	}
}
