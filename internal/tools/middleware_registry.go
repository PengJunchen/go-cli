package tools

import (
	"context"
)

// ToolExecutorWrapper wraps a tool execution function, intercepting the call
// before it reaches the underlying tool definition. It is the function-based
// equivalent of core.ToolMiddleware, designed to be usable from the tools
// package without creating an import cycle (core already imports tools).
//
// The wrapper receives the "next" executor (which calls the real tool) and
// returns a new executor that may short-circuit (e.g. deny) or modify the
// call before delegating to next.
type ToolExecutorWrapper func(next func(ctx context.Context, call ToolCall) (*ToolResult, error)) func(ctx context.Context, call ToolCall) (*ToolResult, error)

// MiddlewareToolRegistry is a ToolRegistry decorator that intercepts Get
// to wrap the returned ToolDefinition's Execute method with one or more
// ToolExecutorWrapper functions. Register and List delegate directly to
// the inner registry.
//
// This enables approval gates, mutation queues, and other cross-cutting
// concerns to be applied without modifying LoopAgent's core structure.
type MiddlewareToolRegistry struct {
	inner    ToolRegistry
	wrappers []ToolExecutorWrapper
}

// Compile-time interface guarantees.
var (
	_ ToolRegistry = (*MiddlewareToolRegistry)(nil)
)

// NewMiddlewareToolRegistry creates a decorator around inner that applies
// the given wrappers to every ToolDefinition returned via Get. Wrappers are
// applied in order, so the first wrapper is outermost (runs first on entry,
// last on exit), forming an onion model.
func NewMiddlewareToolRegistry(inner ToolRegistry, wrappers ...ToolExecutorWrapper) *MiddlewareToolRegistry {
	return &MiddlewareToolRegistry{
		inner:    inner,
		wrappers: wrappers,
	}
}

// Register delegates to the inner registry.
func (r *MiddlewareToolRegistry) Register(ctx context.Context, def ToolDefinition) error {
	return r.inner.Register(ctx, def)
}

// Get returns a wrapped ToolDefinition whose Execute method passes through
// the configured wrappers before reaching the underlying tool.
func (r *MiddlewareToolRegistry) Get(ctx context.Context, name string) (ToolDefinition, error) {
	def, err := r.inner.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(r.wrappers) == 0 {
		return def, nil
	}
	return &wrappedToolDef{def: def, wrappers: r.wrappers}, nil
}

// List delegates to the inner registry. The returned definitions are the
// original (unwrapped) ones; wrapping happens lazily in Get.
func (r *MiddlewareToolRegistry) List(ctx context.Context) ([]ToolDefinition, error) {
	return r.inner.List(ctx)
}

// Version delegates to the inner registry when it supports versioning. This
// keeps the tool-definition cache (which type-asserts to
// interface{ Version() int }) working through the middleware decorator: a
// hot-registered tool bumps the inner version, and the cache sees the new
// version through this forwarder. When the inner registry does not implement
// Version(), 0 is returned (cache stays valid after first build).
func (r *MiddlewareToolRegistry) Version() int {
	if v, ok := r.inner.(interface{ Version() int }); ok {
		return v.Version()
	}
	return 0
}

// wrappedToolDef adapts a ToolDefinition so that Execute passes through
// the configured wrappers. Name and Description delegate directly.
type wrappedToolDef struct {
	def      ToolDefinition
	wrappers []ToolExecutorWrapper
}

// Compile-time interface guarantee.
var _ ToolDefinition = (*wrappedToolDef)(nil)

// Name delegates to the underlying definition.
func (d *wrappedToolDef) Name() string { return d.def.Name() }

// Description delegates to the underlying definition.
func (d *wrappedToolDef) Description() string { return d.def.Description() }

// Execute builds a chain of wrappers around the underlying Execute and
// invokes the resulting function.
func (d *wrappedToolDef) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	executor := func(ctx context.Context, call ToolCall) (*ToolResult, error) {
		return d.def.Execute(ctx, call)
	}
	// Apply wrappers in reverse order so the first wrapper is outermost.
	for i := len(d.wrappers) - 1; i >= 0; i-- {
		executor = d.wrappers[i](executor)
	}
	return executor(ctx, call)
}
