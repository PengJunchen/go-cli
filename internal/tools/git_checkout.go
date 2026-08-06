package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// GitCheckoutTool exposes git checkout through the ToolDefinition interface.
// This is a destructive operation: it can discard uncommitted changes and
// requires approval before execution.
type GitCheckoutTool struct {
	git GitTool
}

var _ ToolDefinition = (*GitCheckoutTool)(nil)

// NewGitCheckoutTool returns a GitCheckoutTool backed by the given GitTool,
// which must be non-nil.
func NewGitCheckoutTool(git GitTool) *GitCheckoutTool {
	return &GitCheckoutTool{git: git}
}

// Name returns the tool name.
func (t *GitCheckoutTool) Name() string { return "git_checkout" }

// Description returns a brief description of the tool.
func (t *GitCheckoutTool) Description() string {
	return "git_checkout: Switch to a different git branch. [REQUIRES APPROVAL] This can discard uncommitted changes."
}

// Parameters returns the JSON Schema describing the tool's input parameters.
func (t *GitCheckoutTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"branch": map[string]any{
				"type":        "string",
				"description": "The name of the branch to checkout.",
			},
		},
		"required":             []string{"branch"},
		"additionalProperties": false,
	}
}

// Execute runs git checkout to switch to the named branch.
func (t *GitCheckoutTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	if t.git == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("git_checkout: nil git tool")
	}

	branch, ok := call.Args["branch"].(string)
	if !ok || strings.TrimSpace(branch) == "" {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_checkout.missing_branch")
		return nil, errors.New("git_checkout: missing string argument 'branch'")
	}

	if err := t.git.Checkout(ctx, branch); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_checkout.failed", "branch", branch, "err", err)
		return nil, err
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("git_checkout.done", "branch", branch)

	return &ToolResult{
		Output:     "switched to branch: " + branch,
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"branch": branch,
		},
	}, nil
}
