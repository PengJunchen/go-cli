package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// GitMergeTool exposes git merge through the ToolDefinition interface. This is
// a destructive operation that can produce merge conflicts and requires
// approval before execution.
type GitMergeTool struct {
	git GitTool
}

var _ ToolDefinition = (*GitMergeTool)(nil)

// NewGitMergeTool returns a GitMergeTool backed by the given GitTool, which
// must be non-nil.
func NewGitMergeTool(git GitTool) *GitMergeTool {
	return &GitMergeTool{git: git}
}

// Name returns the tool name.
func (t *GitMergeTool) Name() string { return "git_merge" }

// Description returns a brief description of the tool.
func (t *GitMergeTool) Description() string {
	return "git_merge: Merge a branch into the current branch. [REQUIRES APPROVAL] May produce merge conflicts."
}

// Parameters returns the JSON Schema describing the tool's input parameters.
func (t *GitMergeTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"branch": map[string]any{
				"type":        "string",
				"description": "The branch to merge into the current branch.",
			},
		},
		"required":             []string{"branch"},
		"additionalProperties": false,
	}
}

// Execute runs git merge and reports conflicts when they occur.
func (t *GitMergeTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	if t.git == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("git_merge: nil git tool")
	}

	branch, ok := call.Args["branch"].(string)
	if !ok || strings.TrimSpace(branch) == "" {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_merge.missing_branch")
		return nil, errors.New("git_merge: missing string argument 'branch'")
	}

	result, err := t.git.Merge(ctx, branch)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_merge.failed", "branch", branch, "err", err)
		return nil, err
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("git_merge.done", "branch", branch, "merge_success", result.Success)

	output := "merged: " + branch
	if !result.Success {
		output = fmt.Sprintf("merge conflicts in %d file(s): %s", len(result.Conflicts), strings.Join(result.Conflicts, ", "))
	}

	return &ToolResult{
		Output:     output,
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"branch":    branch,
			"success":   result.Success,
			"conflicts": result.Conflicts,
		},
	}, nil
}
