package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHostInlineControlProofRequiresExplicitRegistrationThroughWrappers(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	deferred := NewDeferredToolRegistryAdapter(base)
	registry := NewMiddlewareToolRegistry(deferred)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	tool := newMCPGenerationTestTool("skill_load")
	require.NoError(t, registry.RegisterTrustedControl(ctx, tool))
	ordinary, err := registry.PrepareToolCall(ctx, ToolCall{ID: "ordinary", Name: tool.Name(), Args: map[string]any{}})
	require.NoError(t, err)
	_, err = ordinary.Dispatch(ctx)
	require.NoError(t, err)
	require.Empty(t, ordinary.CompletionSignal().Proof(), "a familiar name must not confer a completion proof")
	require.NoError(t, registry.RegisterTrustedInlineControl(ctx, tool))
	inline, err := registry.PrepareToolCall(ctx, ToolCall{ID: "inline", Name: tool.Name(), Args: map[string]any{}})
	require.NoError(t, err)
	_, err = inline.Dispatch(ctx)
	require.NoError(t, err)
	require.Equal(t, CompletionInlinePipelineJoined, inline.CompletionSignal().Proof())
	select {
	case <-inline.CompletionSignal().Done():
	default:
		t.Fatal("inline completion must be joined before dispatch returns")
	}
}
