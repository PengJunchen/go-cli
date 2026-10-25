package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type opaqueCompletionInvocation struct{ PreparedInvocation }

func TestPreparedCompletionPreflightUsesSealedFinalMode(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []PreparedCompletionProof{CompletionInlinePipelineJoined, CompletionQueuedJobJoined, CompletionOwnedProcessGroupJoined, completionScopedSearchJoined, completionLocalProcessOrPrestartNoProcessJoined, "", "invalid-mode"} {
		t.Run(string(mode), func(t *testing.T) {
			fence := NewDispatchFence()
			invocation := newPreparedInvocation(preparedInvocationConfig{fence: fence, generations: fence.Snapshot(), completionProof: mode, execute: func(context.Context, ToolCall) (*ToolResult, error) {
				t.Fatal("preflight must never dispatch")
				return nil, nil
			}})
			if mode == CompletionQueuedJobJoined {
				invocation.schedule = func(context.Context, *preparedInvocation) (*ToolResult, error) {
					t.Fatal("preflight must never enqueue")
					return nil, nil
				}
			}
			supported, err := PreparedCompletionSupported(ctx, invocation, nil)
			require.NoError(t, err)
			require.Equal(t, mode != "" && mode != "invalid-mode", supported)
			require.Nil(t, invocation.CompletionSignal().Done(), "support cannot be inferred from a pre-dispatch Done channel")
			require.Empty(t, invocation.CompletionSignal().Proof())
			require.False(t, invocation.DispatchCommitted())
			unsupported, err := PreparedCompletionSupported(ctx, opaqueCompletionInvocation{invocation}, nil)
			require.NoError(t, err)
			require.False(t, unsupported, "embedding a sealed invocation does not confer support")
			if mode == CompletionQueuedJobJoined {
				invocation.schedule = nil
				supported, err = PreparedCompletionSupported(ctx, invocation, nil)
				require.NoError(t, err)
				require.False(t, supported, "queued proof requires the captured scheduling path")
			}
		})
	}
	for _, invocation := range []PreparedInvocation{nil, (*preparedInvocation)(nil), (*preparedStreamingInvocation)(nil)} {
		supported, err := PreparedCompletionSupported(ctx, invocation, nil)
		require.NoError(t, err)
		require.False(t, supported)
	}
}

func TestPreparedCompletionPreflightChecksCapturedSinkAndAuthority(t *testing.T) {
	ctx := context.Background()
	sink, err := AttestSynchronousStreamSink(&preparedCaptureSink{})
	require.NoError(t, err)
	other, err := AttestSynchronousStreamSink(&preparedCaptureSink{})
	require.NoError(t, err)
	fence := NewDispatchFence()
	config := preparedInvocationConfig{fence: fence, generations: fence.Snapshot(), completionProof: completionLocalProcessOrPrestartNoProcessJoined, execute: func(context.Context, ToolCall) (*ToolResult, error) {
		t.Fatal("preflight must not run an executor")
		return nil, nil
	}}
	stream := func(context.Context, ToolCall, StreamSink) (*ToolResult, error) {
		t.Fatal("preflight must not stream")
		return nil, nil
	}
	plain := newPreparedStreamingInvocation(config, stream)
	supported, err := PreparedCompletionSupported(ctx, plain, nil)
	require.NoError(t, err)
	require.True(t, supported, "ordinary Dispatch of prepared Bash remains valid")
	supported, err = PreparedCompletionSupported(ctx, plain, sink)
	require.NoError(t, err)
	require.False(t, supported, "a preparer that ignored the supplied sink cannot gain streaming support")
	config.streamingSink = sink.(*synchronousStreamSink)
	bound := newPreparedStreamingInvocation(config, stream)
	supported, err = PreparedCompletionSupported(ctx, bound, sink)
	require.NoError(t, err)
	require.True(t, supported)
	supported, err = PreparedCompletionSupported(ctx, bound, other)
	require.NoError(t, err)
	require.False(t, supported, "a different host-attested sink is not the captured sink")
	config.completionProof = CompletionQueuedJobJoined
	config.schedule = func(context.Context, *preparedInvocation) (*ToolResult, error) {
		t.Fatal("preflight must not schedule")
		return nil, nil
	}
	queued := newPreparedStreamingInvocation(config, stream)
	supported, err = PreparedCompletionSupported(ctx, queued, sink)
	require.NoError(t, err)
	require.False(t, supported, "streaming dispatch does not run the captured mutation scheduler")
	_, err = fence.Publish(DispatchFenceApproval, nil, func() {})
	require.NoError(t, err)
	_, err = PreparedCompletionSupported(ctx, plain, sink)
	require.ErrorIs(t, err, ErrDispatchFenceChanged, "a stale fence takes precedence over unsupported streaming")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = PreparedCompletionSupported(canceled, plain, sink)
	require.ErrorIs(t, err, context.Canceled, "caller cancellation takes precedence over a stale fence")
	plain.projectTrustBinding = &projectTrustBindingStub{root: t.TempDir(), generation: 1, valid: false}
	_, err = PreparedCompletionSupported(ctx, plain, sink)
	require.ErrorIs(t, err, errProjectTrustBindingRevoked, "revoked project authority takes precedence over the fence and unsupported sink")
	plain.projectTrustBinding = nil
	plain.consumed.Store(true)
	_, err = PreparedCompletionSupported(ctx, plain, nil)
	require.ErrorIs(t, err, ErrPreparedInvocationConsumed)
}

func TestPreparedCompletionPreflightSeesWrapperSuppressionAndOrdinaryNames(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	registry := NewMiddlewareToolRegistry(base)
	tool := newMCPGenerationTestTool("read")
	wrapper := ToolExecutorWrapper(func(next func(context.Context, ToolCall) (*ToolResult, error)) func(context.Context, ToolCall) (*ToolResult, error) {
		return next
	})
	for _, synchronous := range []bool{false, true} {
		configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{ExecutionWrappers: []ToolExecutorWrapper{wrapper}, SynchronousExecutionWrappers: synchronous})
		require.NoError(t, registry.RegisterTrustedInlineControl(ctx, tool))
		invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "read"})
		require.NoError(t, err)
		supported, err := PreparedCompletionSupported(ctx, invocation, nil)
		require.NoError(t, err)
		require.Equal(t, synchronous, supported)
	}
	require.NoError(t, registry.Register(ctx, tool))
	ordinary, err := registry.PrepareToolCall(ctx, ToolCall{Name: "read"})
	require.NoError(t, err)
	supported, err := PreparedCompletionSupported(ctx, ordinary, nil)
	require.NoError(t, err)
	require.False(t, supported, "a familiar tool name cannot add completion support")
}
