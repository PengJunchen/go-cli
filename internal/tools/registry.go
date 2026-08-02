package tools

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// ErrToolNotFound is returned by Get when no tool with the requested name has
// been registered.
var ErrToolNotFound = errors.New("tools: tool not found")

// DefaultToolRegistry is the default tools.ToolRegistry implementation. It is
// concurrency-safe: Register, Get and List may be called concurrently.
//
// Registering a tool under an already-registered name replaces the previously
// registered definition (last registration wins), and the new definition is
// appended to the registration order returned by List.
type DefaultToolRegistry struct {
	mu    sync.RWMutex
	tools map[string]ToolDefinition
	order []string
}

var _ ToolRegistry = (*DefaultToolRegistry)(nil)

// NewDefaultToolRegistry returns an empty, ready-to-use registry.
func NewDefaultToolRegistry() ToolRegistry {
	return &DefaultToolRegistry{
		tools: map[string]ToolDefinition{},
	}
}

// Register stores def under its name. It returns an error when def is nil.
// Registering a name a second time overwrites the previous definition.
func (r *DefaultToolRegistry) Register(_ context.Context, def ToolDefinition) error {
	if def == nil {
		return errors.New("tools: cannot register a nil tool definition")
	}

	name := def.Name()
	if name == "" {
		return errors.New("tools: cannot register a tool with an empty name")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.tools[name]; !ok {
		r.order = append(r.order, name)
	}
	r.tools[name] = def

	return nil
}

// Get returns the tool registered under name, or ErrToolNotFound.
func (r *DefaultToolRegistry) Get(_ context.Context, name string) (ToolDefinition, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	def, ok := r.tools[name]
	if !ok {
		return nil, ErrToolNotFound
	}
	return def, nil
}

// List returns all registered tools in registration order. The returned slice
// is a copy, so callers may not mutate the registry through it.
func (r *DefaultToolRegistry) List(_ context.Context) ([]ToolDefinition, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	defs := make([]ToolDefinition, 0, len(r.order))
	for _, name := range r.order {
		defs = append(defs, r.tools[name])
	}
	return defs, nil
}

// Execute is a convenience method that looks up a tool by name and runs it,
// emitting a `tool.call` span with tool_name / duration_ms / success
// attributes. It is not part of the ToolRegistry interface; it exists so the
// agent loop and tests have a single entry point for tool execution.
func (r *DefaultToolRegistry) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	if call.Name == "" {
		return nil, errors.New("tools: tool call has no name")
	}

	span, spanCtx := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	start := time.Now()
	defer func() {
		ms := time.Since(start).Milliseconds()
		span.SetAttributes(
			tracing.Attribute{Key: "tool_name", Value: call.Name},
			tracing.Attribute{Key: "duration_ms", Value: ms},
		)
		span.End()
	}()

	def, err := r.Get(ctx, call.Name)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("tool.call.failed",
			"tool", call.Name,
			"duration_ms", time.Since(start).Milliseconds(),
			"err", err)
		return nil, err
	}

	result, execErr := def.Execute(spanCtx, call)
	span.SetAttributes(tracing.Attribute{Key: "success", Value: execErr == nil})

	if execErr != nil {
		logger.Error("tool.call.failed",
			"tool", call.Name,
			"duration_ms", time.Since(start).Milliseconds(),
			"err", execErr)
		return nil, execErr
	}

	logger.Info("tool.call",
		"tool", call.Name,
		"duration_ms", time.Since(start).Milliseconds(),
		"success", true)

	return result, nil
}
