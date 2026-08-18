package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// GitRemoteTool exposes git remote through the ToolDefinition interface. It
// lists the configured remote repositories.
type GitRemoteTool struct {
	git GitTool
}

var _ ToolDefinition = (*GitRemoteTool)(nil)

// NewGitRemoteTool returns a GitRemoteTool backed by the given GitTool, which
// must be non-nil.
func NewGitRemoteTool(git GitTool) *GitRemoteTool {
	return &GitRemoteTool{git: git}
}

// Name returns the tool name.
func (t *GitRemoteTool) Name() string { return "git_remote" }

// Description returns a brief description of the tool.
func (t *GitRemoteTool) Description() string {
	return "git_remote: List configured remote repositories with their URLs."
}

// Parameters returns an empty schema; git_remote takes no parameters.
func (t *GitRemoteTool) Parameters() any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": false,
	}
}

// Execute runs git remote -v and formats the result as text.
func (t *GitRemoteTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	if t.git == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("git_remote: nil git tool")
	}

	remotes, err := t.git.Remote(ctx)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_remote.failed", "err", err)
		return nil, err
	}

	output := formatRemotes(remotes)
	if strings.TrimSpace(output) == "" {
		output = "(no remotes)"
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("git_remote.done", "remotes", len(remotes))

	return &ToolResult{
		Output:     output,
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"remotes": len(remotes),
		},
	}, nil
}

// formatRemotes renders a list of RemoteInfo values as text lines.
func formatRemotes(remotes []RemoteInfo) string {
	var sb strings.Builder
	for _, r := range remotes {
		sb.WriteString(fmt.Sprintf("%s\t%s (%s)\n", r.Name, r.URL, r.Type))
	}
	return strings.TrimSuffix(sb.String(), "\n")
}
