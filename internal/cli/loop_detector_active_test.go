package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type loopDetectorCaptureTool struct{ name string }

func (tool *loopDetectorCaptureTool) Name() string        { return tool.name }
func (tool *loopDetectorCaptureTool) Description() string { return "loop detector test tool" }
func (tool *loopDetectorCaptureTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	return &tools.ToolResult{Output: "ok"}, nil
}

func captureLoopDetectorAttempts(t *testing.T) []tools.FrozenPreparedAttempt {
	t.Helper()
	ctx := context.Background()
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, base.Register(ctx, &loopDetectorCaptureTool{name: "capture"}))
	require.NoError(t, base.Register(ctx, &loopDetectorCaptureTool{name: "other"}))
	registry := tools.NewMiddlewareToolRegistry(tools.NewDeferredToolRegistryAdapter(base))
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	var attempts []tools.FrozenPreparedAttempt
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{
		PreparedAttemptObserver: func(_ context.Context, attempt tools.FrozenPreparedAttempt) error {
			attempts = append(attempts, attempt)
			return nil
		},
	}))
	for _, call := range []tools.ToolCall{
		{ID: "display-1", Name: "capture", Args: map[string]any{"value": "same"}},
		{ID: "display-2", Name: "capture", Args: map[string]any{"value": "same"}},
		{ID: "display-3", Name: "capture", Args: map[string]any{"value": "different"}},
		{ID: "display-4", Name: "other", Args: map[string]any{"value": "same"}},
	} {
		invocation, err := registry.PrepareToolCall(ctx, call)
		require.NoError(t, err)
		require.NotNil(t, invocation)
	}
	return attempts
}

func TestLoopDetectorAttemptKeyUsesIdentityAndFrozenDigestButNotCallID(t *testing.T) {
	attempts := captureLoopDetectorAttempts(t)
	require.Len(t, attempts, 4)
	require.Equal(t, loopDetectorAttemptKey(attempts[0]), loopDetectorAttemptKey(attempts[1]))
	require.NotEqual(t, loopDetectorAttemptKey(attempts[0]), loopDetectorAttemptKey(attempts[2]), "different frozen arguments reset the sequence")
	require.NotEqual(t, loopDetectorAttemptKey(attempts[0]), loopDetectorAttemptKey(attempts[3]), "different registration identities reset the sequence")
}

func TestLoopDetectorRunStateResetsConsecutiveKeysAndIsRunScoped(t *testing.T) {
	state := newLoopDetectorRunState(production.LoopDetectionConfig{SameToolCallThreshold: 3}, nil)
	var first, different [32]byte
	first[0] = 1
	different[0] = 2
	for range 2 {
		_, detected := state.observe(first)
		require.False(t, detected)
	}
	_, detected := state.observe(different)
	require.False(t, detected)
	_, detected = state.observe(first)
	require.False(t, detected, "a different attempt resets the consecutive count")
	_, detected = state.observe(first)
	require.False(t, detected)
	result, detected := state.observe(first)
	require.True(t, detected)
	require.Equal(t, 3, result.Count)
	require.Equal(t, production.DispositionWarn, result.Disposition)

	newRun := newLoopDetectorRunState(production.LoopDetectionConfig{SameToolCallThreshold: 3}, nil)
	_, detected = newRun.observe(first)
	require.False(t, detected, "a prior Run cannot poison a fresh Run")
}

func TestLoopDetectorFirstCauseWinsAndReturnsTypedTermination(t *testing.T) {
	attempt := captureLoopDetectorAttempts(t)[0]
	config := production.LoopDetectionConfig{SameToolCallThreshold: 1, Disposition: production.DispositionTerminate}

	t.Run("caller cancellation first", func(t *testing.T) {
		parent, cancelParent := context.WithCancelCause(context.Background())
		defer cancelParent(nil)
		runCtx, cancelRun, _ := newLoopDetectorRunContext(parent, config)
		defer cancelRun(nil)
		cancelParent(context.Canceled)

		err := observePreparedToolAttempt(runCtx, attempt)
		require.ErrorIs(t, err, context.Canceled)
		require.NotErrorIs(t, err, core.ErrLoopDetected)
		require.ErrorIs(t, context.Cause(runCtx), context.Canceled)
	})

	t.Run("detector cause first", func(t *testing.T) {
		parent, cancelParent := context.WithCancelCause(context.Background())
		defer cancelParent(nil)
		runCtx, cancelRun, _ := newLoopDetectorRunContext(parent, config)
		defer cancelRun(nil)

		err := observePreparedToolAttempt(runCtx, attempt)
		require.ErrorIs(t, err, core.ErrLoopDetected)
		cancelParent(context.Canceled)
		require.ErrorIs(t, context.Cause(runCtx), core.ErrLoopDetected)
		publicErr := normalizeLoopDetectorRunError(runCtx, errors.Join(context.Canceled, err))
		require.ErrorIs(t, publicErr, core.ErrLoopDetected)
		require.NotErrorIs(t, publicErr, context.Canceled)
	})
}
