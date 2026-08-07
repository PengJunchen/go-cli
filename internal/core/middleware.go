package core

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// ModelMiddleware wraps an llm.BaseChatModel to intercept and augment the LLM
// call (the onion model applied to the model layer). It complements the
// AgentLoop-oriented Middleware by sitting in front of model generation.
type ModelMiddleware interface {
	// Name returns the middleware identifier.
	Name() string
	// WrapModel returns a wrapped view of the given chat model.
	WrapModel(next llm.BaseChatModel) llm.BaseChatModel
}

// ModelMiddlewareImpl is the default pass-through ModelMiddleware.
type ModelMiddlewareImpl struct{ name string }

var _ ModelMiddleware = (*ModelMiddlewareImpl)(nil)

// NewModelMiddlewareImpl builds a named pass-through ModelMiddleware.
func NewModelMiddlewareImpl(name string) *ModelMiddlewareImpl {
	return &ModelMiddlewareImpl{name: name}
}

// Name returns the middleware name, defaulting when empty.
func (m *ModelMiddlewareImpl) Name() string {
	if m.name == "" {
		return "default-model-middleware"
	}
	return m.name
}

// WrapModel returns next unchanged (pass-through).
func (m *ModelMiddlewareImpl) WrapModel(next llm.BaseChatModel) llm.BaseChatModel {
	slog.Info("core.model_middleware.wrap", "name", m.Name())
	return next
}

// ToolMiddleware wraps a single tool execution (call -> result). Its small,
// testable signature intercepts one tool call round trip.
type ToolMiddleware interface {
	// Name returns the middleware identifier.
	Name() string
	// WrapToolCall returns a wrapped view of the given tool call executor.
	WrapToolCall(next func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error)) func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error)
}

// ToolMiddlewareImpl is the default pass-through ToolMiddleware.
type ToolMiddlewareImpl struct{ name string }

var _ ToolMiddleware = (*ToolMiddlewareImpl)(nil)

// NewToolMiddlewareImpl builds a named pass-through ToolMiddleware.
func NewToolMiddlewareImpl(name string) *ToolMiddlewareImpl { return &ToolMiddlewareImpl{name: name} }

// Name returns the middleware name, defaulting when empty.
func (m *ToolMiddlewareImpl) Name() string {
	if m.name == "" {
		return "default-tool-middleware"
	}
	return m.name
}

// WrapToolCall returns next unchanged (pass-through).
func (m *ToolMiddlewareImpl) WrapToolCall(next func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error)) func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	slog.Info("core.tool_middleware.wrap", "name", m.Name())
	return next
}

// HookAwareToolMiddleware is a ToolMiddleware that integrates a HookChain into
// the tool execution path. Before each tool call it invokes the chain's
// BeforeToolCall (which may block the call); after the call it invokes
// AfterToolCall so hooks observe the outcome. Hooks that do not implement
// LifecycleHook are skipped.
type HookAwareToolMiddleware struct {
	chain *HookChain
	name  string
}

var _ ToolMiddleware = (*HookAwareToolMiddleware)(nil)

// NewHookAwareToolMiddleware builds a HookAwareToolMiddleware backed by the
// given HookChain. A nil chain makes the middleware a pass-through.
func NewHookAwareToolMiddleware(chain *HookChain) *HookAwareToolMiddleware {
	return &HookAwareToolMiddleware{chain: chain, name: "hook-aware-tool"}
}

// Name returns the middleware identifier.
func (m *HookAwareToolMiddleware) Name() string {
	if m.name == "" {
		return "hook-aware-tool"
	}
	return m.name
}

// WrapToolCall returns a wrapper that invokes BeforeToolCall before the
// underlying tool executes and AfterToolCall afterwards. When BeforeToolCall
// returns an error the tool call is blocked. AfterToolCall errors are logged
// but do not override the original result or error.
func (m *HookAwareToolMiddleware) WrapToolCall(next func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error)) func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	return func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		if m.chain != nil {
			if err := m.chain.BeforeToolCall(ctx, call); err != nil {
				return nil, fmt.Errorf("tool call blocked by hook: %w", err)
			}
		}
		result, err := next(ctx, call)
		if m.chain != nil {
			if aerr := m.chain.AfterToolCall(ctx, call, result, err); aerr != nil {
				slog.Warn("core.hook_aware_tool.after_error", "tool", call.Name, "err", aerr)
			}
		}
		return result, err
	}
}

// MiddlewareChain composes a list of Middleware into a single wrapped AgentLoop
// using the onion model. Middlewares are applied in order so the first listed
// middleware becomes the outermost wrapper.
type MiddlewareChain struct {
	middlewares []Middleware
}

// NewMiddlewareChain builds a MiddlewareChain from the given middlewares.
func NewMiddlewareChain(mws ...Middleware) *MiddlewareChain {
	return &MiddlewareChain{middlewares: append([]Middleware{}, mws...)}
}

// Wrap composes the middlewares over base. The first middleware in the chain is
// the outermost wrapper and runs first when the loop executes.
func (c *MiddlewareChain) Wrap(base AgentLoop) AgentLoop {
	loop := base
	for i := len(c.middlewares) - 1; i >= 0; i-- {
		loop = c.middlewares[i].Wrap(loop)
	}
	return loop
}

// Apply composes the middlewares over base and returns the wrapped loop. It is
// an alias of Wrap for callers that prefer the verb form.
func (c *MiddlewareChain) Apply(base AgentLoop) AgentLoop { return c.Wrap(base) }

// LoggingMiddleware is a concrete Middleware that wraps an AgentLoop and emits
// a "middleware.*" span plus trace-aware slog lines around each Run call.
type LoggingMiddleware struct{ name string }

var _ Middleware = (*LoggingMiddleware)(nil)

// NewLoggingMiddleware builds a named logging middleware.
func NewLoggingMiddleware(name string) *LoggingMiddleware { return &LoggingMiddleware{name: name} }

// Name returns the middleware name, defaulting when empty.
func (m *LoggingMiddleware) Name() string {
	if m.name == "" {
		return "default-logging-middleware"
	}
	return m.name
}

// Wrap returns a loop-view that logs around Run.
func (m *LoggingMiddleware) Wrap(next AgentLoop) AgentLoop {
	return &loggingLoop{name: m.Name(), next: next}
}

// loggingLoop is the concrete wrapped loop produced by LoggingMiddleware.
type loggingLoop struct {
	name string
	next AgentLoop
}

// Run logs before and after delegating to the wrapped loop.
func (l *loggingLoop) Run(ctx context.Context, submission Submission, stream ...EventStream) ([]AgentEvent, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "middleware."+l.name, tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, nil)
	slog.Info("core.logging_middleware.run", "name", l.name, "type", submission.Type)
	logger.Info("middleware.run", "name", l.name, "type", submission.Type)

	events, err := l.next.Run(spanCtx, submission, stream...)

	slog.Info("core.logging_middleware.done", "name", l.name, "events", len(events), "error", err != nil)
	logger.Info("middleware.done", "name", l.name, "events", len(events), "error", err != nil)
	return events, err
}
