package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// GitLogTool exposes git log through the ToolDefinition interface. It returns
// structured commit history entries.
type GitLogTool struct {
	git GitTool
}

var _ ToolDefinition = (*GitLogTool)(nil)

// NewGitLogTool returns a GitLogTool backed by the given GitTool, which must
// be non-nil.
func NewGitLogTool(git GitTool) *GitLogTool {
	return &GitLogTool{git: git}
}

// Name returns the tool name.
func (t *GitLogTool) Name() string { return "git_log" }

// Description returns a brief description of the tool.
func (t *GitLogTool) Description() string {
	return "git_log: Show git commit history. Returns structured log entries with hash, author, date, and message."
}

// Parameters returns the JSON Schema describing the tool's input parameters.
func (t *GitLogTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"max_count": map[string]any{
				"type":        "integer",
				"description": "Maximum number of commits to return.",
			},
			"since": map[string]any{
				"type":        "string",
				"description": "Show commits more recent than this date (e.g. \"2024-01-01\").",
			},
			"until": map[string]any{
				"type":        "string",
				"description": "Show commits older than this date.",
			},
			"author": map[string]any{
				"type":        "string",
				"description": "Filter commits by author name or email.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "Restrict to commits touching this file path.",
			},
		},
		"additionalProperties": false,
	}
}

// Execute runs git log with the supplied options and returns formatted entries.
func (t *GitLogTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	if t.git == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("git_log: nil git tool")
	}

	opts := GitLogOptions{}
	if v, ok := call.Args["max_count"].(float64); ok {
		opts.MaxCount = int(v)
	}
	if v, ok := call.Args["since"].(string); ok {
		opts.Since = v
	}
	if v, ok := call.Args["until"].(string); ok {
		opts.Until = v
	}
	if v, ok := call.Args["author"].(string); ok {
		opts.Author = v
	}
	if v, ok := call.Args["path"].(string); ok {
		opts.Path = v
	}

	entries, err := t.git.Log(ctx, opts)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_log.failed", "err", err)
		return nil, err
	}

	output := formatLogEntries(entries)
	if strings.TrimSpace(output) == "" {
		output = "(no commits)"
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("git_log.done", "entries", len(entries))

	return &ToolResult{
		Output:     output,
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"entries": len(entries),
		},
	}, nil
}

// formatLogEntries renders a list of GitLogEntry values as text lines.
func formatLogEntries(entries []GitLogEntry) string {
	var sb strings.Builder
	for _, e := range entries {
		sb.WriteString(fmt.Sprintf("%s %s <%s> %s\n    %s\n", e.Hash[:min(8, len(e.Hash))], e.Author, e.Email, e.Date, e.Message))
	}
	return strings.TrimSuffix(sb.String(), "\n")
}
