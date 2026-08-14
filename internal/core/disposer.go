package core

import (
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// Disposer is a cleanup function returned by RegisterXxxWithDisposer methods.
// Calling it releases resources associated with a replaced implementation.
type Disposer func()

// DisposableRegistry extends Registry with reversible registration methods.
// Each RegisterXxxWithDisposer method behaves like its RegisterXxx counterpart
// but additionally returns a Disposer for cleanup and stores it internally so
// that DisposeAll can invoke all disposers in reverse registration order.
type DisposableRegistry interface {
	Registry

	// RegisterAgentLoopWithDisposer replaces the AgentLoop implementation,
	// returns the old one, and registers a disposer for cleanup.
	RegisterAgentLoopWithDisposer(n AgentLoop) (AgentLoop, Disposer)

	// RegisterAgentWithDisposer replaces the Agent implementation, returns
	// the old one, and registers a disposer for cleanup.
	RegisterAgentWithDisposer(n Agent) (Agent, Disposer)

	// RegisterHarnessWithDisposer replaces the Harness implementation,
	// returns the old one, and registers a disposer for cleanup.
	RegisterHarnessWithDisposer(n Harness) (Harness, Disposer)

	// RegisterToolRegistryWithDisposer replaces the ToolRegistry
	// implementation, returns the old one, and registers a disposer for
	// cleanup.
	RegisterToolRegistryWithDisposer(n tools.ToolRegistry) (tools.ToolRegistry, Disposer)

	// RegisterModelProviderWithDisposer replaces the ModelProvider
	// implementation, returns the old one, and registers a disposer for
	// cleanup.
	RegisterModelProviderWithDisposer(n llm.ModelProvider) (llm.ModelProvider, Disposer)

	// DisposeAll calls all stored disposers in reverse registration order
	// (LIFO) and clears the internal slice. It is safe to call multiple
	// times; subsequent calls are no-ops.
	DisposeAll()
}
