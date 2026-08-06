package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/mcp"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// ---------------------------------------------------------------------------
// Integration tests for MCP auto-discovery, Skill auto-discovery, and
// SkillInfo.Description injection into the system prompt.
// These tests verify the fixes for:
//   1. SkillInfo.Description is populated and rendered in the system prompt.
//   2. Skills are auto-discovered from .go-cli/skills when no dir is configured.
//   3. MCP servers are auto-discovered from .go-cli/mcp.json when none are
//      configured in the main config file.
// ---------------------------------------------------------------------------

// TestIntegration_SkillDescriptionInSystemPrompt verifies that the
// SkillInfo.Description field — newly added to fix the issue where the LLM
// could not see what skills do — is rendered in the system prompt XML.
func TestIntegration_SkillDescriptionInSystemPrompt(t *testing.T) {
	builder := core.NewDefaultSystemPromptBuilder()
	ctx := context.Background()

	skills := []core.SkillInfo{
		{
			Name:        "pdf",
			Description: "Use this skill whenever the user wants to do anything with PDF files",
			Category:    "document",
		},
		{
			Name:        "canvas-design",
			Description: "Create beautiful visual art in .png and .pdf documents",
			Category:    "design",
		},
	}

	prompt := builder.Build(ctx, core.SystemPromptOptions{
		Cwd:    "/tmp/test",
		Skills: skills,
	})

	// The skills XML section must be present.
	assert.Contains(t, prompt, "<skills>")
	assert.Contains(t, prompt, "</skills>")

	// Each skill name must appear.
	assert.Contains(t, prompt, `name="pdf"`)
	assert.Contains(t, prompt, `name="canvas-design"`)

	// The descriptions must be rendered — this is the core fix. Before the
	// fix, only name and category appeared; the LLM had no idea what the
	// skill did.
	assert.Contains(t, prompt, "Use this skill whenever the user wants to do anything with PDF files")
	assert.Contains(t, prompt, "Create beautiful visual art in .png and .pdf documents")
}

// TestIntegration_SkillDescriptionOmittedWhenEmpty verifies that when
// Description is empty, the XML attribute is gracefully omitted (no
// description="" appears in the output).
func TestIntegration_SkillDescriptionOmittedWhenEmpty(t *testing.T) {
	builder := core.NewDefaultSystemPromptBuilder()
	ctx := context.Background()

	skills := []core.SkillInfo{
		{Name: "no-desc", Category: "misc"},
	}

	prompt := builder.Build(ctx, core.SystemPromptOptions{
		Cwd:    "/tmp/test",
		Skills: skills,
	})

	assert.Contains(t, prompt, `name="no-desc"`)
	assert.NotContains(t, prompt, `description=`)
}

// TestIntegration_SkillAutoDiscovery verifies that registerSkillTools
// auto-discovers skills from .go-cli/skills when no skill.dir is configured.
// This tests the fix for the issue where skills were silently skipped when
// the config didn't explicitly set skill.dir.
func TestIntegration_SkillAutoDiscovery(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()

	// Create the .go-cli/skills directory structure.
	skillDir := filepath.Join(tempDir, ".go-cli", "skills")
	require.NoError(t, os.MkdirAll(skillDir, 0o750))

	// Create a nested skill (SKILL.md inside a directory).
	nestedDir := filepath.Join(skillDir, "auto-discovered-skill")
	require.NoError(t, os.MkdirAll(nestedDir, 0o750))
	skillContent := `---
name: auto-discovered-skill
description: A skill that should be auto-discovered from .go-cli/skills
version: "1.0"
category: test
prompt: Auto-discovered skill prompt
---
This skill was auto-discovered.
`
	require.NoError(t, os.WriteFile(
		filepath.Join(nestedDir, "SKILL.md"),
		[]byte(skillContent), 0o600,
	))

	// Create a flat skill (.md file directly in skills dir).
	flatContent := `---
name: flat-skill
description: A flat-layout skill auto-discovered from .go-cli/skills
category: test
prompt: Flat skill prompt
---
Flat skill body.
`
	require.NoError(t, os.WriteFile(
		filepath.Join(skillDir, "flat-skill.md"),
		[]byte(flatContent), 0o600,
	))

	// Change to the temp dir so discoverSkillDir finds .go-cli/skills.
	t.Chdir(tempDir)

	tr := tools.NewDefaultToolRegistry()

	// Pass a config with no skill.dir — the auto-discovery should kick in.
	rc := &config.Config{}
	infos := registerSkillTools(ctx, rc, tr)

	require.Len(t, infos, 2, "both auto-discovered skills should be registered")

	// Verify SkillInfo includes Description (the fix).
	var foundAuto, foundFlat bool
	for _, info := range infos {
		switch info.Name {
		case "auto-discovered-skill":
			foundAuto = true
			assert.Equal(t, "test", info.Category)
			assert.Contains(t, info.Description, "auto-discovered")
		case "flat-skill":
			foundFlat = true
			assert.Contains(t, info.Description, "flat-layout")
		}
	}
	assert.True(t, foundAuto, "auto-discovered-skill must be registered")
	assert.True(t, foundFlat, "flat-skill must be registered")

	// Verify tools are in the registry.
	for _, name := range []string{"auto-discovered-skill", "flat-skill"} {
		tool, err := tr.Get(ctx, name)
		require.NoError(t, err)
		assert.NotNil(t, tool)
	}
}

// TestIntegration_SkillAutoDiscoveryNotFound verifies that when no
// .go-cli/skills directory exists and no skill.dir is configured,
// registerSkillTools returns nil without error.
func TestIntegration_SkillAutoDiscoveryNotFound(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	tr := tools.NewDefaultToolRegistry()
	rc := &config.Config{}
	infos := registerSkillTools(ctx, rc, tr)

	assert.Empty(t, infos, "no skills should be discovered in an empty temp dir")

	registered, err := tr.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, registered)
}

// TestIntegration_MCPAutoDiscovery_ArrayFormat verifies that MCP servers are
// auto-discovered from .go-cli/mcp.json when the main config has no servers.
// This tests the "servers" array format.
func TestIntegration_MCPAutoDiscovery_ArrayFormat(t *testing.T) {
	ctx := context.Background()

	// Start a mock MCP HTTP server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			//nolint:errcheck // test HTTP response
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":0,"result":{"tools":[{"name":"search","description":"Search the web"}]}}`)) //nolint:errcheck
		} else {
			//nolint:errcheck // test HTTP response
			_, _ = w.Write([]byte(`{}`)) //nolint:errcheck
		}
	}))
	defer srv.Close()

	tempDir := t.TempDir()
	mcpDir := filepath.Join(tempDir, ".go-cli")
	require.NoError(t, os.MkdirAll(mcpDir, 0o750))

	// Write .go-cli/mcp.json in "servers" array format.
	mcpConfig := map[string]any{
		"servers": []map[string]any{
			{"name": "web-search", "url": srv.URL},
		},
	}
	data, err := json.Marshal(mcpConfig)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(mcpDir, "mcp.json"), data, 0o600))

	t.Chdir(tempDir)

	tr := tools.NewDefaultToolRegistry()

	// Pass a config with no MCP servers — auto-discovery should find mcp.json.
	rc := &config.Config{}
	require.NoError(t, registerMCPTools(ctx, rc, tr))

	// The tool from the auto-discovered MCP server should be registered.
	tool, err := tr.Get(ctx, mcp.NormalizeToolName("web-search", "search"))
	require.NoError(t, err)
	assert.NotNil(t, tool)
	assert.Equal(t, "mcp__web-search__search", tool.Name())
	assert.Equal(t, "Search the web", tool.Description())
}

// TestIntegration_MCPAutoDiscovery_MapFormat verifies that MCP servers are
// auto-discovered from .go-cli/mcp.json using the "mcpServers" map format
// (the common format used by Claude Desktop and other tools).
func TestIntegration_MCPAutoDiscovery_MapFormat(t *testing.T) {
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			//nolint:errcheck // test HTTP response
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":0,"result":{"tools":[{"name":"echo","description":"Echo tool"}]}}`)) //nolint:errcheck
		} else {
			//nolint:errcheck // test HTTP response
			_, _ = w.Write([]byte(`{}`)) //nolint:errcheck
		}
	}))
	defer srv.Close()

	tempDir := t.TempDir()
	mcpDir := filepath.Join(tempDir, ".go-cli")
	require.NoError(t, os.MkdirAll(mcpDir, 0o750))

	// Write .go-cli/mcp.json in "mcpServers" map format.
	mcpConfig := map[string]any{
		"mcpServers": map[string]any{
			"echo-server": map[string]any{
				"url": srv.URL,
			},
		},
	}
	data, err := json.Marshal(mcpConfig)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(mcpDir, "mcp.json"), data, 0o600))

	t.Chdir(tempDir)

	tr := tools.NewDefaultToolRegistry()
	rc := &config.Config{}
	require.NoError(t, registerMCPTools(ctx, rc, tr))

	tool, err := tr.Get(ctx, mcp.NormalizeToolName("echo-server", "echo"))
	require.NoError(t, err)
	assert.NotNil(t, tool)
	assert.Equal(t, "mcp__echo-server__echo", tool.Name())
}

// TestIntegration_MCPConfigOverridesAutoDiscovery verifies that MCP servers
// configured in the main config file take priority over auto-discovered ones.
func TestIntegration_MCPConfigOverridesAutoDiscovery(t *testing.T) {
	ctx := context.Background()

	// Two mock servers: one for config, one for auto-discovery.
	configSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			//nolint:errcheck // test HTTP response
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":0,"result":{"tools":[{"name":"config-tool","description":"From config"}]}}`)) //nolint:errcheck
		} else {
			//nolint:errcheck // test HTTP response
			_, _ = w.Write([]byte(`{}`)) //nolint:errcheck
		}
	}))
	defer configSrv.Close()

	autoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			//nolint:errcheck // test HTTP response
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":0,"result":{"tools":[{"name":"auto-tool","description":"From auto-discovery"}]}}`)) //nolint:errcheck
		} else {
			//nolint:errcheck // test HTTP response
			_, _ = w.Write([]byte(`{}`)) //nolint:errcheck
		}
	}))
	defer autoSrv.Close()

	tempDir := t.TempDir()
	mcpDir := filepath.Join(tempDir, ".go-cli")
	require.NoError(t, os.MkdirAll(mcpDir, 0o750))

	// Write .go-cli/mcp.json that would be auto-discovered.
	mcpConfig := map[string]any{
		"servers": []map[string]any{
			{"name": "auto-server", "url": autoSrv.URL},
		},
	}
	data, _ := json.Marshal(mcpConfig) //nolint:errcheck
	require.NoError(t, os.WriteFile(filepath.Join(mcpDir, "mcp.json"), data, 0o600))

	t.Chdir(tempDir)

	tr := tools.NewDefaultToolRegistry()

	// Config has its own MCP server — should take priority over mcp.json.
	rc := &config.Config{
		MCP: config.MCPConfig{
			Servers: []config.MCPServerConfig{
				{Name: "config-server", URL: configSrv.URL},
			},
		},
	}
	require.NoError(t, registerMCPTools(ctx, rc, tr))

	// Config server's tool should be registered.
	tool, err := tr.Get(ctx, mcp.NormalizeToolName("config-server", "config-tool"))
	require.NoError(t, err)
	assert.NotNil(t, tool)

	// Auto-discovered server's tool should NOT be registered.
	_, err = tr.Get(ctx, mcp.NormalizeToolName("auto-server", "auto-tool"))
	assert.Error(t, err, "auto-discovered server should not be used when config has servers")
}

// TestIntegration_LoadMCPServers_NoFiles verifies loadMCPServers returns nil
// when no config and no auto-discovery files exist.
func TestIntegration_LoadMCPServers_NoFiles(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	servers := loadMCPServers(nil)
	assert.Empty(t, servers)

	servers = loadMCPServers(&config.Config{})
	assert.Empty(t, servers)
}

// TestIntegration_FullSystemPromptWithAutoDiscoveredSkills is the most
// end-to-end test: it creates a .go-cli/skills directory with a real skill,
// auto-discovers it via registerSkillTools, then feeds the resulting
// SkillInfo into the DefaultSystemPromptBuilder to verify the full pipeline
// produces a system prompt containing the skill name and description.
func TestIntegration_FullSystemPromptWithAutoDiscoveredSkills(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()

	// Set up .go-cli/skills with a realistic skill.
	skillDir := filepath.Join(tempDir, ".go-cli", "skills", "my-integration-skill")
	require.NoError(t, os.MkdirAll(skillDir, 0o750))

	skillContent := `---
name: my-integration-skill
description: Handles end-to-end integration testing of the skill pipeline
version: "1.0"
category: testing
prompt: Run integration tests and verify all assertions pass
---
This skill guides integration test execution.
`
	require.NoError(t, os.WriteFile(
		filepath.Join(skillDir, "SKILL.md"),
		[]byte(skillContent), 0o600,
	))

	t.Chdir(tempDir)

	// Step 1: Auto-discover and register skills (no skill.dir in config).
	tr := tools.NewDefaultToolRegistry()
	rc := &config.Config{}
	infos := registerSkillTools(ctx, rc, tr)
	require.Len(t, infos, 1)

	// Step 2: Build the system prompt using the discovered SkillInfo.
	builder := core.NewDefaultSystemPromptBuilder()
	prompt := builder.Build(ctx, core.SystemPromptOptions{
		Cwd:    tempDir,
		Skills: infos,
	})

	// Step 3: Verify the full pipeline output.
	// The skill name must be in the prompt.
	assert.Contains(t, prompt, "my-integration-skill",
		"skill name must appear in the system prompt")

	// The description must be in the prompt — this is the core fix.
	assert.Contains(t, prompt, "Handles end-to-end integration testing of the skill pipeline",
		"skill description must appear in the system prompt")

	// The skills XML section must be properly formed.
	assert.Contains(t, prompt, "<skills>")
	assert.Contains(t, prompt, "</skills>")

	// Verify the tool is also registered in the tool registry so the LLM
	// can actually invoke it.
	tool, err := tr.Get(ctx, "my-integration-skill")
	require.NoError(t, err)
	assert.NotNil(t, tool)
	assert.Contains(t, tool.Description(), "end-to-end integration testing")

	// Step 4: Verify the tool list in the system prompt includes the skill.
	toolList, err := tr.List(ctx)
	require.NoError(t, err)
	var foundInTools bool
	for _, td := range toolList {
		if td.Name() == "my-integration-skill" {
			foundInTools = true
			break
		}
	}
	assert.True(t, foundInTools, "skill must be registered as a tool")
	assert.True(t, strings.Contains(prompt, "my-integration-skill"),
		"system prompt must reference the skill tool name")
}
