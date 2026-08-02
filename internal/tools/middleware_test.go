package tools

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// fakeQueue is a minimal FileMutationQueue giving tests synchronous control
// over the result channel and captured mutations without spawning workers.
type fakeQueue struct {
	mu         sync.Mutex
	captured   []FileMutation
	enqueueErr error
	resultCh   chan FileMutationResult
}

func newFakeQueue() *fakeQueue {
	return &fakeQueue{resultCh: make(chan FileMutationResult, 1)}
}

func (f *fakeQueue) Enqueue(_ context.Context, m FileMutation) (<-chan FileMutationResult, error) {
	f.mu.Lock()
	f.captured = append(f.captured, m)
	f.mu.Unlock()
	if f.enqueueErr != nil {
		return nil, f.enqueueErr
	}
	return f.resultCh, nil
}

func (f *fakeQueue) Name() string { return "fake" }

func (f *fakeQueue) mutations() []FileMutation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FileMutation(nil), f.captured...)
}

// TestWithMutationQueueNilNextReturnsError guards the constructor precondition:
// a nil next handler must surface an error instead of panicking on dispatch.
func TestWithMutationQueueNilNextReturnsError(t *testing.T) {
	wrapped := WithMutationQueue(newFakeQueue(), nil)
	_, err := wrapped(context.Background(), ToolCall{Name: "write", Args: map[string]any{"path": "a", "content": "b"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-nil next")
}

// TestWithMutationQueueEditMutationQueued proves the edit branch routes through
// the queue with file_path extraction and an {old_string,new_string} payload,
// distinct from the already-tested write branch.
func TestWithMutationQueueEditMutationQueued(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	q := newFakeQueue()
	wrapped := WithMutationQueue(q, func(_ context.Context, _ ToolCall) (*ToolResult, error) {
		t.Fatal("next must not run for mutation tools")
		return nil, nil
	})

	q.resultCh <- FileMutationResult{Success: true}
	res, err := wrapped(context.Background(), ToolCall{
		Name: "edit",
		Args: map[string]any{"file_path": "main.go", "old_string": "a", "new_string": "b"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "edit queued and applied for main.go")
	assert.Equal(t, "main.go", res.Metadata["path"])
	assert.Equal(t, true, res.Metadata["queued"])

	ms := q.mutations()
	require.Len(t, ms, 1)
	assert.Equal(t, "main.go", ms[0].FilePath)
	assert.Equal(t, "edit", ms[0].Operation)
	assert.Equal(t, "edit", ms[0].ToolName)
	content, ok := ms[0].Content.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "a", content["old_string"])
	assert.Equal(t, "b", content["new_string"])
}

// TestWithMutationQueueEnqueueErrorWrapped ensures a failed enqueue is wrapped
// with the tool name so callers can attribute the failure.
func TestWithMutationQueueEnqueueErrorWrapped(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	q := newFakeQueue()
	q.enqueueErr = errors.New("queue closed")
	wrapped := WithMutationQueue(q, func(_ context.Context, _ ToolCall) (*ToolResult, error) {
		t.Fatal("next must not run when enqueue fails")
		return nil, nil
	})

	_, err := wrapped(context.Background(), ToolCall{Name: "write", Args: map[string]any{"path": "a.txt", "content": "x"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "enqueue write")
	assert.ErrorIs(t, err, q.enqueueErr)
}

// TestWithMutationQueueResultErrorPropagated verifies a worker-reported failure
// is returned as the tool error while still tagging the result metadata.
func TestWithMutationQueueResultErrorPropagated(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	boom := errors.New("disk full")
	q := newFakeQueue()
	wrapped := WithMutationQueue(q, func(_ context.Context, _ ToolCall) (*ToolResult, error) {
		t.Fatal("next must not run for mutation tools")
		return nil, nil
	})

	q.resultCh <- FileMutationResult{Success: false, Error: boom}
	res, err := wrapped(context.Background(), ToolCall{Name: "write", Args: map[string]any{"path": "a.txt", "content": "x"}})
	require.ErrorIs(t, err, boom)
	require.NotNil(t, res)
	assert.Empty(t, res.Output)
	assert.Equal(t, true, res.Metadata["queued"])
}

// TestWithMutationQueueContextCancellation confirms a canceled context wins the
// select when no result has been produced, returning ctx.Err() deterministically.
func TestWithMutationQueueContextCancellation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	q := newFakeQueue()
	wrapped := WithMutationQueue(q, func(_ context.Context, _ ToolCall) (*ToolResult, error) {
		t.Fatal("next must not run for mutation tools")
		return nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := wrapped(ctx, ToolCall{Name: "write", Args: map[string]any{"path": "a.txt", "content": "x"}})
	assert.ErrorIs(t, err, context.Canceled)
}

// TestMutationPathFromCall covers path extraction across the write (path) and
// edit (file_path) argument shapes plus the degenerate cases.
func TestMutationPathFromCall(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"write path key", map[string]any{"path": "/a/b.txt"}, "/a/b.txt"},
		{"edit file_path key", map[string]any{"file_path": "/c/d.go"}, "/c/d.go"},
		{"path takes precedence over file_path", map[string]any{"path": "p", "file_path": "fp"}, "p"},
		{"missing both keys", map[string]any{"content": "x"}, ""},
		{"non-string path ignored", map[string]any{"path": 123}, ""},
		{"non-string file_path ignored", map[string]any{"file_path": true}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, mutationPathFromCall(ToolCall{Args: tt.args}))
		})
	}
}

// TestMutationContentFromCall covers payload extraction for write/edit and the
// default nil for unknown tool names.
func TestMutationContentFromCall(t *testing.T) {
	t.Run("write returns content string", func(t *testing.T) {
		assert.Equal(t, "hello", mutationContentFromCall(ToolCall{Name: "write", Args: map[string]any{"content": "hello"}}))
	})
	t.Run("write without content returns empty", func(t *testing.T) {
		assert.Equal(t, "", mutationContentFromCall(ToolCall{Name: "write", Args: map[string]any{}}))
	})
	t.Run("write non-string content returns empty", func(t *testing.T) {
		assert.Equal(t, "", mutationContentFromCall(ToolCall{Name: "write", Args: map[string]any{"content": 42}}))
	})
	t.Run("edit returns old/new map", func(t *testing.T) {
		got := mutationContentFromCall(ToolCall{Name: "edit", Args: map[string]any{"old_string": "a", "new_string": "b"}})
		m, ok := got.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "a", m["old_string"])
		assert.Equal(t, "b", m["new_string"])
	})
	t.Run("edit partial keys", func(t *testing.T) {
		got := mutationContentFromCall(ToolCall{Name: "edit", Args: map[string]any{"old_string": "a"}})
		m, ok := got.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "a", m["old_string"])
		_, hasNew := m["new_string"]
		assert.False(t, hasNew)
	})
	t.Run("unknown tool returns nil", func(t *testing.T) {
		assert.Nil(t, mutationContentFromCall(ToolCall{Name: "grep", Args: map[string]any{}}))
	})
}
