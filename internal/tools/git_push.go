package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// GitPushTool exposes git push through the ToolDefinition interface. This is a
// destructive operation that pushes commits to a remote repository and requires
// approval before execution. The force flag triggers an extra warning.
type GitPushTool struct {
	git GitTool
}

var _ ToolDefinition = (*GitPushTool)(nil)

// NewGitPushTool returns a GitPushTool backed by the given GitTool, which must
// be non-nil.
func NewGitPushTool(git GitTool) *GitPushTool {
	return &GitPushTool{git: git}
}

// Name returns the tool name.
func (t *GitPushTool) Name() string { return "git_push" }

// Description returns a brief description of the tool.
func (t *GitPushTool) Description() string {
	return "git_push: Push commits to a remote repository. [REQUIRES APPROVAL] Destructive operation. Use force=true to force-push (overwrites remote history)."
}

// Parameters returns the JSON Schema describing the tool's input parameters.
func (t *GitPushTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"remote": map[string]any{
				"type":        "string",
				"description": "The remote name (e.g. \"origin\").",
			},
			"branch": map[string]any{
				"type":        "string",
				"description": "The branch to push.",
			},
			"force": map[string]any{
				"type":        "boolean",
				"description": "WARNING: Force-push overwrites remote history. Use with extreme caution.",
			},
		},
		"required":             []string{"remote", "branch"},
		"additionalProperties": false,
	}
}

// Execute runs git push to push the named branch to the named remote.
func (t *GitPushTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	if t.git == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("git_push: nil git tool")
	}

	remote, ok := call.Args["remote"].(string)
	if !ok || strings.TrimSpace(remote) == "" {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_push.missing_remote")
		return nil, errors.New("git_push: missing string argument 'remote'")
	}

	branch, ok := call.Args["branch"].(string)
	if !ok || strings.TrimSpace(branch) == "" {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_push.missing_branch")
		return nil, errors.New("git_push: missing string argument 'branch'")
	}

	force := false
	if v, ok := call.Args["force"].(bool); ok {
		force = v
	}

	if force {
		logger.Warn("git_push.force_warning", "remote", remote, "branch", branch)
	}

	if err := t.git.Push(ctx, remote, branch, force); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_push.failed", "remote", remote, "branch", branch, "force", force, "err", err)
		return nil, err
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("git_push.done", "remote", remote, "branch", branch, "force", force)

	output := "pushed " + branch + " to " + remote
	if force {
		output += " (forced)"
	}

	return &ToolResult{
		Output:     output,
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"remote": remote,
			"branch": branch,
			"force":  force,
		},
	}, nil
}
