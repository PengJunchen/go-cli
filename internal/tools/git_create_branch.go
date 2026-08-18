package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// GitCreateBranchTool exposes git branch creation through the ToolDefinition
// interface. It creates a new branch from an optional base.
type GitCreateBranchTool struct {
	git GitTool
}

var _ ToolDefinition = (*GitCreateBranchTool)(nil)

// NewGitCreateBranchTool returns a GitCreateBranchTool backed by the given
// GitTool, which must be non-nil.
func NewGitCreateBranchTool(git GitTool) *GitCreateBranchTool {
	return &GitCreateBranchTool{git: git}
}

// Name returns the tool name.
func (t *GitCreateBranchTool) Name() string { return "git_create_branch" }

// Description returns a brief description of the tool.
func (t *GitCreateBranchTool) Description() string {
	return "git_create_branch: Create a new git branch from an optional base branch or commit."
}

// Parameters returns the JSON Schema describing the tool's input parameters.
func (t *GitCreateBranchTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "The name of the new branch.",
			},
			"base": map[string]any{
				"type":        "string",
				"description": "The base branch or commit to create from. Defaults to HEAD.",
			},
		},
		"required":             []string{"name"},
		"additionalProperties": false,
	}
}

// Execute runs git branch to create a new branch.
func (t *GitCreateBranchTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	if t.git == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("git_create_branch: nil git tool")
	}

	name, ok := call.Args["name"].(string)
	if !ok || strings.TrimSpace(name) == "" {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_create_branch.missing_name")
		return nil, errors.New("git_create_branch: missing string argument 'name'")
	}

	base := ""
	if v, ok := call.Args["base"].(string); ok {
		base = v
	}

	if err := t.git.CreateBranch(ctx, name, base); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_create_branch.failed", "name", name, "base", base, "err", err)
		return nil, err
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("git_create_branch.done", "name", name, "base", base)

	return &ToolResult{
		Output:     "created branch: " + name,
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"name": name,
			"base": base,
		},
	}, nil
}
