package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// GitPRCreateTool exposes `gh pr create` through the ToolDefinition interface.
// It creates a pull request on GitHub via the gh CLI. This is a destructive
// operation that requires approval before execution.
type GitPRCreateTool struct {
	cwd string
}

var _ ToolDefinition = (*GitPRCreateTool)(nil)

// NewGitPRCreateTool returns a GitPRCreateTool that runs gh in cwd.
func NewGitPRCreateTool(cwd string) *GitPRCreateTool {
	return &GitPRCreateTool{cwd: cwd}
}

// Name returns the tool name.
func (t *GitPRCreateTool) Name() string { return "git_pr_create" }

// Description returns a brief description of the tool.
func (t *GitPRCreateTool) Description() string {
	return "git_pr_create: Create a pull request via gh CLI. [REQUIRES APPROVAL] Requires gh CLI installed and authenticated."
}

// Parameters returns the JSON Schema describing the tool's input parameters.
func (t *GitPRCreateTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{
				"type":        "string",
				"description": "The title of the pull request.",
			},
			"body": map[string]any{
				"type":        "string",
				"description": "The body description of the pull request.",
			},
			"base": map[string]any{
				"type":        "string",
				"description": "The base branch to merge into (e.g. \"main\").",
			},
			"head": map[string]any{
				"type":        "string",
				"description": "The head branch to merge from (e.g. \"feature-branch\").",
			},
		},
		"required":             []string{"title", "base", "head"},
		"additionalProperties": false,
	}
}

// Execute runs `gh pr create` to create a pull request and returns the PR URL.
func (t *GitPRCreateTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	title, ok := call.Args["title"].(string)
	if !ok || strings.TrimSpace(title) == "" {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("git_pr_create: missing string argument 'title'")
	}
	base, ok := call.Args["base"].(string)
	if !ok || strings.TrimSpace(base) == "" {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("git_pr_create: missing string argument 'base'")
	}
	head, ok := call.Args["head"].(string)
	if !ok || strings.TrimSpace(head) == "" {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("git_pr_create: missing string argument 'head'")
	}
	body := ""
	if v, ok := call.Args["body"].(string); ok {
		body = v
	}

	// Check that gh CLI is installed.
	if _, err := exec.LookPath("gh"); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_pr_create.gh_not_found", "err", err)
		return nil, errors.New("git_pr_create: gh CLI is not installed. Install it from https://cli.github.com/")
	}

	args := []string{"pr", "create", "--title", title, "--base", base, "--head", head}
	if strings.TrimSpace(body) != "" {
		args = append(args, "--body", body)
	}

	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = t.cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("git_pr_create.failed", "title", title, "base", base, "head", head, "err", err, "output", string(out))
		return nil, fmt.Errorf("git_pr_create: %w: %s", err, strings.TrimSpace(string(out)))
	}

	prURL := strings.TrimSpace(string(out))

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("git_pr_create.done", "title", title, "base", base, "head", head, "url", prURL)

	return &ToolResult{
		Output:     "pull request created: " + prURL,
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"title": title,
			"base":  base,
			"head":  head,
			"url":   prURL,
		},
	}, nil
}
