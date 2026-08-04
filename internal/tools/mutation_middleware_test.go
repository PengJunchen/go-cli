package tools_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// TestMutationWrapper_SerializesConcurrentWrites verifies that concurrent
// write calls to the same file path execute sequentially (not in parallel).
func TestMutationWrapper_SerializesConcurrentWrites(t *testing.T) {
	var concurrentCount int32
	var maxConcurrent int32

	// slowTool simulates a tool that takes 50ms, recording concurrent invocations.
	slowTool := &mockMutationTool{
		name: "write",
		execute: func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
			cur := atomic.AddInt32(&concurrentCount, 1)
			if cur > atomic.LoadInt32(&maxConcurrent) {
				atomic.StoreInt32(&maxConcurrent, cur)
			}
			time.Sleep(50 * time.Millisecond)
			atomic.AddInt32(&concurrentCount, -1)
			return &tools.ToolResult{Output: "ok"}, nil
		},
	}

	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, tr.Register(context.Background(), slowTool))

	wrapped := tools.NewMiddlewareToolRegistry(tr, tools.NewMutationWrapper())

	def, err := wrapped.Get(context.Background(), "write")
	require.NoError(t, err)

	// Launch 3 concurrent writes to the same file.
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = def.Execute(context.Background(), tools.ToolCall{
				ID:   "1",
				Name: "write",
				Args: map[string]any{"path": "/tmp/test-mutation-serial.txt", "content": "x"},
			})
		}()
	}
	wg.Wait()

	// All writes to the same file must be serialized: max concurrent == 1.
	assert.Equal(t, int32(1), atomic.LoadInt32(&maxConcurrent),
		"concurrent writes to the same file must be serialized")
}

// TestMutationWrapper_DifferentFilesParallel verifies that writes to different
// files can run in parallel.
func TestMutationWrapper_DifferentFilesParallel(t *testing.T) {
	var concurrentCount int32
	var maxConcurrent int32

	slowTool := &mockMutationTool{
		name: "write",
		execute: func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
			cur := atomic.AddInt32(&concurrentCount, 1)
			if cur > atomic.LoadInt32(&maxConcurrent) {
				atomic.StoreInt32(&maxConcurrent, cur)
			}
			time.Sleep(50 * time.Millisecond)
			atomic.AddInt32(&concurrentCount, -1)
			return &tools.ToolResult{Output: "ok"}, nil
		},
	}

	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, tr.Register(context.Background(), slowTool))

	wrapped := tools.NewMiddlewareToolRegistry(tr, tools.NewMutationWrapper())

	def, err := wrapped.Get(context.Background(), "write")
	require.NoError(t, err)

	// Launch 3 concurrent writes to different files.
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _ = def.Execute(context.Background(), tools.ToolCall{
				ID:   "1",
				Name: "write",
				Args: map[string]any{"path": "/tmp/test-mutation-diff-" + string(rune('a'+idx)) + ".txt", "content": "x"},
			})
		}(i)
	}
	wg.Wait()

	// Different files should be able to run in parallel: max concurrent >= 2.
	assert.GreaterOrEqual(t, atomic.LoadInt32(&maxConcurrent), int32(2),
		"writes to different files should run in parallel")
}

// TestMutationWrapper_NonMutationPassthrough verifies that non-mutation tools
// (e.g. "read") pass through without locking.
func TestMutationWrapper_NonMutationPassthrough(t *testing.T) {
	called := false
	readTool := &mockMutationTool{
		name: "read",
		execute: func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
			called = true
			return &tools.ToolResult{Output: "content"}, nil
		},
	}

	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, tr.Register(context.Background(), readTool))

	wrapped := tools.NewMiddlewareToolRegistry(tr, tools.NewMutationWrapper())

	def, err := wrapped.Get(context.Background(), "read")
	require.NoError(t, err)

	res, err := def.Execute(context.Background(), tools.ToolCall{ID: "1", Name: "read"})
	require.NoError(t, err)
	assert.Equal(t, "content", res.Output)
	assert.True(t, called, "non-mutation tool should execute normally")
}

// TestMutationWrapper_EditAlsoSerialized verifies that "edit" tool calls are
// also serialized by file path.
func TestMutationWrapper_EditAlsoSerialized(t *testing.T) {
	var concurrentCount int32
	var maxConcurrent int32

	slowTool := &mockMutationTool{
		name: "edit",
		execute: func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
			cur := atomic.AddInt32(&concurrentCount, 1)
			if cur > atomic.LoadInt32(&maxConcurrent) {
				atomic.StoreInt32(&maxConcurrent, cur)
			}
			time.Sleep(50 * time.Millisecond)
			atomic.AddInt32(&concurrentCount, -1)
			return &tools.ToolResult{Output: "ok"}, nil
		},
	}

	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, tr.Register(context.Background(), slowTool))

	wrapped := tools.NewMiddlewareToolRegistry(tr, tools.NewMutationWrapper())

	def, err := wrapped.Get(context.Background(), "edit")
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = def.Execute(context.Background(), tools.ToolCall{
				ID:   "1",
				Name: "edit",
				Args: map[string]any{"file_path": "/tmp/test-mutation-edit.txt", "old_string": "a", "new_string": "b"},
			})
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), atomic.LoadInt32(&maxConcurrent),
		"concurrent edits to the same file must be serialized")
}

// mockMutationTool is a test ToolDefinition with a configurable Execute function.
type mockMutationTool struct {
	name    string
	execute func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error)
}

func (m *mockMutationTool) Name() string        { return m.name }
func (m *mockMutationTool) Description() string { return "mock " + m.name }
func (m *mockMutationTool) Execute(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	return m.execute(ctx, call)
}
