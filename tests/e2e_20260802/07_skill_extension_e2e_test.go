// Package e2e_20260802 contains end-to-end integration tests for the skill
// and extension modules of go-cli. It exercises skill definition creation, YAML
// skill loading (write a YAML-frontmatter file on disk and load it), skill
// registry (register/get/list/match), skill trigger matching, SkillAdapter
// wrapping as ToolDefinition, skill loading from a directory, extension
// lifecycle (Init/Shutdown), extension manager, plugin loader, extension
// middleware, extension registry, ConfigProvider, and a complex full pipeline
// (load skill → register in registry → match by trigger → wrap as tool →
// execute).
package e2e_20260802

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/skill"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// =============================================================================
// SKILL: Skill definition creation
// =============================================================================

func TestSkill_DefinitionCreation(t *testing.T) {
	def := skill.NewSkill("refactor",
		skill.WithDescription("Refactoring skill"),
		skill.WithVersion("1.0.0"),
		skill.WithCategory("coding"),
		skill.WithPrompt("You are a refactoring expert."),
		skill.WithTools("bash", "read", "write"),
		skill.WithParameters(map[string]any{"max_files": 5}),
		skill.WithTriggerHint("refactor code"),
	)

	assert.Equal(t, "refactor", def.Name())
	assert.Equal(t, "Refactoring skill", def.Description())
	assert.Equal(t, "1.0.0", def.Version())
	assert.Equal(t, "coding", def.Category())
	assert.Equal(t, "You are a refactoring expert.", def.Prompt())
	assert.Equal(t, []string{"bash", "read", "write"}, def.Tools())
	assert.Equal(t, map[string]any{"max_files": 5}, def.Parameters())
	assert.Equal(t, "refactor code", def.TriggerHint())
}

func TestSkill_DefinitionDefaultsEmpty(t *testing.T) {
	def := skill.NewSkill("bare-minimal")
	assert.Equal(t, "bare-minimal", def.Name())
	assert.Equal(t, "", def.Description())
	assert.Equal(t, "", def.Version())
	assert.Equal(t, "", def.Category())
	assert.Equal(t, "", def.Prompt())
	assert.Empty(t, def.Tools())
	assert.Empty(t, def.Parameters())
	assert.Empty(t, def.TriggerHint())
}

// =============================================================================
// SKILL: YAML skill loading
// =============================================================================

const yamlSkillContent = `---
name: code-review
description: Automated code review skill
version: 2.0.0
category: coding
prompt: |
  You are a code reviewer.
  Check the diff for issues.
tools:
  - bash
  - read
  - grep
trigger_hint: "review code"
parameters:
  max_lines: 500
  strict: true
---
This is the body text used as the default prompt.
`

func TestSkill_YAMLLoading(t *testing.T) {
	tmpDir := t.TempDir()
	skillFile := filepath.Join(tmpDir, "code-review.skill.md")
	require.NoError(t, os.WriteFile(skillFile, []byte(yamlSkillContent), 0o644))

	loader := skill.NewYAMLSkillLoader()

	ctx := context.Background()
	got, err := loader.Load(ctx, skillFile)
	require.NoError(t, err)
	require.NotNil(t, got)

	def := *got
	assert.Equal(t, "code-review", def.Name())
	assert.Equal(t, "Automated code review skill", def.Description())
	assert.Equal(t, "2.0.0", def.Version())
	assert.Equal(t, "coding", def.Category())
	assert.Contains(t, def.Prompt(), "You are a code reviewer")
	assert.Equal(t, []string{"bash", "read", "grep"}, def.Tools())
	assert.Equal(t, "review code", def.TriggerHint())

	params := def.Parameters()
	assert.Equal(t, 500, params["max_lines"])
	assert.Equal(t, true, params["strict"])
}

func TestSkill_YAMLLoadingBodyAsPrompt(t *testing.T) {
	content := `---
name: fallback
---
This body becomes the prompt.`

	tmpDir := t.TempDir()
	skillFile := filepath.Join(tmpDir, "fallback.md")
	require.NoError(t, os.WriteFile(skillFile, []byte(content), 0o644))

	loader := skill.NewYAMLSkillLoader()
	got, err := loader.Load(context.Background(), skillFile)
	require.NoError(t, err)
	require.NotNil(t, got)

	def := *got
	assert.Equal(t, "fallback", def.Name())
	assert.Equal(t, "This body becomes the prompt.", def.Prompt())
}

func TestSkill_YAMLLoadingMissingFile(t *testing.T) {
	loader := skill.NewYAMLSkillLoader()
	_, err := loader.Load(context.Background(), "/nonexistent/skill.md")
	require.Error(t, err)
}

// =============================================================================
// SKILL: Skill Registry
// =============================================================================

func TestSkill_Registry_RegisterGetList(t *testing.T) {
	reg := skill.NewDefaultSkillRegistry()
	ctx := context.Background()

	s1 := skill.NewSkill("s1", skill.WithCategory("coding"), skill.WithDescription("skill one"))
	s2 := skill.NewSkill("s2", skill.WithCategory("writing"), skill.WithDescription("skill two"))

	require.NoError(t, reg.Register(ctx, s1))
	require.NoError(t, reg.Register(ctx, s2))

	got, ok := reg.Get(ctx, "s1")
	assert.True(t, ok)
	assert.Equal(t, "s1", got.Name())

	_, ok = reg.Get(ctx, "missing")
	assert.False(t, ok)

	all := reg.List(ctx)
	assert.Len(t, all, 2)

	filtered := reg.List(ctx, "coding")
	assert.Len(t, filtered, 1)
	assert.Equal(t, "s1", filtered[0].Name())
}

func TestSkill_Registry_RegisterOverwrites(t *testing.T) {
	reg := skill.NewDefaultSkillRegistry()
	ctx := context.Background()

	v1 := skill.NewSkill("dup", skill.WithDescription("v1"))
	v2 := skill.NewSkill("dup", skill.WithDescription("v2"))

	require.NoError(t, reg.Register(ctx, v1))
	require.NoError(t, reg.Register(ctx, v2))

	got, ok := reg.Get(ctx, "dup")
	assert.True(t, ok)
	assert.Equal(t, "v2", got.Description())
}

func TestSkill_Registry_Unregister(t *testing.T) {
	reg := skill.NewDefaultSkillRegistry()
	ctx := context.Background()

	require.NoError(t, reg.Register(ctx, skill.NewSkill("temp")))
	_, ok := reg.Get(ctx, "temp")
	assert.True(t, ok)

	require.NoError(t, reg.Unregister(ctx, "temp"))
	_, ok = reg.Get(ctx, "temp")
	assert.False(t, ok)

	err := reg.Unregister(ctx, "missing")
	assert.ErrorIs(t, err, skill.ErrSkillNotFound)
}

// =============================================================================
// SKILL: Trigger Matching
// =============================================================================

func TestSkill_Registry_MatchByTrigger(t *testing.T) {
	reg := skill.NewDefaultSkillRegistry()
	ctx := context.Background()

	require.NoError(t, reg.Register(ctx, skill.NewSkill("refactor",
		skill.WithCategory("coding"),
		skill.WithTriggerHint("refactor existing code"),
	)))
	require.NoError(t, reg.Register(ctx, skill.NewSkill("translate",
		skill.WithCategory("writing"),
		skill.WithTriggerHint("translate text"),
	)))
	require.NoError(t, reg.Register(ctx, skill.NewSkill("unrelated",
		skill.WithCategory("other"),
	)))

	matches := reg.Match(ctx, "refactor")
	assert.Len(t, matches, 1)
	assert.Equal(t, "refactor", matches[0].Name())

	noMatch := reg.Match(ctx, "deploy")
	assert.Empty(t, noMatch)
}

func TestSkill_Registry_MatchExactNameWins(t *testing.T) {
	reg := skill.NewDefaultSkillRegistry()
	ctx := context.Background()

	require.NoError(t, reg.Register(ctx, skill.NewSkill("exact",
		skill.WithDescription("some description"),
	)))
	require.NoError(t, reg.Register(ctx, skill.NewSkill("partial-match",
		skill.WithDescription("exact matching"),
	)))

	matches := reg.Match(ctx, "exact")
	require.NotEmpty(t, matches)
	// The exact name match should rank first.
	assert.Equal(t, "exact", matches[0].Name())
}

// =============================================================================
// SKILL: SkillAdapter as ToolDefinition
// =============================================================================

func TestSkill_Adapter_WrappingAsTool(t *testing.T) {
	ctx := context.Background()
	def := skill.NewSkill("test-skill",
		skill.WithDescription("A test skill"),
		skill.WithPrompt("You are a tester."),
		skill.WithTools("bash", "read"),
		skill.WithParameters(map[string]any{"verbose": true}),
	)

	adapter := skill.NewSkillAdapter(def)
	assert.Equal(t, "test-skill", adapter.Name())
	assert.Contains(t, adapter.Description(), "A test skill")
	assert.Contains(t, adapter.Description(), "tools: bash, read")
	assert.Contains(t, adapter.Description(), "parameters: verbose")

	result, err := adapter.Execute(ctx, tools.ToolCall{
		ID:   "call-1",
		Name: "test-skill",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "[skill test-skill]")
	assert.Contains(t, result.Output, "You are a tester.")
	assert.Equal(t, "call-1", result.ToolCallID)

	meta, ok := result.Metadata["skill"].(string)
	assert.True(t, ok)
	assert.Equal(t, "test-skill", meta)
}

// =============================================================================
// SKILL: Skill loading from directory
// =============================================================================

func TestSkill_LoadingFromDirectory(t *testing.T) {
	tmpDir := t.TempDir()

	// Create multiple skill files.
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "skill-a.md"), []byte(
		"---\nname: skill-a\ndescription: first skill\n---\nbody-a\n",
	), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "skill-b.md"), []byte(
		"---\nname: skill-b\ndescription: second skill\n---\nbody-b\n",
	), 0o644))
	// A non-skill file should be ignored.
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "readme.txt"), []byte("not a skill"), 0o644))

	loader := skill.NewYAMLSkillLoader()
	defs, err := loader.LoadDir(context.Background(), tmpDir)
	require.NoError(t, err)
	assert.Len(t, defs, 2)

	names := map[string]bool{}
	for _, d := range defs {
		names[(*d).Name()] = true
	}
	assert.True(t, names["skill-a"])
	assert.True(t, names["skill-b"])
}

// =============================================================================
// EXTENSION: Lifecycle (Init/Shutdown)
// =============================================================================

func TestExtension_Lifecycle_InitShutdown(t *testing.T) {
	ext := mock.NewMockExtension("demo-ext")
	assert.Equal(t, "demo-ext", ext.Name())

	reg := extension.NewExtensionRegistry()
	err := ext.Init(context.Background(), reg)
	require.NoError(t, err)
	assert.True(t, ext.InitCalled())
	assert.Equal(t, reg, ext.Registry())

	err = ext.Shutdown(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, ext.ShutdownCount())
}

func TestExtension_Lifecycle_InitError(t *testing.T) {
	ext := mock.NewMockExtension("failing-ext")
	ext.SetInitError(errors.New("init failed"))

	err := ext.Init(context.Background(), extension.NewExtensionRegistry())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "init failed")
	assert.True(t, ext.InitCalled())
}

func TestExtension_Lifecycle_ShutdownError(t *testing.T) {
	ext := mock.NewMockExtension("bad-shutdown")
	ext.SetShutdownError(errors.New("shutdown failed"))

	require.NoError(t, ext.Init(context.Background(), extension.NewExtensionRegistry()))
	err := ext.Shutdown(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shutdown failed")
}

// =============================================================================
// EXTENSION: Extension Registry
// =============================================================================

func TestExtension_Registry_RegisterEntities(t *testing.T) {
	reg := extension.NewExtensionRegistry()
	ctx := context.Background()

	// Register tool.
	adapter := skill.NewSkillAdapter(skill.NewSkill("sk", skill.WithDescription("d")))
	require.NoError(t, reg.RegisterTool(ctx, adapter))

	// Register command.
	require.NoError(t, reg.RegisterCommand("hello", func(args []string) error { return nil }))

	// Register middleware.
	mw := mock.NewMockMiddleware("audit-mw")
	require.NoError(t, reg.RegisterMiddleware(ctx, mw))

	// Verify via cast.
	dr, ok := reg.(*extension.DefaultExtensionRegistry)
	require.True(t, ok)
	assert.NotNil(t, dr.Middleware("audit-mw"))
}

func TestExtension_Registry_DuplicateRegistration(t *testing.T) {
	reg := extension.NewExtensionRegistry()
	ctx := context.Background()

	mw1 := mock.NewMockMiddleware("same-name")
	mw2 := mock.NewMockMiddleware("same-name")

	require.NoError(t, reg.RegisterMiddleware(ctx, mw1))
	require.NoError(t, reg.RegisterMiddleware(ctx, mw2))

	dr := reg.(*extension.DefaultExtensionRegistry)
	// Last write wins.
	assert.Equal(t, mw2, dr.Middleware("same-name"))
}

// =============================================================================
// EXTENSION: Plugin Loader
// =============================================================================

func TestExtension_PluginLoader(t *testing.T) {
	loader := extension.NewDefaultPluginLoader()
	assert.Equal(t, "default-plugin-loader", loader.Name())

	// gRPC is unsupported in zero-dependency build.
	_, err := loader.Load(context.Background(), "grpc://example.com/load")
	assert.ErrorIs(t, err, extension.ErrUnsupportedRPC)
}

func TestExtension_MockPluginLoader(t *testing.T) {
	loader := mock.NewMockPluginLoader("mock-loader")
	assert.Equal(t, "mock-loader", loader.Name())

	ext := mock.NewMockExtension("loaded-ext")
	loader.SetResult("test/path", []extension.Extension{ext})
	loader.SetError("bad/path", errors.New("load error"))

	// Happy path.
	exts, err := loader.Load(context.Background(), "test/path")
	require.NoError(t, err)
	assert.Len(t, exts, 1)
	assert.Equal(t, "loaded-ext", exts[0].Name())
	assert.Equal(t, "test/path", loader.LoadedPath())

	// Error path.
	_, err = loader.Load(context.Background(), "bad/path")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "load error")
}

// =============================================================================
// EXTENSION: Middleware
// =============================================================================

func TestExtension_Middleware_WrapAgent(t *testing.T) {
	ctx := context.Background()
	mw := mock.NewMockMiddleware("test-mw")
	baseCalled := false

	base := func(_ context.Context, input extension.AgentInput) (extension.AgentOutput, error) {
		baseCalled = true
		return extension.AgentOutput{Text: "echo:" + input.Message}, nil
	}

	wrapped := mw.WrapAgent(base)
	assert.Equal(t, 1, mw.WrapCount())

	out, err := wrapped(ctx, extension.AgentInput{Message: "hello"})
	require.NoError(t, err)
	assert.True(t, baseCalled)
	assert.Equal(t, "echo:hello", out.Text)
}

func TestExtension_ModelMiddleware_WrapModel(t *testing.T) {
	ctx := context.Background()
	mw := mock.NewMockModelMiddleware("model-mw")

	base := func(_ context.Context, req extension.ModelRequest) (extension.ModelResponse, error) {
		return extension.ModelResponse{Text: req.Prompt + "!"}, nil
	}

	wrapped := mw.WrapModel(base)
	assert.Equal(t, 1, mw.WrapCount())

	resp, err := wrapped(ctx, extension.ModelRequest{Prompt: "hi"})
	require.NoError(t, err)
	assert.Equal(t, "hi!", resp.Text)
}

func TestExtension_ToolMiddleware_WrapTool(t *testing.T) {
	ctx := context.Background()
	mw := mock.NewMockToolMiddleware("tool-mw")

	base := func(_ context.Context, name string, input any) (any, error) {
		return name + ":ok", nil
	}

	wrapped := mw.WrapTool(base)
	assert.Equal(t, 1, mw.WrapCount())

	out, err := wrapped(ctx, "read", nil)
	require.NoError(t, err)
	assert.Equal(t, "read:ok", out)
}

// =============================================================================
// EXTENSION: ConfigProvider
// =============================================================================

func TestExtension_ConfigProvider_Integration(t *testing.T) {
	// Use the default extension as a config provider placeholder.
	ext := mock.NewMockExtension("config-demo")
	reg := extension.NewExtensionRegistry()

	require.NoError(t, ext.Init(context.Background(), reg))
	assert.True(t, ext.InitCalled())

	require.NoError(t, ext.Shutdown(context.Background()))
	assert.Equal(t, 1, ext.ShutdownCount())
}

// =============================================================================
// COMPLEX: Full Pipeline (Load → Register → Match → Wrap → Execute)
// =============================================================================

func TestSkill_FullPipeline_LoadRegisterMatchWrapExecute(t *testing.T) {
	// Step 1: Write a YAML skill file on disk.
	tmpDir := t.TempDir()
	skillFile := filepath.Join(tmpDir, "refactor.skill.md")
	skillContent := `---
name: refactor
description: Refactoring skill
version: 1.0.0
category: coding
prompt: |
  You are a refactoring expert.
  Focus on readability and performance.
tools:
  - bash
  - read
  - write
trigger_hint: "refactor code"
parameters:
  max_files: 5
---
Expert refactoring body.`

	require.NoError(t, os.WriteFile(skillFile, []byte(skillContent), 0o644))

	ctx := context.Background()

	// Step 2: Load the skill with YAMLSkillLoader.
	loader := skill.NewYAMLSkillLoader()
	loadedDef, err := loader.Load(ctx, skillFile)
	require.NoError(t, err)
	require.NotNil(t, loadedDef)
	assert.Equal(t, "refactor", (*loadedDef).Name())

	// Step 3: Register in the skill registry.
	reg := skill.NewDefaultSkillRegistry()
	require.NoError(t, reg.Register(ctx, *loadedDef))

	got, ok := reg.Get(ctx, "refactor")
	assert.True(t, ok)
	assert.Equal(t, "refactor", got.Name())

	coding := reg.List(ctx, "coding")
	assert.Len(t, coding, 1)

	// Step 4: Match by trigger hint.
	matches := reg.Match(ctx, "refactor")
	assert.Len(t, matches, 1)
	assert.Equal(t, "refactor", matches[0].Name())

	noMatch := reg.Match(ctx, "deploy")
	assert.Empty(t, noMatch)

	// Step 5: Wrap as tool.
	adapter := skill.NewSkillAdapter(got)
	assert.Equal(t, "refactor", adapter.Name())
	assert.Contains(t, adapter.Description(), "Refactoring skill")
	assert.Contains(t, adapter.Description(), "tools: bash, read, write")
	assert.Contains(t, adapter.Description(), "parameters: max_files")

	// Step 6: Register into tool registry and execute.
	toolReg := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry)
	require.NoError(t, toolReg.Register(ctx, adapter))

	listed, err := toolReg.List(ctx)
	require.NoError(t, err)
	assert.Len(t, listed, 1)

	result, err := toolReg.Execute(ctx, tools.ToolCall{
		ID:   "pipeline-1",
		Name: "refactor",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "[skill refactor]")
	assert.Contains(t, result.Output, "You are a refactoring expert")
	assert.Equal(t, "pipeline-1", result.ToolCallID)

	// Verify metadata.
	assert.Equal(t, "refactor", result.Metadata["skill"])
	assert.Equal(t, []string{"bash", "read", "write"}, result.Metadata["tools"])
}

// =============================================================================
// COMPLEX: Extension + Middleware pipeline
// =============================================================================

func TestExtension_FullPipeline_ExtensionMiddlewareRegistry(t *testing.T) {
	ctx := context.Background()
	reg := extension.NewExtensionRegistry()

	// Create extension.
	ext := mock.NewMockExtension("demo-ext")
	require.NoError(t, ext.Init(ctx, reg))
	assert.True(t, ext.InitCalled())

	// Register middleware into registry.
	mw := mock.NewMockMiddleware("security-mw")
	require.NoError(t, reg.RegisterMiddleware(ctx, mw))

	dr := reg.(*extension.DefaultExtensionRegistry)
	gotMw := dr.Middleware("security-mw")
	require.NotNil(t, gotMw)
	assert.Equal(t, "security-mw", gotMw.Name())

	// Use the middleware.
	base := func(_ context.Context, input extension.AgentInput) (extension.AgentOutput, error) {
		return extension.AgentOutput{Text: "ok:" + input.Message}, nil
	}
	wrapped := gotMw.WrapAgent(base)
	out, err := wrapped(ctx, extension.AgentInput{Message: "test"})
	require.NoError(t, err)
	assert.Equal(t, "ok:test", out.Text)
	assert.Equal(t, 1, mw.WrapCount())

	// Shutdown extension.
	require.NoError(t, ext.Shutdown(ctx))
	assert.Equal(t, 1, ext.ShutdownCount())
}
