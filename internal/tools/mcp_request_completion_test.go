package tools

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type requestUnknownTestError struct{}

func (requestUnknownTestError) Error() string        { return "request may have executed" }
func (requestUnknownTestError) OutcomeUnknown() bool { return true }

func TestPreparedInlineCompletionDoesNotResolveUnknownOutcome(t *testing.T) {
	for _, callErr := range []error{requestUnknownTestError{}, errors.Join(errors.New("wrapped"), requestUnknownTestError{})} {
		fence := NewDispatchFence()
		call, err := freezeToolCall(ToolCall{Name: "owned-child", Args: map[string]any{}})
		require.NoError(t, err)
		invocation := newPreparedInvocation(preparedInvocationConfig{
			fence: fence, generations: fence.Snapshot(), call: call,
			completionProof: CompletionInlinePipelineJoined,
			execute:         func(context.Context, ToolCall) (*ToolResult, error) { return nil, callErr },
		})
		_, err = invocation.Dispatch(context.Background())
		require.ErrorIs(t, err, callErr)
		require.NotNil(t, invocation.CompletionSignal().Done())
		require.Empty(t, invocation.CompletionSignal().Proof(), "a returned unknown outcome does not prove resource containment")
	}
}

type pendingMCPDiscoveryController struct{ done <-chan struct{} }

func (c pendingMCPDiscoveryController) DiscoverMCP(context.Context, string, string) (string, <-chan struct{}, error) {
	return "[]", c.done, nil
}

func TestMCPDiscoveryRequiresActionScopedJoinSignal(t *testing.T) {
	for _, signal := range []<-chan struct{}{nil, make(chan struct{}), closedCompletionChannel()} {
		registry := NewMiddlewareToolRegistry(NewDefaultToolRegistry())
		configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
		require.NoError(t, registry.Register(context.Background(), NewOwnedMCPDiscoveryTool(pendingMCPDiscoveryController{done: signal})))
		invocation, err := registry.PrepareToolCall(context.Background(), ToolCall{Name: "mcp_discover", Args: map[string]any{}})
		require.NoError(t, err)
		supported, err := PreparedCompletionSupported(context.Background(), invocation, nil)
		require.NoError(t, err)
		require.True(t, supported)
		_, err = invocation.Dispatch(context.Background())
		joined := false
		if signal != nil {
			select {
			case <-signal:
				joined = true
			default:
			}
		}
		if joined {
			require.NoError(t, err)
			require.Equal(t, CompletionMCPRequestJoined, invocation.CompletionSignal().Proof())
		} else {
			require.Error(t, err)
			require.Empty(t, invocation.CompletionSignal().Proof())
		}
		require.Equal(t, []TrustedToolEffect{EffectUnknown}, invocation.EffectSet().Values())
	}
}

func TestPreparedMCPRequestKeepsGenerationLeaseUntilPendingResourcesJoin(t *testing.T) {
	lease := &mcpDispatchLeaseProbe{}
	resourceDone := make(chan struct{})
	call, err := freezeToolCall(ToolCall{Name: "mcp__srv__pending", Args: map[string]any{}})
	require.NoError(t, err)
	fence := NewDispatchFence()
	invocation := newPreparedInvocation(preparedInvocationConfig{
		fence: fence, generations: fence.Snapshot(), call: call, dispatchLease: lease,
		completionProof: CompletionMCPRequestJoined,
		execute: func(context.Context, ToolCall) (*ToolResult, error) {
			return nil, &pendingExecutionErrorStub{cause: context.Canceled, done: resourceDone, unknown: true}
		},
	})
	_, err = invocation.Dispatch(context.Background())
	require.Error(t, err)
	require.Zero(t, lease.releaseCount.Load(), "returning a pending error does not release its frozen client generation")
	select {
	case <-invocation.CompletionSignal().Done():
		t.Fatal("pending request join signal closed early")
	default:
	}
	close(resourceDone)
	require.Eventually(t, func() bool { return lease.releaseCount.Load() == 1 }, time.Second, time.Millisecond)
	require.Empty(t, invocation.CompletionSignal().Proof(), "joining local resources does not turn unknown outcome into positive proof")
}

type selfDeclaredMCPRequestClient struct{ mcpGenerationTestExecutor }

func (selfDeclaredMCPRequestClient) MCPRequestCompletionSupported() bool { return true }

func TestGenericMCPHandleCannotSelfDeclareCompletion(t *testing.T) {
	calls := &atomic.Int32{}
	client := selfDeclaredMCPRequestClient{mcpGenerationTestExecutor{output: "unreviewed", calls: calls}}
	candidate := newMCPDispatchLeaseCandidate(t, "mcp__self_declared__tool", client, mcpGenerationTestLease{})
	base := NewDeferredToolRegistryAdapter(NewDefaultToolRegistry())
	update, err := base.BeginMCPGenerationUpdate(context.Background(), "self-declared", 1, []MCPToolCandidate{candidate})
	require.NoError(t, err)
	require.NoError(t, update.PublishMaterialized())
	registry := NewMiddlewareToolRegistry(base)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	invocation, err := registry.PrepareToolCall(context.Background(), ToolCall{Name: "mcp__self_declared__tool", Args: map[string]any{}})
	require.NoError(t, err)
	supported, err := PreparedCompletionSupported(context.Background(), invocation, nil)
	require.NoError(t, err)
	require.False(t, supported, "a bool method cannot upgrade a generic registration")
	require.Zero(t, calls.Load())
}

func TestGenericMCPDiscoveryCannotSelfDeclareCompletion(t *testing.T) {
	registry := NewMiddlewareToolRegistry(NewDefaultToolRegistry())
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	require.NoError(t, registry.Register(context.Background(), NewMCPDiscoveryTool(pendingMCPDiscoveryController{done: closedCompletionChannel()})))
	invocation, err := registry.PrepareToolCall(context.Background(), ToolCall{Name: "mcp_discover", Args: map[string]any{}})
	require.NoError(t, err)
	supported, err := PreparedCompletionSupported(context.Background(), invocation, nil)
	require.NoError(t, err)
	require.False(t, supported, "a controller returning a closed channel cannot upgrade generic registration")
}

type overridingOwnedDiscovery struct{ *MCPDiscoveryTool }

func (*overridingOwnedDiscovery) Execute(context.Context, ToolCall) (*ToolResult, error) {
	return &ToolResult{Output: "counterfeit"}, nil
}

func TestGenericRegistrationEmbeddingOwnedDiscoveryCannotKeepProof(t *testing.T) {
	registry := NewMiddlewareToolRegistry(NewDefaultToolRegistry())
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	owned := NewOwnedMCPDiscoveryTool(pendingMCPDiscoveryController{done: closedCompletionChannel()})
	require.NoError(t, registry.Register(context.Background(), &overridingOwnedDiscovery{MCPDiscoveryTool: owned}))
	invocation, err := registry.PrepareToolCall(context.Background(), ToolCall{Name: "mcp_discover", Args: map[string]any{}})
	require.NoError(t, err)
	supported, err := PreparedCompletionSupported(context.Background(), invocation, nil)
	require.NoError(t, err)
	require.False(t, supported)
}
