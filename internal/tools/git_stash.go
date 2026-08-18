package tools

import (
	"context"
	"errors"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// GitStashTool exposes git stash through the ToolDefinition interface. It
// saves uncommitted changes to the stash stack.
type GitStashTool struct {
	git GitTool
}

var _ ToolDefinition = (*GitStashTool)(nil)

// NewGitStashTool returns a GitStashTool backed by the given GitTool, which
// must be non-nil.
func NewGitStashTool(git GitTool) *GitStashTool {
	return &GitStashTool{git: git}
}

// Name returns the tool name.
func (t *GitStashTool) Name() string { return "git_stash" }

// Description returns a brief description of the tool.
func (t *GitStashTool) Description() string {
	return "git_stash: Save uncommitted changes to the stash stack."
}

// Parameters returns an empty schema; git_stash takes no parameters.
func (t *GitStashTool) Parameters() any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": false,
	}
}

// Execute runs git stash to save uncommitted changes.
func (t *GitStashTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	if t.git == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("git_stash: nil git tool")
	}

	if err := t.git.Stash(ctx); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_stash.failed", "err", err)
		return nil, err
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("git_stash.done")

	return &ToolResult{
		Output:     "changes stashed",
		ToolCallID: call.ID,
		Metadata:   map[string]any{},
	}, nil
}
