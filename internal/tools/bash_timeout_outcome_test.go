//go:build darwin || linux

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPreparedBashOwnTimeoutAuthorityIsCurrentAndRevocable(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%t", streaming), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
			require.NoError(t, RegisterDefaults(ctx, base, WithRegisteredBuiltinWhitelist([]string{"bash"}), WithRegisteredBashSandbox(AllowAllSandbox{}), WithRegisteredBashTimeout(300*time.Millisecond)))
			registry := NewMiddlewareToolRegistry(base)
			configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
			call := ToolCall{ID: "post-hook-looking-model-call", Name: "bash", Args: map[string]any{"command": "printf bound-timeout-sentinel; exec sleep 5"}}
			var invocation PreparedInvocation
			var result *ToolResult
			var dispatchErr error
			if streaming {
				sink, err := AttestSynchronousStreamSink(&mockStreamSink{})
				require.NoError(t, err)
				prepared, err := registry.PrepareStreamingToolCall(ctx, call, sink)
				require.NoError(t, err)
				invocation = prepared
				result, dispatchErr = prepared.DispatchStreaming(ctx, sink)
			} else {
				prepared, err := registry.PrepareToolCall(ctx, call)
				require.NoError(t, err)
				invocation = prepared
				result, dispatchErr = prepared.Dispatch(ctx)
			}
			require.NoError(t, ctx.Err())
			require.NotNil(t, result)
			require.Contains(t, result.Output, "bound-timeout-sentinel", "real command startup is a separate precondition")
			require.ErrorIs(t, dispatchErr, context.DeadlineExceeded)
			require.Equal(t, CompletionOwnedProcessGroupJoined, invocation.CompletionSignal().Proof())
			require.True(t, PreparedBashOwnTimeoutKnown(invocation, dispatchErr))
			require.True(t, IsPreparedBashOwnTimeout(dispatchErr))
			other, err := registry.PrepareToolCall(ctx, ToolCall{ID: "other-action", Name: "bash", Args: map[string]any{"command": "printf must-not-run"}})
			require.NoError(t, err)
			_, otherErr := other.Dispatch(ctx)
			require.NoError(t, otherErr)
			require.True(t, other.DispatchCommitted())
			require.Equal(t, CompletionOwnedProcessGroupJoined, other.CompletionSignal().Proof())
			require.False(t, PreparedBashOwnTimeoutKnown(other, dispatchErr), "a genuine earlier error is not authority even for another completed stock action with its own valid proof")
			require.False(t, PreparedBashOwnTimeoutKnown(invocation, context.DeadlineExceeded))
			require.False(t, PreparedBashOwnTimeoutKnown(invocation, newUnknownExecutionError(dispatchErr)), "an unknown wrapper must not inherit inner known authority")
			require.False(t, PreparedBashOwnTimeoutKnown(invocation, errors.Join(dispatchErr, errors.New("another outcome"))), "a joined additional error cannot inherit authority")
			var captured *preparedInvocation
			switch p := invocation.(type) {
			case *preparedInvocation:
				captured = p
			case *preparedStreamingInvocation:
				captured = p.preparedInvocation
			default:
				t.Fatal("unexpected prepared capability")
			}
			captured.invalidateCompletionProof()
			require.False(t, PreparedBashOwnTimeoutKnown(invocation, dispatchErr), "revoked actual proof must not remain usable")
		})
	}
}

func TestPreparedBashOwnTimeoutLifecycleGuardsStayUnknown(t *testing.T) {
	parent := context.Background()
	own, cancel := context.WithCancel(parent)
	cancel()
	current := &preparedInvocation{identity: newTrustedToolIdentity(RegistrationSourceBuiltin, "bash", 1, [32]byte{1}, TrustedToolScope{}), completionProof: completionLocalProcessOrPrestartNoProcessJoined}
	current.committed.Store(true)
	parent = withPreparedBashInvocation(parent, current)
	good := managedLocalResult{containmentProven: true, quiescent: true, readersJoined: true}
	// A canceled exec context is never an own DeadlineExceeded even if joined.
	err := bashLifecycleError(parent, own, context.Canceled, good)
	require.False(t, IsPreparedBashOwnTimeout(err))
	deadline, finish := context.WithDeadline(parent, time.Now().Add(-time.Second))
	defer finish()
	for _, change := range []struct {
		name   string
		mutate func(*managedLocalResult)
	}{
		{"unproved containment", func(r *managedLocalResult) { r.containmentProven = false }},
		{"pending group", func(r *managedLocalResult) { r.quiescent = false }},
		{"readers not joined", func(r *managedLocalResult) { r.readersJoined = false }},
		{"forced pipe close", func(r *managedLocalResult) { r.forcedPipeClose = true }},
		{"reader failure", func(r *managedLocalResult) { r.readerErr = errors.New("synthetic reader failure") }},
		{"cleanup failure", func(r *managedLocalResult) { r.cleanupErr = errors.New("synthetic cleanup failure") }},
	} {
		t.Run(change.name, func(t *testing.T) {
			observed := good
			change.mutate(&observed)
			err := bashLifecycleError(parent, deadline, context.DeadlineExceeded, observed)
			require.False(t, IsPreparedBashOwnTimeout(err))
		})
	}
	deadParent, stop := context.WithCancel(parent)
	stop()
	err = bashLifecycleError(deadParent, deadline, context.DeadlineExceeded, good)
	require.False(t, IsPreparedBashOwnTimeout(err), "caller cancellation never becomes recoverable own-budget failure")
	err = bashLifecycleError(context.Background(), deadline, context.DeadlineExceeded, good)
	require.False(t, IsPreparedBashOwnTimeout(err), "unprepared local join is not sealed authority")
}

func TestPreparedBashParentCancellationIsNotRecoverableTimeout(t *testing.T) {
	testPreparedBashParentCancellation(t, false)
}

func TestPreparedBashPerCallParentCancellationIsNotRecoverableTimeout(t *testing.T) {
	testPreparedBashParentCancellation(t, true)
}

func testPreparedBashParentCancellation(t *testing.T, perCall bool) {
	root := t.TempDir()
	pidPath := filepath.Join(root, "started.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, RegisterDefaults(ctx, base, WithRegisteredBuiltinWhitelist([]string{"bash"}), WithRegisteredBashSandbox(AllowAllSandbox{}), WithRegisteredBashTimeout(10*time.Second)))
	registry := NewMiddlewareToolRegistry(base)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	quoted := "'" + strings.ReplaceAll(pidPath, "'", "'\\''") + "'"
	args := map[string]any{"command": "printf '%s' \"$$\" > " + quoted + "; exec sleep 20"}
	if perCall {
		args["timeout_ms"] = 6000
	}
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{ID: "cancel-parent", Name: "bash", Args: args})
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { _, dispatchErr := invocation.Dispatch(ctx); done <- dispatchErr }()
	// Always join the real dispatcher even if startup assertions fail.
	joined := false
	defer func() {
		cancel()
		if !joined {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("prepared cancel dispatcher did not join")
			}
		}
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	started := false
	for !started {
		if value, readErr := os.ReadFile(pidPath); readErr == nil && len(value) > 0 {
			started = true
			break
		}
		select {
		case dispatchErr := <-done:
			joined = true
			t.Fatalf("dispatcher exited before startup: %v", dispatchErr)
		case <-timer.C:
			t.Fatal("synthetic Bash startup not reached")
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case dispatchErr := <-done:
		joined = true
		require.ErrorIs(t, dispatchErr, context.Canceled)
		require.Equal(t, CompletionOwnedProcessGroupJoined, invocation.CompletionSignal().Proof(), "parent cancellation remains non-recoverable even after actual group/readers close")
		require.False(t, IsPreparedBashOwnTimeout(dispatchErr))
		require.False(t, PreparedBashOwnTimeoutKnown(invocation, dispatchErr))
	case <-time.After(5 * time.Second):
		t.Fatal("prepared parent cancellation did not join")
	}
}

func TestBashErrorPartialCaptureKeepsStructuredTruncation(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%t", streaming), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			options := []BashToolOption{WithTimeout(5 * time.Second), WithNoSandbox(), WithMaxOutput(4096)}
			call := ToolCall{ID: "actual-truncated-exit", Name: "bash", Args: map[string]any{"command": "printf 'bounded-partial-header\\n'; printf '%2048s\\n' {1..12}; exit 7"}}
			var result *ToolResult
			var err error
			if streaming {
				result, err = NewStreamingBashTool(options...).ExecuteStreaming(ctx, call, nil)
			} else {
				result, err = NewBashTool(options...).Execute(ctx, call)
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), "exit status 7")
			require.NotNil(t, result)
			require.Contains(t, result.Output, "bounded-partial-header")
			require.Equal(t, true, result.Metadata["truncated"], "actual stock output capture—not a forged result—reports source truncation")
			require.Less(t, len(result.Output), 5000)
			require.NoError(t, ctx.Err())
		})
	}
}

func TestPreparedRuntimeOwnedBashDeadlineRemainsUnknown(t *testing.T) {
	for _, timeoutOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout-only=%t", timeoutOnly), func(t *testing.T) {
			parent, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
			require.NoError(t, RegisterDefaults(parent, base, WithRegisteredBuiltinWhitelist([]string{"bash"}), WithRegisteredBashSandbox(AllowAllSandbox{}), WithRegisteredBashTimeout(time.Second)))
			registry := NewMiddlewareToolRegistry(base)
			configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
			runtimeCtx := WithBashRuntimeInput(parent, "host-owned input", nil)
			if timeoutOnly {
				runtimeCtx = WithBashRuntimeExecutionTimeout(parent, 100*time.Millisecond)
			}
			invocation, err := registry.PrepareToolCall(runtimeCtx, ToolCall{ID: "call_1234567890abcdef", Name: "bash", Args: map[string]any{"command": "printf runtime-budget-started; exec sleep 5"}})
			require.NoError(t, err)
			result, dispatchErr := invocation.Dispatch(runtimeCtx)
			require.NotNil(t, result)
			require.Contains(t, result.Output, "runtime-budget-started")
			require.ErrorIs(t, dispatchErr, context.DeadlineExceeded)
			require.NoError(t, parent.Err())
			require.Equal(t, CompletionOwnedProcessGroupJoined, invocation.CompletionSignal().Proof(), "actual local resource closure does not override the host runtime-hook stop policy")
			require.False(t, IsPreparedBashOwnTimeout(dispatchErr))
			require.False(t, PreparedBashOwnTimeoutKnown(invocation, dispatchErr))
			var unknown interface{ OutcomeUnknown() bool }
			require.True(t, errors.As(dispatchErr, &unknown))
			require.True(t, unknown.OutcomeUnknown())
		})
	}
}
