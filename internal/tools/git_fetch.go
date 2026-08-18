package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// GitFetchTool exposes git fetch through the ToolDefinition interface. It
// downloads objects and refs from a remote repository.
type GitFetchTool struct {
	git GitTool
}

var _ ToolDefinition = (*GitFetchTool)(nil)

// NewGitFetchTool returns a GitFetchTool backed by the given GitTool, which
// must be non-nil.
func NewGitFetchTool(git GitTool) *GitFetchTool {
	return &GitFetchTool{git: git}
}

// Name returns the tool name.
func (t *GitFetchTool) Name() string { return "git_fetch" }

// Description returns a brief description of the tool.
func (t *GitFetchTool) Description() string {
	return "git_fetch: Download objects and refs from a remote repository."
}

// Parameters returns the JSON Schema describing the tool's input parameters.
func (t *GitFetchTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"remote": map[string]any{
				"type":        "string",
				"description": "The remote name (e.g. \"origin\"). Defaults to origin.",
			},
		},
		"additionalProperties": false,
	}
}

// Execute runs git fetch to download objects from the named remote.
func (t *GitFetchTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	if t.git == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("git_fetch: nil git tool")
	}

	remote := ""
	if v, ok := call.Args["remote"].(string); ok {
		remote = v
	}

	if err := t.git.Fetch(ctx, remote); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_fetch.failed", "remote", remote, "err", err)
		return nil, err
	}

	resolved := remote
	if strings.TrimSpace(resolved) == "" {
		resolved = "origin"
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("git_fetch.done", "remote", resolved)

	return &ToolResult{
		Output:     "fetched from " + resolved,
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"remote": resolved,
		},
	}, nil
}
