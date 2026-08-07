package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// GitResetTool exposes git reset through the ToolDefinition interface. This is
// a destructive operation that discards uncommitted changes and requires
// approval before execution.
type GitResetTool struct {
	git GitTool
}

var _ ToolDefinition = (*GitResetTool)(nil)

// NewGitResetTool returns a GitResetTool backed by the given GitTool, which
// must be non-nil.
func NewGitResetTool(git GitTool) *GitResetTool {
	return &GitResetTool{git: git}
}

// Name returns the tool name.
func (t *GitResetTool) Name() string { return "git_reset" }

// Description returns a brief description of the tool.
func (t *GitResetTool) Description() string {
	return "git_reset: Reset the working tree to HEAD. [REQUIRES APPROVAL] Destructive operation that discards uncommitted changes."
}

// Parameters returns the JSON Schema describing the tool's input parameters.
func (t *GitResetTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode": map[string]any{
				"type":        "string",
				"description": "Reset mode: hard, soft, mixed, keep, or merge. Defaults to hard.",
			},
		},
		"additionalProperties": false,
	}
}

// Execute runs git reset with the given mode.
func (t *GitResetTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	if t.git == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("git_reset: nil git tool")
	}

	mode := "hard"
	if v, ok := call.Args["mode"].(string); ok && strings.TrimSpace(v) != "" {
		mode = v
	}

	if err := t.git.Reset(ctx, mode); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_reset.failed", "mode", mode, "err", err)
		return nil, err
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("git_reset.done", "mode", mode)

	return &ToolResult{
		Output:     "reset --" + mode,
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"mode": mode,
		},
	}, nil
}
