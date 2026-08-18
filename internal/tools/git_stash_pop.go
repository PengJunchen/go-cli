package tools

import (
	"context"
	"errors"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// GitStashPopTool exposes git stash pop through the ToolDefinition interface.
// It restores the most recently stashed changes.
type GitStashPopTool struct {
	git GitTool
}

var _ ToolDefinition = (*GitStashPopTool)(nil)

// NewGitStashPopTool returns a GitStashPopTool backed by the given GitTool,
// which must be non-nil.
func NewGitStashPopTool(git GitTool) *GitStashPopTool {
	return &GitStashPopTool{git: git}
}

// Name returns the tool name.
func (t *GitStashPopTool) Name() string { return "git_stash_pop" }

// Description returns a brief description of the tool.
func (t *GitStashPopTool) Description() string {
	return "git_stash_pop: Restore the most recently stashed changes."
}

// Parameters returns an empty schema; git_stash_pop takes no parameters.
func (t *GitStashPopTool) Parameters() any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": false,
	}
}

// Execute runs git stash pop to restore stashed changes.
func (t *GitStashPopTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	if t.git == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("git_stash_pop: nil git tool")
	}

	if err := t.git.StashPop(ctx); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_stash_pop.failed", "err", err)
		return nil, err
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("git_stash_pop.done")

	return &ToolResult{
		Output:     "stash restored",
		ToolCallID: call.ID,
		Metadata:   map[string]any{},
	}, nil
}
