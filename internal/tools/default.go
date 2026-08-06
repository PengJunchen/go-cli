package tools

import (
	"context"
	"errors"
	"log/slog"
)

// These types are the default (compile-time) implementations of the tools
// contracts. Every production interface in a package should
// carry a `var _ Interface = (*Impl)(nil)` default-implementation assertion.
// Real behavior is provided by the mock framework (internal/mock) and later
// production registries; these defaults are minimal placeholders that exist so
// the interfaces are always backed by a conforming type within this package.

// errDefaultTool reports that a default tool/registry is not backed by a real
// implementation.
var errDefaultTool = errors.New("tools: default tool not implemented")

// defaultToolDef is a minimal ToolDefinition that does nothing.
type defaultToolDef struct{}

func (defaultToolDef) Name() string        { return "default" }
func (defaultToolDef) Description() string { return "" }
func (defaultToolDef) Execute(_ context.Context, _ ToolCall) (*ToolResult, error) {
	slog.Debug("tools: default tool execute called (not implemented)")
	return nil, errDefaultTool
}

var _ ToolDefinition = (*defaultToolDef)(nil)

// defaultToolRegistry is a minimal ToolRegistry with no registered tools.
type defaultToolRegistry struct{}

func (defaultToolRegistry) Register(_ context.Context, _ ToolDefinition) error {
	slog.Debug("tools: default registry register called (no-op)")
	return nil
}
func (defaultToolRegistry) Get(_ context.Context, _ string) (ToolDefinition, error) {
	slog.Debug("tools: default registry get called (not implemented)")
	return nil, errDefaultTool
}
func (defaultToolRegistry) List(_ context.Context) ([]ToolDefinition, error) {
	slog.Debug("tools: default registry list called (empty)")
	return nil, nil
}

var _ ToolRegistry = (*defaultToolRegistry)(nil)
