package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// GitStatusTool exposes git status through the ToolDefinition interface. It
// returns the list of changed files with their status.
type GitStatusTool struct {
	git GitTool
}

var _ ToolDefinition = (*GitStatusTool)(nil)

// NewGitStatusTool returns a GitStatusTool backed by the given GitTool, which
// must be non-nil.
func NewGitStatusTool(git GitTool) *GitStatusTool {
	return &GitStatusTool{git: git}
}

// Name returns the tool name.
func (t *GitStatusTool) Name() string { return "git_status" }

// Description returns a brief description of the tool.
func (t *GitStatusTool) Description() string {
	return "git_status: Show git working tree status. Returns list of changed files with their status."
}

// Parameters returns an empty schema; git_status takes no parameters.
func (t *GitStatusTool) Parameters() any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": false,
	}
}

// Execute runs git status and formats the result as text.
func (t *GitStatusTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	if t.git == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("git_status: nil git tool")
	}

	files, err := t.git.Status(ctx)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_status.failed", "err", err)
		return nil, err
	}

	output := formatFileStatuses(files)
	if strings.TrimSpace(output) == "" {
		output = "(clean working tree)"
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("git_status.done", "files", len(files))

	return &ToolResult{
		Output:     output,
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"files": len(files),
		},
	}, nil
}

// formatFileStatuses renders a list of GitFileStatus entries as text lines.
func formatFileStatuses(files []GitFileStatus) string {
	var sb strings.Builder
	for _, f := range files {
		scope := "unstaged"
		if f.Staged {
			scope = "staged"
		}
		sb.WriteString(fmt.Sprintf("[%s] %s: %s\n", scope, f.Status, f.File))
	}
	return strings.TrimSuffix(sb.String(), "\n")
}
