package tools

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mcpGenerationTestTool struct {
	name       string
	parameters any
	calls      atomic.Int32
}

func (t *mcpGenerationTestTool) Name() string        { return t.name }
func (t *mcpGenerationTestTool) Description() string { return "MCP test tool " + t.name }
func (t *mcpGenerationTestTool) Parameters() any     { return t.parameters }
func (t *mcpGenerationTestTool) Execute(context.Context, ToolCall) (*ToolResult, error) {
	t.calls.Add(1)
	return &ToolResult{Output: t.name}, nil
}

func newMCPGenerationTestTool(name string) *mcpGenerationTestTool {
	return &mcpGenerationTestTool{
		name: name,
		parameters: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
	}
}

func TestMCPGenerationUpdatePublishesCompleteGeneration(t *testing.T) {
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	adapter := NewDeferredToolRegistryAdapter(base)
	firstFence := base.DispatchFence().Snapshot()
	oldOne := newMCPGenerationTestTool("mcp__srv__old_one")
	oldTwo := newMCPGenerationTestTool("mcp__srv__old_two")

	first, err := adapter.BeginMCPGenerationUpdate(context.Background(), "project-config", 1, []MCPToolCandidate{
		{Definition: oldOne},
		{Definition: oldTwo},
	})
	require.NoError(t, err)
	require.NoError(t, first.Publish())
	assert.Equal(t, firstFence.Registry+1, base.DispatchFence().Snapshot().Registry)
	assert.Equal(t, []string{oldOne.Name(), oldTwo.Name()}, deferredToolNames(mustListTools(t, adapter)))
	assert.ErrorIs(t, func() error { _, err := base.Get(context.Background(), oldOne.Name()); return err }(), ErrToolNotFound)

	loaded, err := adapter.Get(context.Background(), oldOne.Name())
	require.NoError(t, err)
	assert.NotSame(t, oldOne, loaded, "published generations expose a candidate-owned immutable definition wrapper")
	assert.Equal(t, oldOne.Name(), loaded.Name())
	beforeReplace := base.DispatchFence().Snapshot()
	newOnly := newMCPGenerationTestTool("mcp__srv__new_only")
	second, err := adapter.BeginMCPGenerationUpdate(context.Background(), "project-config", 2, []MCPToolCandidate{{Definition: newOnly}})
	require.NoError(t, err)
	require.NoError(t, second.Publish())
	assert.Equal(t, beforeReplace.Registry+1, base.DispatchFence().Snapshot().Registry)
	assert.Equal(t, []string{newOnly.Name()}, deferredToolNames(mustListTools(t, adapter)))
	assert.False(t, adapter.IsLoaded(oldOne.Name()))
	assert.False(t, adapter.IsLoaded(oldTwo.Name()))
	_, err = adapter.Get(context.Background(), oldOne.Name())
	assert.ErrorIs(t, err, ErrToolNotFound)
	assert.ErrorIs(t, func() error { _, err := base.Get(context.Background(), oldOne.Name()); return err }(), ErrToolNotFound)
}

func TestMCPGenerationCollisionDisablesOnlySourceOwnedTools(t *testing.T) {
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	adapter := NewDeferredToolRegistryAdapter(base)
	old := newMCPGenerationTestTool("mcp__srv__old")
	first, err := adapter.BeginMCPGenerationUpdate(context.Background(), "project-config", 1, []MCPToolCandidate{{Definition: old}})
	require.NoError(t, err)
	require.NoError(t, first.Publish())
	_, err = adapter.Get(context.Background(), old.Name())
	require.NoError(t, err)

	foreign := &fakeTool{name: "mcp__srv__collision", description: "foreign"}
	require.NoError(t, base.Register(context.Background(), foreign))
	beforeRejectedPublish := base.DispatchFence().Snapshot()
	candidate := newMCPGenerationTestTool(foreign.Name())
	second, err := adapter.BeginMCPGenerationUpdate(context.Background(), "project-config", 2, []MCPToolCandidate{{Definition: candidate}})
	require.NoError(t, err)
	require.ErrorIs(t, second.Publish(), ErrMCPNameReserved)
	assert.Equal(t, beforeRejectedPublish, base.DispatchFence().Snapshot(), "checked rejection must not bump any fence generation")
	require.NoError(t, second.Disable())
	assert.Equal(t, beforeRejectedPublish.Registry+1, base.DispatchFence().Snapshot().Registry)
	assert.Equal(t, []string{foreign.Name()}, deferredToolNames(mustListTools(t, adapter)))
	assert.ErrorIs(t, func() error { _, err := base.Get(context.Background(), old.Name()); return err }(), ErrToolNotFound)
	gotForeign, err := base.Get(context.Background(), foreign.Name())
	require.NoError(t, err)
	assert.Same(t, foreign, gotForeign)
}

func TestMCPGenerationReservationsRejectEagerAndDeferredShadowing(t *testing.T) {
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	adapter := NewDeferredToolRegistryAdapter(base)
	tool := newMCPGenerationTestTool("mcp__srv__reserved")
	update, err := adapter.BeginMCPGenerationUpdate(context.Background(), "project-config", 1, []MCPToolCandidate{{Definition: tool}})
	require.NoError(t, err)
	require.NoError(t, update.Publish())
	fence := base.DispatchFence().Snapshot()

	err = adapter.Register(context.Background(), &fakeTool{name: tool.Name(), description: "shadow"})
	assert.ErrorIs(t, err, ErrMCPNameReserved)
	err = base.Register(context.Background(), &fakeTool{name: tool.Name(), description: "direct shadow"})
	assert.ErrorIs(t, err, ErrMCPNameReserved)
	err = adapter.RegisterDeferred(context.Background(), tool.Name(), func() (ToolDefinition, error) {
		return &fakeTool{name: tool.Name()}, nil
	})
	assert.ErrorIs(t, err, ErrMCPNameReserved)
	assert.Equal(t, fence, base.DispatchFence().Snapshot(), "rejected shadow registrations must leave publication unchanged")
	assert.Equal(t, []string{tool.Name()}, deferredToolNames(mustListTools(t, adapter)))
}

func TestMCPGenerationRejectsTrustedRegistrationOverReservedName(t *testing.T) {
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	adapter := NewDeferredToolRegistryAdapter(base)
	tool := newMCPGenerationTestTool("mcp__srv__reserved")
	update, err := adapter.BeginMCPGenerationUpdate(context.Background(), "project-config", 1, []MCPToolCandidate{{Definition: tool}})
	require.NoError(t, err)
	require.NoError(t, update.Publish())
	fence := base.DispatchFence().Snapshot()

	trusted := newMCPGenerationTestTool(tool.Name())
	err = base.RegisterTrustedControl(context.Background(), trusted)
	assert.ErrorIs(t, err, ErrMCPNameReserved, "direct trusted registration must respect source reservations")
	err = adapter.RegisterTrustedControl(context.Background(), trusted)
	assert.ErrorIs(t, err, ErrMCPNameReserved, "adapter-forwarded trusted registration must respect source reservations")
	assert.Equal(t, fence, base.DispatchFence().Snapshot(), "rejected registrations must not change the published generation")
	assert.Equal(t, []string{tool.Name()}, deferredToolNames(mustListTools(t, adapter)))
}

func TestMCPGenerationSnapshotsCandidateNameAndSchemaBeforePublish(t *testing.T) {
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	adapter := NewDeferredToolRegistryAdapter(base)
	tool := newMCPGenerationTestTool("mcp__srv__original")
	originalSchema := tool.Parameters()
	update, err := adapter.BeginMCPGenerationUpdate(context.Background(), "project-config", 1, []MCPToolCandidate{{Definition: tool}})
	require.NoError(t, err)

	tool.name = "mcp__srv__mutated"
	tool.parameters = map[string]any{"type": "string"}
	require.NoError(t, update.Publish())

	loaded, err := adapter.Get(context.Background(), "mcp__srv__original")
	require.NoError(t, err)
	assert.Equal(t, "mcp__srv__original", loaded.Name())
	parameterized, ok := loaded.(Parameterized)
	require.True(t, ok)
	assert.Equal(t, originalSchema, parameterized.Parameters(), "published schema must not retain the caller's mutable map")
	_, err = adapter.Get(context.Background(), "mcp__srv__mutated")
	assert.ErrorIs(t, err, ErrToolNotFound)
}

func TestMCPDeferredMaterializationFailureIsRetryable(t *testing.T) {
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	adapter := NewDeferredToolRegistryAdapter(base)
	tool := newMCPGenerationTestTool("mcp__srv__retry")
	update, err := adapter.BeginMCPGenerationUpdate(context.Background(), "project-config", 1, []MCPToolCandidate{{Definition: tool}})
	require.NoError(t, err)
	require.NoError(t, update.Publish())

	// Model a late bypassing registration by placing a foreign eager owner
	// behind the active reservation. The source loader must not cache success.
	foreign := &fakeTool{name: tool.Name(), description: "late foreign owner"}
	base.mu.Lock()
	base.tools[tool.Name()] = foreign
	base.metadata[tool.Name()] = toolRegistrationMetadata{
		identity: newTrustedToolIdentity(RegistrationSourceToolRegistry, tool.Name(), 1, [32]byte{}, TrustedToolScope{}),
		effects:  unknownTrustedEffects(),
	}
	base.order = append(base.order, tool.Name())
	base.mu.Unlock()
	fence := base.DispatchFence().Snapshot()

	_, err = adapter.Load(context.Background(), tool.Name())
	require.ErrorIs(t, err, ErrMCPNameReserved)
	assert.False(t, adapter.IsLoaded(tool.Name()))
	assert.Equal(t, fence, base.DispatchFence().Snapshot(), "failed materialization must not bump the registry fence")

	base.mu.Lock()
	delete(base.tools, tool.Name())
	delete(base.metadata, tool.Name())
	removeRegistryName(&base.order, tool.Name())
	base.mu.Unlock()
	got, err := adapter.Load(context.Background(), tool.Name())
	require.NoError(t, err)
	assert.Equal(t, tool.Name(), got.Name())
	result, err := got.Execute(context.Background(), ToolCall{Name: tool.Name()})
	require.NoError(t, err)
	assert.Equal(t, tool.Name(), result.Output)
	assert.True(t, adapter.IsLoaded(tool.Name()))
	assert.Equal(t, fence.Registry+1, base.DispatchFence().Snapshot().Registry)
}

func TestMCPDeferredLoadSerializesAgainstReplacement(t *testing.T) {
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	adapter := NewDeferredToolRegistryAdapter(base)
	old := newMCPGenerationTestTool("mcp__srv__shared")
	first, err := adapter.BeginMCPGenerationUpdate(context.Background(), "project-config", 1, []MCPToolCandidate{{Definition: old}})
	require.NoError(t, err)
	require.NoError(t, first.Publish())

	loadEntered := make(chan struct{})
	resumeLoad := make(chan struct{})
	adapter.mu.Lock()
	adapter.loaders[old.Name()] = func() (ToolDefinition, error) {
		close(loadEntered)
		<-resumeLoad
		return old, nil
	}
	adapter.mu.Unlock()
	loadDone := make(chan error, 1)
	go func() {
		_, err := adapter.Load(context.Background(), old.Name())
		loadDone <- err
	}()
	<-loadEntered

	newTool := newMCPGenerationTestTool(old.Name())
	replacementDone := make(chan error, 1)
	go func() {
		replacement, err := adapter.BeginMCPGenerationUpdate(context.Background(), "project-config", 2, []MCPToolCandidate{{Definition: newTool}})
		if err == nil {
			err = replacement.Publish()
		}
		replacementDone <- err
	}()
	select {
	case err := <-replacementDone:
		require.NoError(t, err)
		t.Fatal("replacement crossed the deferred load's DTR critical section")
	case <-time.After(20 * time.Millisecond):
	}
	close(resumeLoad)
	require.NoError(t, <-loadDone)
	require.NoError(t, <-replacementDone)

	got, err := adapter.Get(context.Background(), old.Name())
	require.NoError(t, err)
	assert.Equal(t, newTool.Name(), got.Name())
	assert.Equal(t, int32(0), old.calls.Load())
	assert.Equal(t, int32(0), newTool.calls.Load(), "materialization must not execute the captured candidate")
	assert.Equal(t, []string{old.Name()}, deferredToolNames(mustListTools(t, adapter)))
}

func TestMCPGenerationDisablePreservesForeignReservationsAndAdvancesGeneration(t *testing.T) {
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	adapter := NewDeferredToolRegistryAdapter(base)
	firstTool := newMCPGenerationTestTool("mcp__srv__first")
	first, err := adapter.BeginMCPGenerationUpdate(context.Background(), "project-config", 1, []MCPToolCandidate{{Definition: firstTool}})
	require.NoError(t, err)
	require.NoError(t, first.Publish())
	foreign := &fakeTool{name: "mcp__other__foreign", description: "foreign"}
	require.NoError(t, base.Register(context.Background(), foreign))

	disable, err := adapter.BeginMCPGenerationUpdate(context.Background(), "project-config", 2, nil)
	require.NoError(t, err)
	require.NoError(t, disable.Disable())
	assert.Equal(t, []string{foreign.Name()}, deferredToolNames(mustListTools(t, adapter)))

	stale, err := adapter.BeginMCPGenerationUpdate(context.Background(), "project-config", 2, nil)
	assert.Nil(t, stale)
	assert.ErrorIs(t, err, ErrMCPGenerationStale)
	_, err = adapter.Get(context.Background(), firstTool.Name())
	assert.ErrorIs(t, err, ErrToolNotFound)
}

func mustListTools(t *testing.T, registry ToolRegistry) []ToolDefinition {
	t.Helper()
	defs, err := registry.List(context.Background())
	require.NoError(t, err)
	return defs
}

func TestMCPGenerationRejectedCandidateDoesNotMutateBeforeDisable(t *testing.T) {
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	adapter := NewDeferredToolRegistryAdapter(base)
	foreign := &fakeTool{name: "mcp__srv__foreign", description: "foreign"}
	require.NoError(t, base.Register(context.Background(), foreign))
	before := base.DispatchFence().Snapshot()
	update, err := adapter.BeginMCPGenerationUpdate(context.Background(), "project-config", 1, []MCPToolCandidate{{Definition: newMCPGenerationTestTool(foreign.Name())}})
	require.NoError(t, err)
	err = update.Publish()
	require.ErrorIs(t, err, ErrMCPNameReserved)
	assert.Equal(t, before, base.DispatchFence().Snapshot())

	// The source is empty on first publication, so Abort is the correct
	// availability-preserving outcome for this isolated registry transaction.
	update.Abort()
	assert.ErrorIs(t, update.Disable(), ErrMCPGenerationTransactionClosed)
	got := mustListTools(t, adapter)
	assert.Equal(t, []string{foreign.Name()}, deferredToolNames(got))
	assert.False(t, errors.Is(err, ErrMCPGenerationStale))
}
