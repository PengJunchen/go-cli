package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMCPAdditionPreservesPreparedSiblingAuthority(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	deferred := NewDeferredToolRegistryAdapter(base)
	firstTool := newMCPGenerationTestTool("mcp__first__read")
	first, err := deferred.BeginMCPGenerationUpdate(ctx, "first", 1, []MCPToolCandidate{newMCPGenerationTestCandidate(t, firstTool)})
	require.NoError(t, err)
	require.NoError(t, first.Publish())
	registry := NewMiddlewareToolRegistry(deferred)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	pending, err := registry.PrepareToolCall(ctx, ToolCall{ID: "pending", Name: firstTool.Name(), Args: map[string]any{}})
	require.NoError(t, err)
	fence := base.DispatchFence().Snapshot()
	version := base.Version()
	secondTool := newMCPGenerationTestTool("mcp__second__read")
	addition, err := deferred.BeginMCPGenerationAddition(ctx, "second", 1, []MCPToolCandidate{newMCPGenerationTestCandidate(t, secondTool)})
	require.NoError(t, err)
	require.NoError(t, addition.Publish())
	require.Equal(t, fence, base.DispatchFence().Snapshot())
	require.Greater(t, base.Version(), version)
	_, err = registry.PrepareToolCall(ctx, ToolCall{ID: "new-request", Name: secondTool.Name(), Args: map[string]any{}})
	require.NoError(t, err)
	require.Equal(t, fence, base.DispatchFence().Snapshot(), "first materialization is an addition, not replacement")
	result, err := pending.Dispatch(ctx)
	require.NoError(t, err)
	require.Equal(t, firstTool.Name(), result.Output)
}

func TestMCPAdditionCannotBypassReplacementOrRevocation(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	deferred := NewDeferredToolRegistryAdapter(base)
	tool := newMCPGenerationTestTool("mcp__source__read")
	first, err := deferred.BeginMCPGenerationAddition(ctx, "source", 1, []MCPToolCandidate{newMCPGenerationTestCandidate(t, tool)})
	require.NoError(t, err)
	require.NoError(t, first.Publish())
	fence := base.DispatchFence().Snapshot()
	replacement, err := deferred.BeginMCPGenerationAddition(ctx, "source", 2, []MCPToolCandidate{newMCPGenerationTestCandidate(t, tool)})
	require.NoError(t, err)
	require.NoError(t, replacement.Publish())
	require.Equal(t, fence.Registry+1, base.DispatchFence().Snapshot().Registry)
	disable, err := deferred.BeginMCPGenerationUpdate(ctx, "source", 3, nil)
	require.NoError(t, err)
	require.NoError(t, disable.Disable())
	fence = base.DispatchFence().Snapshot()
	regrant, err := deferred.BeginMCPGenerationAddition(ctx, "source", 4, []MCPToolCandidate{newMCPGenerationTestCandidate(t, tool)})
	require.NoError(t, err)
	require.NoError(t, regrant.Publish())
	require.Equal(t, fence.Registry+1, base.DispatchFence().Snapshot().Registry)
	collision, err := deferred.BeginMCPGenerationAddition(ctx, "foreign", 1, []MCPToolCandidate{newMCPGenerationTestCandidate(t, tool)})
	require.ErrorIs(t, err, ErrMCPNameReserved)
	require.Nil(t, collision)
}

func TestMCPMaterializedPublicationHasNoPostCommitLoader(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	deferred := NewDeferredToolRegistryAdapter(base)
	tool := newMCPGenerationTestTool("mcp__atomic__read")
	candidate := newMCPGenerationTestCandidate(t, tool)
	preview := candidate.PreviewDefinition()
	_, err := preview.Execute(ctx, ToolCall{Name: tool.Name()})
	require.ErrorIs(t, err, ErrPreparedInvocationUnavailable)
	update, err := deferred.BeginMCPGenerationAddition(ctx, "atomic", 1, []MCPToolCandidate{candidate})
	require.NoError(t, err)
	require.NoError(t, update.PublishMaterialized())
	require.True(t, deferred.IsLoaded(tool.Name()))
	definition, err := base.Get(ctx, tool.Name())
	require.NoError(t, err)
	result, err := definition.Execute(ctx, ToolCall{Name: tool.Name()})
	require.NoError(t, err)
	require.Equal(t, tool.Name(), result.Output)
}
