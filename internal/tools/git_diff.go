package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// GitDiffTool exposes git diff through the ToolDefinition interface. It
// returns the unified diff text for staged or unstaged changes.
type GitDiffTool struct {
	git GitTool
}

var _ ToolDefinition = (*GitDiffTool)(nil)

// NewGitDiffTool returns a GitDiffTool backed by the given GitTool, which must
// be non-nil.
func NewGitDiffTool(git GitTool) *GitDiffTool {
	return &GitDiffTool{git: git}
}

// Name returns the tool name.
func (t *GitDiffTool) Name() string { return "git_diff" }

// Description returns a brief description of the tool.
func (t *GitDiffTool) Description() string {
	return "git_diff: Show git diff for staged or unstaged changes. Returns unified diff text."
}

// Parameters returns the JSON Schema describing the tool's input parameters.
func (t *GitDiffTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"staged": map[string]any{
				"type":        "boolean",
				"description": "If true, show staged (cached) changes; otherwise show unstaged changes.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "Optional file path to restrict the diff to.",
			},
		},
		"additionalProperties": false,
	}
}

// Execute runs git diff with the supplied options and returns the unified diff
// text as the tool result.
func (t *GitDiffTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	if t.git == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("git_diff: nil git tool")
	}

	opts := GitDiffOptions{}
	if v, ok := call.Args["staged"].(bool); ok {
		opts.Staged = v
	}
	if v, ok := call.Args["path"].(string); ok {
		opts.Path = v
	}

	out, err := t.git.Diff(ctx, opts)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_diff.failed", "staged", opts.Staged, "path", opts.Path, "err", err)
		return nil, err
	}

	if strings.TrimSpace(out) == "" {
		out = "(no changes)"
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("git_diff.done", "staged", opts.Staged, "path", opts.Path, "bytes", len(out))

	return &ToolResult{
		Output:     out,
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"staged": opts.Staged,
			"path":   opts.Path,
		},
	}, nil
}
