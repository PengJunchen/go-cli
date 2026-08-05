package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// GitBlameTool exposes git blame through the ToolDefinition interface. It
// returns line-by-line attribution for a file range.
type GitBlameTool struct {
	git GitTool
}

var _ ToolDefinition = (*GitBlameTool)(nil)

// NewGitBlameTool returns a GitBlameTool backed by the given GitTool, which
// must be non-nil.
func NewGitBlameTool(git GitTool) *GitBlameTool {
	return &GitBlameTool{git: git}
}

// Name returns the tool name.
func (t *GitBlameTool) Name() string { return "git_blame" }

// Description returns a brief description of the tool.
func (t *GitBlameTool) Description() string {
	return "git_blame: Show line-by-line author attribution for a file. Returns hash, author, date, and content per line."
}

// Parameters returns the JSON Schema describing the tool's input parameters.
func (t *GitBlameTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file": map[string]any{
				"type":        "string",
				"description": "The file to blame.",
			},
			"start_line": map[string]any{
				"type":        "integer",
				"description": "Starting line number (1-based). Defaults to 1.",
			},
			"end_line": map[string]any{
				"type":        "integer",
				"description": "Ending line number. Defaults to start_line.",
			},
		},
		"required":             []string{"file"},
		"additionalProperties": false,
	}
}

// Execute runs git blame and formats the result as text.
func (t *GitBlameTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	if t.git == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("git_blame: nil git tool")
	}

	file, ok := call.Args["file"].(string)
	if !ok || strings.TrimSpace(file) == "" {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_blame.missing_file")
		return nil, errors.New("git_blame: missing string argument 'file'")
	}

	startLine := 1
	endLine := 0
	if v, ok := call.Args["start_line"].(float64); ok {
		startLine = int(v)
	}
	if v, ok := call.Args["end_line"].(float64); ok {
		endLine = int(v)
	}
	if endLine < startLine {
		endLine = startLine
	}

	lines, err := t.git.Blame(ctx, file, startLine, endLine)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_blame.failed", "file", file, "err", err)
		return nil, err
	}

	output := formatBlameLines(lines)
	if strings.TrimSpace(output) == "" {
		output = "(no blame data)"
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("git_blame.done", "file", file, "lines", len(lines))

	return &ToolResult{
		Output:     output,
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"file":  file,
			"lines": len(lines),
		},
	}, nil
}

// formatBlameLines renders a list of GitBlameLine values as text lines.
func formatBlameLines(lines []GitBlameLine) string {
	var sb strings.Builder
	for _, l := range lines {
		hash := l.Hash
		if len(hash) > 8 {
			hash = hash[:8]
		}
		sb.WriteString(fmt.Sprintf("%s  %d  %s <%s>  %s\n    %s\n", hash, l.LineNum, l.Author, l.Email, l.Date, l.Content))
	}
	return strings.TrimSuffix(sb.String(), "\n")
}
