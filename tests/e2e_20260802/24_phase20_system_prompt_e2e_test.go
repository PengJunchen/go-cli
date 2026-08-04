//go:build e2e

// Package e2e_20260802 contains end-to-end integration tests.
// This file verifies Phase 20 system prompt assembly: AGENTS.md loading,
// tool guidelines, custom/append prompts, skills XML, date/cwd suffix, and
// AgentAssembly wiring.
package e2e_20260802

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// TestET_Phase20_SystemPrompt exercises the system prompt builder and project
// context loader end-to-end using the real filesystem (t.TempDir) and real
// DefaultSystemPromptBuilder / DefaultProjectContextLoader. No mocks are used.
func TestET_Phase20_SystemPrompt(t *testing.T) {
	ctx := context.Background()
	builder := core.NewDefaultSystemPromptBuilder()
	loader := core.NewDefaultProjectContextLoader()

	// AC-1: AGENTS.md in cwd is loaded by DefaultProjectContextLoader.
	t.Run("AC1_AGENTSMD_Loaded", func(t *testing.T) {
		dir := t.TempDir()
		content := "Use Go conventions"
		require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(content), 0644))

		files, err := loader.Load(ctx, dir)
		require.NoError(t, err)

		var found bool
		for _, f := range files {
			if f.Content == content {
				found = true
				break
			}
		}
		assert.True(t, found, "expected a ContextFile containing %q", content)
	})

	// AC-2: Nested parent/child directories each with AGENTS.md -> both loaded,
	// child appears after parent (higher priority).
	t.Run("AC2_NestedDirectories_ChildAfterParent", func(t *testing.T) {
		root := t.TempDir()
		parentDir := filepath.Join(root, "parent")
		childDir := filepath.Join(parentDir, "child")
		require.NoError(t, os.MkdirAll(childDir, 0755))

		parentContent := "parent context"
		childContent := "child context"
		require.NoError(t, os.WriteFile(filepath.Join(parentDir, "AGENTS.md"), []byte(parentContent), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(childDir, "AGENTS.md"), []byte(childContent), 0644))

		files, err := loader.Load(ctx, childDir)
		require.NoError(t, err)

		parentIdx, childIdx := -1, -1
		for i, f := range files {
			if f.Content == parentContent {
				parentIdx = i
			}
			if f.Content == childContent {
				childIdx = i
			}
		}
		require.NotEqual(t, -1, parentIdx, "parent AGENTS.md must be loaded")
		require.NotEqual(t, -1, childIdx, "child AGENTS.md must be loaded")
		assert.Less(t, parentIdx, childIdx, "parent must appear before child (child has higher priority)")
	})

	// AC-3: Tools implementing PromptGuideliner contribute guidelines to the
	// system prompt.
	t.Run("AC3_ToolGuidelines", func(t *testing.T) {
		readTool := tools.NewReadTool()
		prompt := builder.Build(ctx, core.SystemPromptOptions{
			Cwd:   "/tmp/test",
			Tools: []tools.ToolDefinition{readTool},
		})
		assert.Contains(t, prompt, "Tool guidelines:")
		assert.Contains(t, prompt, "Use read to examine files instead of cat or sed",
			"system prompt must contain read tool guideline text")
	})

	// AC-4: CustomPrompt replaces the base prompt entirely.
	t.Run("AC4_CustomPromptReplacesBase", func(t *testing.T) {
		custom := "You are a custom assistant for testing."
		prompt := builder.Build(ctx, core.SystemPromptOptions{
			Cwd:          "/tmp/test",
			CustomPrompt: custom,
		})
		assert.Contains(t, prompt, custom, "custom prompt must be present")
		assert.NotContains(t, prompt, "You are a helpful AI assistant embedded in a developer CLI",
			"default base prompt must be replaced entirely when CustomPrompt is set")
	})

	// AC-5: AppendPrompt appears at the end of the system prompt.
	t.Run("AC5_AppendPromptAtEnd", func(t *testing.T) {
		appendText := "REMEMBER: Always format output as JSON."
		prompt := builder.Build(ctx, core.SystemPromptOptions{
			Cwd:          "/tmp/test",
			AppendPrompt: appendText,
		})
		assert.Contains(t, prompt, appendText, "append prompt must be present")

		// The append prompt must come after the cwd line.
		cwdIdx := strings.LastIndex(prompt, "Current working directory:")
		appendIdx := strings.LastIndex(prompt, appendText)
		assert.Greater(t, appendIdx, cwdIdx, "append prompt must appear after cwd line")

		// The append prompt must be the very last content in the prompt.
		assert.True(t, strings.HasSuffix(prompt, appendText),
			"prompt must end with the append prompt text")
	})

	// AC-6: Skills formatted as XML appear in the system prompt.
	t.Run("AC6_SkillsXML", func(t *testing.T) {
		skills := []core.SkillInfo{
			{Name: "code-review", Category: "development"},
			{Name: "debug", Category: "development"},
		}
		prompt := builder.Build(ctx, core.SystemPromptOptions{
			Cwd:    "/tmp/test",
			Skills: skills,
		})
		assert.Contains(t, prompt, "<skills>", "skills XML section must be present")
		assert.Contains(t, prompt, "</skills>", "skills XML section must be closed")
		assert.Contains(t, prompt, `name="code-review"`, "first skill must appear in XML")
		assert.Contains(t, prompt, `name="debug"`, "second skill must appear in XML")
	})

	// AC-7: System prompt ends with "Current date:" and "Current working directory:".
	t.Run("AC7_EndsWithDateAndCwd", func(t *testing.T) {
		cwd := "/tmp/e2e-test-cwd"
		prompt := builder.Build(ctx, core.SystemPromptOptions{
			Cwd: cwd,
		})
		assert.Contains(t, prompt, "Current date:", "prompt must contain current date label")
		assert.Contains(t, prompt, "Current working directory:", "prompt must contain cwd label")
		assert.Contains(t, prompt, cwd, "prompt must contain the actual cwd value")
		// Without AppendPrompt, the prompt ends with the cwd value.
		assert.True(t, strings.HasSuffix(prompt, cwd),
			"prompt must end with the cwd value when no append prompt is set")
	})

	// AC-8: AgentAssembly has PromptBuilder and ContextLoader fields that are
	// non-nil after AssembleAgent.
	t.Run("AC8_AssemblyPromptBuilderAndContextLoader", func(t *testing.T) {
		assembly := phase19wAssemble(t, phase19wTestConfig())
		require.NotNil(t, assembly.PromptBuilder, "PromptBuilder must be non-nil after AssembleAgent")
		require.NotNil(t, assembly.ContextLoader, "ContextLoader must be non-nil after AssembleAgent")
	})
}
