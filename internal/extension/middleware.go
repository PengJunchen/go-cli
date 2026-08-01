package extension

import (
	"context"
	"log/slog"
)

// This file defines the Phase 4 middleware contracts: the agent-level
// Middleware plus the narrower ModelMiddleware and ToolMiddleware, together
// with self-contained value structs for the data they shuttle.

// AgentInput is the input handed to an agent function.
type AgentInput struct {
	// Message is the user-provided prompt.
	Message string
	// Data carries optional structured context.
	Data any
}

// AgentOutput is the output produced by an agent function.
type AgentOutput struct {
	// Text is the final human-readable response.
	Text string
	// Data carries optional structured result data.
	Data any
}

// AgentFunc is the underlying agent computation that middleware wraps.
type AgentFunc func(ctx context.Context, input AgentInput) (AgentOutput, error)

// Middleware wraps an AgentFunc (onion model) to intercept and augment agent
// behavior.
type Middleware interface {
	// Name returns the middleware identifier.
	Name() string
	// WrapAgent returns a wrapped view of the given AgentFunc.
	WrapAgent(next AgentFunc) AgentFunc
}

// DefaultMiddleware is a pass-through middleware that returns the underlying
// AgentFunc unchanged.
type DefaultMiddleware struct {
	name string
}

var _ Middleware = (*DefaultMiddleware)(nil)

// Name returns the middleware name, defaulting to "default-middleware".
func (m *DefaultMiddleware) Name() string {
	return m.name
}

// WrapAgent returns next unchanged (pass-through).
func (m *DefaultMiddleware) WrapAgent(next AgentFunc) AgentFunc {
	return func(ctx context.Context, input AgentInput) (AgentOutput, error) {
		slog.Info("extension.middleware", "name", m.Name())
		return next(ctx, input)
	}
}

// ModelRequest is the request handed to a model function.
type ModelRequest struct {
	// Prompt is the user-facing prompt text.
	Prompt string
	// Model is the requested model identifier.
	Model string
	// Temperature controls sampling randomness.
	Temperature float64
}

// ModelResponse is the response produced by a model function.
type ModelResponse struct {
	// Text is the generated completion text.
	Text string
}

// ModelFunc is the underlying model computation that middleware wraps.
type ModelFunc func(ctx context.Context, req ModelRequest) (ModelResponse, error)

// ModelMiddleware wraps a ModelFunc to intercept and augment model calls.
type ModelMiddleware interface {
	// Name returns the middleware identifier.
	Name() string
	// WrapModel returns a wrapped view of the given ModelFunc.
	WrapModel(next ModelFunc) ModelFunc
}

// DefaultModelMiddleware is a pass-through model middleware.
type DefaultModelMiddleware struct {
	name string
}

var _ ModelMiddleware = (*DefaultModelMiddleware)(nil)

// Name returns the middleware name, defaulting to "default-model-middleware".
func (m *DefaultModelMiddleware) Name() string {
	return m.name
}

// WrapModel returns next unchanged (pass-through).
func (m *DefaultModelMiddleware) WrapModel(next ModelFunc) ModelFunc {
	return func(ctx context.Context, req ModelRequest) (ModelResponse, error) {
		slog.Info("extension.model_middleware", "name", m.Name())
		return next(ctx, req)
	}
}

// ToolFunc is the underlying tool computation that middleware wraps.
type ToolFunc func(ctx context.Context, name string, input any) (any, error)

// ToolMiddleware wraps a ToolFunc to intercept and augment tool calls.
type ToolMiddleware interface {
	// Name returns the middleware identifier.
	Name() string
	// WrapTool returns a wrapped view of the given ToolFunc.
	WrapTool(next ToolFunc) ToolFunc
}

// DefaultToolMiddleware is a pass-through tool middleware.
type DefaultToolMiddleware struct {
	name string
}

var _ ToolMiddleware = (*DefaultToolMiddleware)(nil)

// Name returns the middleware name, defaulting to "default-tool-middleware".
func (m *DefaultToolMiddleware) Name() string {
	return m.name
}

// WrapTool returns next unchanged (pass-through).
func (m *DefaultToolMiddleware) WrapTool(next ToolFunc) ToolFunc {
	return func(ctx context.Context, name string, input any) (any, error) {
		slog.Info("extension.tool_middleware", "name", m.Name())
		return next(ctx, name, input)
	}
}
