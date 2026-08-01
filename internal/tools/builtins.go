package tools

import (
	"context"
	"fmt"
	"log/slog"
)

// RegisterDefaults registers the built-in read, bash and write tools into the
// given registry. It returns an error if any registration conflicts with an
// existing tool name.
func RegisterDefaults(ctx context.Context, reg ToolRegistry) error {
	if reg == nil {
		return fmt.Errorf("tools: nil registry")
	}

	defs := []ToolDefinition{
		NewReadTool(),
		NewBashTool(),
		NewWriteTool(),
	}

	for _, def := range defs {
		if err := reg.Register(ctx, def); err != nil {
			return fmt.Errorf("tools: register %s: %w", def.Name(), err)
		}
		slog.Info("tools.register_default", "tool", def.Name())
	}

	return nil
}
