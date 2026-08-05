package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// GitBranchTool exposes git branch through the ToolDefinition interface. It
// returns the list of local and remote branches.
type GitBranchTool struct {
	git GitTool
}

var _ ToolDefinition = (*GitBranchTool)(nil)

// NewGitBranchTool returns a GitBranchTool backed by the given GitTool, which
// must be non-nil.
func NewGitBranchTool(git GitTool) *GitBranchTool {
	return &GitBranchTool{git: git}
}

// Name returns the tool name.
func (t *GitBranchTool) Name() string { return "git_branch" }

// Description returns a brief description of the tool.
func (t *GitBranchTool) Description() string {
	return "git_branch: List git branches (local and remote). Marks the current branch."
}

// Parameters returns an empty schema; git_branch takes no parameters.
func (t *GitBranchTool) Parameters() any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": false,
	}
}

// Execute runs git branch and formats the result as text.
func (t *GitBranchTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	if t.git == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("git_branch: nil git tool")
	}

	branches, err := t.git.Branch(ctx)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_branch.failed", "err", err)
		return nil, err
	}

	output := formatBranches(branches)
	if strings.TrimSpace(output) == "" {
		output = "(no branches)"
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("git_branch.done", "branches", len(branches))

	return &ToolResult{
		Output:     output,
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"branches": len(branches),
		},
	}, nil
}

// formatBranches renders a list of GitBranch values as text lines.
func formatBranches(branches []GitBranch) string {
	var sb strings.Builder
	for _, b := range branches {
		marker := "  "
		if b.Current {
			marker = "* "
		}
		scope := "local"
		if b.Remote {
			scope = "remote"
		}
		sb.WriteString(fmt.Sprintf("%s[%s] %s\n", marker, scope, b.Name))
	}
	return strings.TrimSuffix(sb.String(), "\n")
}
