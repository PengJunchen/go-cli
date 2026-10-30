//go:build darwin || linux

package tools

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBashPerCallTimeoutActualPreparedPaths(t *testing.T) {
	for _, mode := range []string{"ordinary", "stream-execute", "stream-sink"} {
		t.Run(mode, func(t *testing.T) {
			parent, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			options := []BashToolOption{WithTimeout(5 * time.Second), WithBashSandbox(AllowAllSandbox{}), WithBashWorkdir(t.TempDir())}
			var definition ToolDefinition = NewBashTool(options...)
			if mode != "ordinary" {
				definition = NewStreamingBashTool(options...)
			}
			base := NewDefaultToolRegistry().(*DefaultToolRegistry)
			require.NoError(t, base.registerTrustedLocalProcess(parent, definition))
			registry := NewMiddlewareToolRegistry(base)
			configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
			call := ToolCall{ID: "per-call-deadline", Name: "bash", Args: map[string]any{"command": "exec sleep 10", "timeout_ms": 2000}}
			var invocation PreparedInvocation
			var result *ToolResult
			var dispatchErr error
			if mode == "stream-sink" {
				sink, err := AttestSynchronousStreamSink(&mockStreamSink{})
				require.NoError(t, err)
				prepared, err := registry.PrepareStreamingToolCall(parent, call, sink)
				require.NoError(t, err)
				invocation = prepared
				result, dispatchErr = prepared.DispatchStreaming(parent, sink)
			} else {
				prepared, err := registry.PrepareToolCall(parent, call)
				require.NoError(t, err)
				invocation = prepared
				result, dispatchErr = prepared.Dispatch(parent)
			}
			require.NoError(t, parent.Err())
			require.NotNil(t, result)
			require.ErrorIs(t, dispatchErr, context.DeadlineExceeded)
			require.Contains(t, dispatchErr.Error(), "timed out after 2s")
			require.Equal(t, CompletionOwnedProcessGroupJoined, invocation.CompletionSignal().Proof(), "this case requires actual managed startup and joined resources; no marker-before-deadline assumption")
			require.True(t, PreparedBashOwnTimeoutKnown(invocation, dispatchErr))
			other, err := registry.PrepareToolCall(parent, ToolCall{ID: "other-stock-action", Name: "bash", Args: map[string]any{"command": "true", "timeout_ms": 2000}})
			require.NoError(t, err)
			_, err = other.Dispatch(parent)
			require.NoError(t, err)
			require.Equal(t, CompletionOwnedProcessGroupJoined, other.CompletionSignal().Proof())
			require.False(t, PreparedBashOwnTimeoutKnown(other, dispatchErr), "a different completed stock action cannot borrow this per-call timeout proof")
			require.False(t, PreparedBashOwnTimeoutKnown(invocation, newUnknownExecutionError(dispatchErr)))
			require.False(t, PreparedBashOwnTimeoutKnown(invocation, errors.Join(dispatchErr, errors.New("foreign outcome"))))
			var pending PendingExecutionError
			require.False(t, errors.As(dispatchErr, &pending))
		})
	}
}

func TestBashPerCallTimeoutSharedInstanceDoesNotMutateHost(t *testing.T) {
	tool := NewStreamingBashTool(WithTimeout(3*time.Second), WithBashSandbox(AllowAllSandbox{}), WithBashWorkdir(t.TempDir()))
	results := make(chan error, 2)
	go func() {
		_, err := tool.Execute(t.Context(), ToolCall{ID: "short", Name: "bash", Args: map[string]any{"command": "exec sleep 1.2", "timeout_ms": 100}})
		results <- fmt.Errorf("short: %w", err)
	}()
	go func() {
		_, err := tool.Execute(t.Context(), ToolCall{ID: "omitted", Name: "bash", Args: map[string]any{"command": "exec sleep 1.2"}})
		results <- err
	}()
	first, second := <-results, <-results
	if first == nil {
		first, second = second, first
	}
	require.ErrorIs(t, first, context.DeadlineExceeded)
	require.NoError(t, second)
	require.Equal(t, 3*time.Second, tool.Timeout)
	require.False(t, tool.TimeoutTier)
}

func TestBashPerCallTimeoutDoesNotRecoverRemainingChildren(t *testing.T) {
	parent, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	require.NoError(t, RegisterDefaults(parent, base, WithRegisteredBuiltinWhitelist([]string{"bash"}), WithRegisteredBashSandbox(AllowAllSandbox{}), WithRegisteredBashTimeout(3*time.Second)))
	registry := NewMiddlewareToolRegistry(base)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	invocation, err := registry.PrepareToolCall(parent, ToolCall{ID: "remaining-child", Name: "bash", Args: map[string]any{"command": "sleep 60 >/dev/null 2>&1 & exit 124", "timeout_ms": 2000}})
	require.NoError(t, err)
	_, err = invocation.Dispatch(parent)
	require.ErrorContains(t, err, "child processes remained")
	var unknown interface{ OutcomeUnknown() bool }
	require.ErrorAs(t, err, &unknown)
	require.True(t, unknown.OutcomeUnknown())
	require.False(t, PreparedBashOwnTimeoutKnown(invocation, err))
	require.NoError(t, parent.Err())
}

func TestBashPerCallTimeoutRuntimeInputStillUnknown(t *testing.T) {
	parent, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ctx := WithBashRuntimeInput(parent, "", nil)
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	require.NoError(t, RegisterDefaults(ctx, base, WithRegisteredBuiltinWhitelist([]string{"bash"}), WithRegisteredBashSandbox(AllowAllSandbox{}), WithRegisteredBashTimeout(3*time.Second)))
	registry := NewMiddlewareToolRegistry(base)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{ID: "runtime-owned", Name: "bash", Args: map[string]any{"command": "exec sleep 10", "timeout_ms": 1000}})
	require.NoError(t, err)
	_, err = invocation.Dispatch(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.False(t, PreparedBashOwnTimeoutKnown(invocation, err))
	require.NoError(t, parent.Err())
	var unknown interface{ OutcomeUnknown() bool }
	require.ErrorAs(t, err, &unknown)
	require.True(t, unknown.OutcomeUnknown())
}
