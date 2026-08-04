package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// GitCommitTool exposes git commit through the ToolDefinition interface. It
// stages (optionally) and commits changes, requiring a commit message.
type GitCommitTool struct {
	git GitTool
}

var _ ToolDefinition = (*GitCommitTool)(nil)

// NewGitCommitTool returns a GitCommitTool backed by the given GitTool, which
// must be non-nil.
func NewGitCommitTool(git GitTool) *GitCommitTool {
	return &GitCommitTool{git: git}
}

// Name returns the tool name.
func (t *GitCommitTool) Name() string { return "git_commit" }

// Description returns a brief description of the tool.
func (t *GitCommitTool) Description() string {
	return "git_commit: Stage and commit changes to git. Requires a commit message."
}

// Parameters returns the JSON Schema describing the tool's input parameters.
func (t *GitCommitTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"message": map[string]any{
				"type":        "string",
				"description": "The commit message.",
			},
			"add_all": map[string]any{
				"type":        "boolean",
				"description": "If true, run `git add -A` before committing.",
			},
		},
		"required":             []string{"message"},
		"additionalProperties": false,
	}
}

// Execute stages (optionally) and commits changes, returning the commit hash.
func (t *GitCommitTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	if t.git == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("git_commit: nil git tool")
	}

	message, ok := call.Args["message"].(string)
	if !ok || strings.TrimSpace(message) == "" {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_commit.missing_message")
		return nil, errors.New("git_commit: missing string argument 'message'")
	}

	opts := GitCommitOptions{Message: message}
	if v, ok := call.Args["add_all"].(bool); ok {
		opts.AddAll = v
	}

	hash, err := t.git.Commit(ctx, opts)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_commit.failed", "err", err)
		return nil, err
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("git_commit.done", "hash", hash)

	return &ToolResult{
		Output:     fmt.Sprintf("committed: %s", hash),
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"hash":    hash,
			"add_all": opts.AddAll,
		},
	}, nil
}
