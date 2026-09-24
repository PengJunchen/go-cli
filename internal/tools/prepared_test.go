package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestDispatchFencePublishRejectsStaleFullSnapshot(t *testing.T) {
	fence := NewDispatchFence()
	initial := fence.Snapshot()

	updated, err := fence.Publish(DispatchFencePlan, nil, func() {})
	require.NoError(t, err)
	assert.Equal(t, initial.Plan+1, updated.Plan)
	assert.Equal(t, initial.Registry, updated.Registry)
	assert.Equal(t, initial.Approval, updated.Approval)

	updatedCalled := false
	_, err = fence.Publish(DispatchFenceApproval, &initial, func() {
		updatedCalled = true
	})
	require.ErrorIs(t, err, ErrDispatchFenceChanged)
	assert.False(t, updatedCalled, "stale state updates must not publish")
	assert.Equal(t, updated, fence.Snapshot())
}

func TestDispatchFenceCommitChecksSnapshotBeforeMark(t *testing.T) {
	fence := NewDispatchFence()
	initial := fence.Snapshot()
	marked := false

	require.NoError(t, fence.commit(initial, func() { marked = true }))
	assert.True(t, marked)

	_, err := fence.Publish(DispatchFenceRegistry, nil, func() {})
	require.NoError(t, err)
	marked = false
	require.ErrorIs(t, fence.commit(initial, func() { marked = true }), ErrDispatchFenceChanged)
	assert.False(t, marked, "a stale commit must not consume a permit")
}

func TestFrozenToolCallDefensivelyCopiesNestedJSONAndDigest(t *testing.T) {
	call := ToolCall{
		ID:   "call-1",
		Name: "write",
		Args: map[string]any{
			"max_depth": float64(2),
			"content": map[string]any{
				"parts": []any{map[string]any{"text": "original"}},
			},
		},
	}

	frozen, err := freezeToolCall(call)
	require.NoError(t, err)
	digest := frozen.Digest()

	call.Args["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"] = "mutated source"
	returned := frozen.ToolCall()
	returned.Args["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"] = "mutated return"

	secondRead := frozen.ToolCall()
	assert.Equal(t, "original", secondRead.Args["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"])
	assert.Equal(t, float64(2), secondRead.Args["max_depth"])
	assert.Equal(t, digest, frozen.Digest())
}

func TestToolSchemaSnapshotDefensivelyCopiesJSON(t *testing.T) {
	schema := map[string]any{
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
		},
		"required": []string{"path"},
	}

	snapshot, err := freezeToolSchema(schema)
	require.NoError(t, err)
	require.True(t, snapshot.Present())
	digest := snapshot.Digest()

	schema["properties"].(map[string]any)["path"].(map[string]any)["type"] = "object"
	returned := snapshot.Schema().(map[string]any)
	returned["properties"].(map[string]any)["path"].(map[string]any)["type"] = "boolean"

	secondRead := snapshot.Schema().(map[string]any)
	assert.Equal(t, "string", secondRead["properties"].(map[string]any)["path"].(map[string]any)["type"])
	assert.IsType(t, []string{}, secondRead["required"], "schema snapshots preserve native JSON slice types")
	assert.Equal(t, digest, snapshot.Digest())
}

func TestTrustedEffectSetReturnsDefensiveCopyAndUnknownDefault(t *testing.T) {
	effects := unknownTrustedEffects()
	assert.True(t, effects.Contains(EffectUnknown))

	values := effects.Values()
	require.Len(t, values, 1)
	values[0] = EffectWorkspaceRead
	assert.True(t, effects.Contains(EffectUnknown), "callers cannot upgrade the captured effect set")
}

func TestTrustedEffectValuesAreStableAndIdentitySnapshotIsJSONSafe(t *testing.T) {
	effects := newTrustedEffectSet(EffectWorkspaceWrite, EffectControl, EffectWorkspaceWrite)
	assert.Equal(t, []TrustedToolEffect{EffectControl, EffectWorkspaceWrite}, effects.Values())

	identity := newTrustedToolIdentity("builtin", "tool", 7, [32]byte{0x01}, newTrustedToolScope("workspace:/tmp/project"))
	snapshot := identity.Snapshot()
	assert.Equal(t, "builtin", snapshot.RegistrationSource)
	assert.Equal(t, "tool", snapshot.ToolName)
	assert.EqualValues(t, 7, snapshot.RegistrationGeneration)
	assert.Equal(t, "0100000000000000000000000000000000000000000000000000000000000000", snapshot.ExecutorFingerprint)
	assert.Len(t, snapshot.ScopeFingerprint, 64)

	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(encoded, &fields))
	assert.Equal(t, "builtin", fields["registration_source"])
	assert.Equal(t, "tool", fields["tool_name"])
	assert.Equal(t, float64(7), fields["registration_generation"])
	assert.Equal(t, snapshot.ExecutorFingerprint, fields["executor_fingerprint"])
	assert.Equal(t, snapshot.ScopeFingerprint, fields["scope_fingerprint"])
}

func TestPreparedInvocationConsumesDirectPermitOnce(t *testing.T) {
	fence := NewDispatchFence()
	var calls atomic.Int32
	invocation := newPreparedInvocation(preparedInvocationConfig{
		fence:       fence,
		generations: fence.Snapshot(),
		call:        mustFreezeToolCall(t, ToolCall{Name: "tool", Args: map[string]any{"value": "captured"}}),
		execute: func(_ context.Context, call ToolCall) (*ToolResult, error) {
			calls.Add(1)
			return &ToolResult{Output: call.Args["value"].(string)}, nil
		},
	})

	result, err := invocation.Dispatch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "captured", result.Output)

	_, err = invocation.Dispatch(context.Background())
	require.ErrorIs(t, err, ErrPreparedInvocationConsumed)
	assert.EqualValues(t, 1, calls.Load())
	assert.True(t, invocation.DispatchCommitted())
}

func TestPreparedStreamingAndDirectDispatchShareOneUsePermit(t *testing.T) {
	fence := NewDispatchFence()
	var directCalls atomic.Int32
	var streamCalls atomic.Int32
	invocation := newPreparedStreamingInvocation(preparedInvocationConfig{
		fence:       fence,
		generations: fence.Snapshot(),
		call:        mustFreezeToolCall(t, ToolCall{Name: "bash", Args: map[string]any{"command": "true"}}),
		execute: func(context.Context, ToolCall) (*ToolResult, error) {
			directCalls.Add(1)
			return &ToolResult{Output: "direct"}, nil
		},
	}, func(_ context.Context, _ ToolCall, sink StreamSink) (*ToolResult, error) {
		streamCalls.Add(1)
		if sink != nil {
			if err := sink.Send("line", "call-1", "stdout"); err != nil {
				return nil, err
			}
		}
		return &ToolResult{Output: "streamed"}, nil
	})

	result, err := invocation.DispatchStreaming(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "streamed", result.Output)

	_, err = invocation.Dispatch(context.Background())
	require.ErrorIs(t, err, ErrPreparedInvocationConsumed)
	assert.EqualValues(t, 0, directCalls.Load())
	assert.EqualValues(t, 1, streamCalls.Load())
	assert.True(t, invocation.DispatchCommitted())
}

func TestPreparedInvocationStaleFenceDoesNotInvokeExecutor(t *testing.T) {
	fence := NewDispatchFence()
	var calls atomic.Int32
	invocation := newPreparedInvocation(preparedInvocationConfig{
		fence:       fence,
		generations: fence.Snapshot(),
		call:        mustFreezeToolCall(t, ToolCall{Name: "write"}),
		execute: func(context.Context, ToolCall) (*ToolResult, error) {
			calls.Add(1)
			return &ToolResult{}, nil
		},
	})

	_, err := fence.Publish(DispatchFenceApproval, nil, func() {})
	require.NoError(t, err)
	_, err = invocation.Dispatch(context.Background())
	require.ErrorIs(t, err, ErrDispatchFenceChanged)
	assert.EqualValues(t, 0, calls.Load())
	assert.False(t, invocation.DispatchCommitted())
	_, err = invocation.Dispatch(context.Background())
	assert.ErrorIs(t, err, ErrPreparedInvocationConsumed, "the failed first attempt still consumes the direct permit")
}

func TestCanceledFirstDispatchConsumesPermitAndCannotRetryWithFreshContext(t *testing.T) {
	fence := NewDispatchFence()
	var calls atomic.Int32
	invocation := newPreparedInvocation(preparedInvocationConfig{
		fence:       fence,
		generations: fence.Snapshot(),
		call:        mustFreezeToolCall(t, ToolCall{Name: "tool"}),
		execute: func(context.Context, ToolCall) (*ToolResult, error) {
			calls.Add(1)
			return &ToolResult{}, nil
		},
	})

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := invocation.Dispatch(canceled)
	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, invocation.DispatchCommitted())

	_, err = invocation.Dispatch(context.Background())
	assert.ErrorIs(t, err, ErrPreparedInvocationConsumed)
	assert.Zero(t, calls.Load())
}

func TestCanceledStreamingFirstAttemptConsumesSharedPermit(t *testing.T) {
	fence := NewDispatchFence()
	var calls atomic.Int32
	invocation := newPreparedStreamingInvocation(preparedInvocationConfig{
		fence:       fence,
		generations: fence.Snapshot(),
		call:        mustFreezeToolCall(t, ToolCall{Name: "stream"}),
	}, func(context.Context, ToolCall, StreamSink) (*ToolResult, error) {
		calls.Add(1)
		return &ToolResult{}, nil
	})

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := invocation.DispatchStreaming(canceled, nil)
	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, invocation.DispatchCommitted())

	_, err = invocation.Dispatch(context.Background())
	assert.ErrorIs(t, err, ErrPreparedInvocationConsumed)
	assert.Zero(t, calls.Load())
}

func TestPreparedInvocationReportsCommitEvenWhenExecutorReturnsFenceSentinel(t *testing.T) {
	fence := NewDispatchFence()
	invocation := newPreparedInvocation(preparedInvocationConfig{
		fence:       fence,
		generations: fence.Snapshot(),
		call:        mustFreezeToolCall(t, ToolCall{Name: "tool"}),
		execute: func(context.Context, ToolCall) (*ToolResult, error) {
			return nil, ErrDispatchFenceChanged
		},
	})

	_, err := invocation.Dispatch(context.Background())
	assert.ErrorIs(t, err, ErrDispatchFenceChanged)
	assert.True(t, invocation.DispatchCommitted(), "the error came from an executor after dispatch committed")
}

func TestPreparedDispatchPreservesFindAndGitNumericArguments(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	findDir := setupFindDir(t)
	findTool := NewFindTool(WithFindWorkdir(findDir), WithFindForceNode(true))
	findFence := NewDispatchFence()
	findInvocation := newPreparedInvocation(preparedInvocationConfig{
		fence:       findFence,
		generations: findFence.Snapshot(),
		call: mustFreezeToolCall(t, ToolCall{
			Name: "find",
			Args: map[string]any{"max_depth": float64(1)},
		}),
		execute: findTool.Execute,
	})
	findResult, err := findInvocation.Dispatch(context.Background())
	require.NoError(t, err)
	assert.Contains(t, findResult.Output, "sub")
	assert.NotContains(t, findResult.Output, "sub/c.go", "float64 max_depth reaches FindTool unchanged")

	git := setupGitRepo(t)
	dir := git.cwd
	writeFileInDir(t, dir, "two-lines.txt", "first line\nsecond line\n")
	runGitInDir(t, dir, "add", "two-lines.txt")
	runGitInDir(t, dir, "commit", "-m", "add two lines")
	gitBlame := NewGitBlameTool(git)
	gitFence := NewDispatchFence()
	gitInvocation := newPreparedInvocation(preparedInvocationConfig{
		fence:       gitFence,
		generations: gitFence.Snapshot(),
		call: mustFreezeToolCall(t, ToolCall{
			Name: "git_blame",
			Args: map[string]any{"file": "two-lines.txt", "start_line": float64(2), "end_line": float64(2)},
		}),
		execute: gitBlame.Execute,
	})
	gitResult, err := gitInvocation.Dispatch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, gitResult.Metadata["lines"])
	assert.Contains(t, gitResult.Output, "second line")
	assert.NotContains(t, gitResult.Output, "first line", "float64 line bounds reach GitBlameTool unchanged")
}

func TestPreparedValueRejectsNonJSONObjects(t *testing.T) {
	_, err := freezeToolCall(ToolCall{Args: map[string]any{"invalid": errors.New("bad")}})
	assert.ErrorIs(t, err, ErrPreparedValueNotJSON)
}

func mustFreezeToolCall(t *testing.T, call ToolCall) FrozenToolCall {
	t.Helper()
	frozen, err := freezeToolCall(call)
	require.NoError(t, err)
	return frozen
}
