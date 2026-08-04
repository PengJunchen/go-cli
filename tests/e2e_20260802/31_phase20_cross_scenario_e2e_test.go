//go:build e2e

// Package e2e_20260802 contains end-to-end integration tests.
// This file verifies Phase 20 cross-scenario integration: system prompt +
// git tools, token estimation + git, and SSE parser + git.
package e2e_20260802

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// TestET_Phase20_CrossScenario exercises multiple Phase 20 features together
// to verify they integrate correctly. Real components are used throughout.
func TestET_Phase20_CrossScenario(t *testing.T) {
	// AC-1: System Prompt with project context + Git tools.
	t.Run("AC1_SystemPromptWithGitTools", func(t *testing.T) {
		ctx := context.Background()
		dir := setupPhase20GitRepo(t)

		// Create AGENTS.md with project context.
		agentsContent := "# Project Rules\nUse Go conventions.\nRun tests before committing."
		phase20WriteFile(t, dir, "AGENTS.md", agentsContent)

		// Create a source file in the repo.
		phase20WriteFile(t, dir, "main.go", "package main\n")

		// Load project context.
		loader := core.NewDefaultProjectContextLoader()
		files, err := loader.Load(ctx, dir)
		require.NoError(t, err)

		// Build git tools.
		git := tools.NewDefaultGitTool(dir)
		gitTools := []tools.ToolDefinition{
			tools.NewGitStatusTool(git),
			tools.NewGitDiffTool(git),
			tools.NewGitCommitTool(git),
		}

		// Build system prompt with context files and git tools.
		builder := core.NewDefaultSystemPromptBuilder()
		prompt := builder.Build(ctx, core.SystemPromptOptions{
			Cwd:          dir,
			Tools:        gitTools,
			ContextFiles: files,
		})

		// Verify AGENTS.md content is in the prompt.
		assert.Contains(t, prompt, agentsContent, "system prompt must contain AGENTS.md content")

		// Verify git tool names are in the prompt.
		assert.Contains(t, prompt, "git_status", "system prompt must list git_status tool")
		assert.Contains(t, prompt, "git_diff", "system prompt must list git_diff tool")
		assert.Contains(t, prompt, "git_commit", "system prompt must list git_commit tool")
	})

	// AC-2: Token estimation + Git.
	t.Run("AC2_TokenEstimationWithGit", func(t *testing.T) {
		ctx := context.Background()
		dir := setupPhase20GitRepo(t)

		// Create a Chinese text file.
		chineseText := "你好世界，这是一个测试文件。Hello World."
		phase20WriteFile(t, dir, "chinese.txt", chineseText)

		// Use UnicodeTokenEstimator to estimate tokens for the file content.
		est := compaction.NewUnicodeTokenEstimator()
		n, err := est.Estimate(chineseText)
		require.NoError(t, err)
		assert.Greater(t, n, 0, "token estimate for non-empty text must be > 0")

		// Use GitStatusTool to check the file is untracked.
		git := tools.NewDefaultGitTool(dir)
		statusTool := tools.NewGitStatusTool(git)
		res, err := statusTool.Execute(ctx, tools.ToolCall{
			ID: "tc-1", Name: "git_status", Args: map[string]any{},
		})
		require.NoError(t, err)
		assert.Contains(t, res.Output, "untracked", "git status should report chinese.txt as untracked")
		assert.Contains(t, res.Output, "chinese.txt", "git status should list chinese.txt")
	})

	// AC-3: SSE Parser + Git - run concurrently and verify independent results.
	t.Run("AC3_SSEParserWithGit", func(t *testing.T) {
		ctx := context.Background()
		dir := setupPhase20GitRepo(t)
		phase20WriteFile(t, dir, "file.txt", "content")

		// Construct an SSE stream.
		sseInput := phase20SSEEvent("message_start", `{"type":"message_start"}`) +
			phase20SSEEvent("content_block_delta", `{"type":"content_block_delta"}`)

		parser := llm.NewDefaultSSEParser()

		// Set up git status tool.
		git := tools.NewDefaultGitTool(dir)
		statusTool := tools.NewGitStatusTool(git)

		var events []llm.SSEEvent
		var sseErr error
		var gitRes *tools.ToolResult
		var gitErr error

		var wg sync.WaitGroup
		wg.Add(2)

		// Parse SSE stream concurrently.
		go func() {
			defer wg.Done()
			ch, err := parser.Parse(strings.NewReader(sseInput))
			sseErr = err
			if err == nil {
				for e := range ch {
					events = append(events, e)
				}
			}
		}()

		// Run git status concurrently.
		go func() {
			defer wg.Done()
			gitRes, gitErr = statusTool.Execute(ctx, tools.ToolCall{
				ID: "tc-1", Name: "git_status", Args: map[string]any{},
			})
		}()

		wg.Wait()

		// Verify SSE parsing results.
		require.NoError(t, sseErr, "SSE Parse should not error")
		require.Len(t, events, 2, "SSE parser should emit 2 events")
		assert.Equal(t, "message_start", events[0].Type)
		assert.Equal(t, "content_block_delta", events[1].Type)

		// Verify git status results.
		require.NoError(t, gitErr, "git status should not error")
		require.NotNil(t, gitRes)
		assert.Contains(t, gitRes.Output, "untracked", "git status should report file.txt as untracked")
		assert.Contains(t, gitRes.Output, "file.txt", "git status should list file.txt")
	})
}
