package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// GitPullTool exposes git pull through the ToolDefinition interface. It fetches
// from and integrates with a remote repository.
type GitPullTool struct {
	git GitTool
}

var _ ToolDefinition = (*GitPullTool)(nil)

// NewGitPullTool returns a GitPullTool backed by the given GitTool, which must
// be non-nil.
func NewGitPullTool(git GitTool) *GitPullTool {
	return &GitPullTool{git: git}
}

// Name returns the tool name.
func (t *GitPullTool) Name() string { return "git_pull" }

// Description returns a brief description of the tool.
func (t *GitPullTool) Description() string {
	return "git_pull: Fetch from and integrate with a remote repository."
}

// Parameters returns the JSON Schema describing the tool's input parameters.
func (t *GitPullTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"remote": map[string]any{
				"type":        "string",
				"description": "The remote name (e.g. \"origin\"). Defaults to origin.",
			},
			"branch": map[string]any{
				"type":        "string",
				"description": "The branch to pull. Defaults to the current branch.",
			},
		},
		"additionalProperties": false,
	}
}

// Execute runs git pull to fetch and integrate changes.
func (t *GitPullTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	if t.git == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("git_pull: nil git tool")
	}

	remote := ""
	if v, ok := call.Args["remote"].(string); ok {
		remote = v
	}
	branch := ""
	if v, ok := call.Args["branch"].(string); ok {
		branch = v
	}

	if err := t.git.Pull(ctx, remote, branch); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_pull.failed", "remote", remote, "branch", branch, "err", err)
		return nil, err
	}

	resolved := remote
	if strings.TrimSpace(resolved) == "" {
		resolved = "origin"
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("git_pull.done", "remote", resolved, "branch", branch)

	return &ToolResult{
		Output:     "pulled from " + resolved,
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"remote": resolved,
			"branch": branch,
		},
	}, nil
}
