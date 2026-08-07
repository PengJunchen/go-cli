package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// GitRevertTool exposes git revert through the ToolDefinition interface. This
// is a destructive operation that reverts a commit and requires approval
// before execution.
type GitRevertTool struct {
	git GitTool
}

var _ ToolDefinition = (*GitRevertTool)(nil)

// NewGitRevertTool returns a GitRevertTool backed by the given GitTool, which
// must be non-nil.
func NewGitRevertTool(git GitTool) *GitRevertTool {
	return &GitRevertTool{git: git}
}

// Name returns the tool name.
func (t *GitRevertTool) Name() string { return "git_revert" }

// Description returns a brief description of the tool.
func (t *GitRevertTool) Description() string {
	return "git_revert: Revert a commit by creating a new commit that undoes it. [REQUIRES APPROVAL] Destructive operation."
}

// Parameters returns the JSON Schema describing the tool's input parameters.
func (t *GitRevertTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"commit": map[string]any{
				"type":        "string",
				"description": "The commit hash to revert.",
			},
		},
		"required":             []string{"commit"},
		"additionalProperties": false,
	}
}

// Execute runs git revert to undo the given commit.
func (t *GitRevertTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	if t.git == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("git_revert: nil git tool")
	}

	commit, ok := call.Args["commit"].(string)
	if !ok || strings.TrimSpace(commit) == "" {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_revert.missing_commit")
		return nil, errors.New("git_revert: missing string argument 'commit'")
	}

	if err := t.git.Revert(ctx, commit); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_revert.failed", "commit", commit, "err", err)
		return nil, err
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("git_revert.done", "commit", commit)

	return &ToolResult{
		Output:     "reverted: " + commit,
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"commit": commit,
		},
	}, nil
}
