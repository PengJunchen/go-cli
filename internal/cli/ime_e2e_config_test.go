package cli

import (
	"context"
	"encoding/json"
	"fmt"
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
// E2E integration tests that mirror the real user scenario:
// .go-cli.yaml is present with MCP and skill configuration. These tests
// verify the full pipeline: config load → tool/skill registration → system
// prompt generation → LLM-visible descriptions.
// ---------------------------------------------------------------------------

// TestE2E_RealConfigScenario_FullPipeline is the master test that replicates
// the user's actual setup: a .go-cli.yaml with mcp.servers and skill.dir,
// a .go-cli/skills/ directory with real skill files, and a mock MCP server.
// It verifies:
//  1. Config loads MCP servers from .go-cli.yaml
//  2. Config loads skill.dir from .go-cli.yaml
//  3. registerMCPTools connects and registers MCP tools
//  4. registerSkillTools loads and registers skills with descriptions
//  5. System prompt contains skill names AND descriptions
//  6. MCP tools are callable from the tool registry
func TestE2E_RealConfigScenario_FullPipeline(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// --- Setup: mock MCP HTTP server ---
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			// Respond to initialize
			if strings.Contains(r.URL.Path, "initialize") || r.Header.Get("Content-Type") != "" {
				body, _ := json.Marshal(map[string]any{ //nolint:errcheck
					"jsonrpc": "2.0",
					"id":      0,
					"result": map[string]any{
						"tools": []map[string]any{
							{"name": "webSearchPro", "description": "Professional web search"},
							{"name": "webSearchStd", "description": "Standard web search"},
						},
					},
				})
				//nolint:errcheck // test HTTP response
				_, _ = w.Write(body) //nolint:errcheck
				return
			}
		}
		//nolint:errcheck // test HTTP response
		_, _ = w.Write([]byte(`{}`)) //nolint:errcheck
	}))
	defer mcpSrv.Close()

	// --- Setup: .go-cli.yaml (mirrors the real config) ---
	yamlContent := fmt.Sprintf(`provider:
  name: mock
  model: test-model
mcp:
  servers:
    - name: zhipu-web-search
      url: "%s"
skill:
  dir: .go-cli/skills
`, mcpSrv.URL)
	require.NoError(t, os.WriteFile(".go-cli.yaml", []byte(yamlContent), 0o600))

	// --- Setup: .go-cli/skills/ with a real skill ---
	skillDir := filepath.Join(".go-cli", "skills", "pdf")
	require.NoError(t, os.MkdirAll(skillDir, 0o750))
	skillContent := `---
name: pdf
description: Use this skill whenever the user wants to do anything with PDF files. This includes reading, merging, splitting, and OCR.
version: "1.0"
category: document
prompt: PDF skill instructions here
---
PDF skill body.
`
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0o600))

	// Also create a second skill (flat layout).
	flatContent := `---
name: docx
description: Create, read, and edit Word documents (.docx)
category: document
prompt: DOCX skill instructions
---
DOCX skill body.
`
	require.NoError(t, os.WriteFile(
		filepath.Join(".go-cli", "skills", "docx.md"),
		[]byte(flatContent), 0o600,
	))

	// --- Step 1: Load config (mirrors what the CLI does) ---
	cfg, err := config.Load()
	require.NoError(t, err, "config must load from .go-cli.yaml")
	require.NotEmpty(t, cfg.MCP.Servers, "MCP servers must be loaded from config")
	require.Equal(t, "zhipu-web-search", cfg.MCP.Servers[0].Name)
	require.Equal(t, ".go-cli/skills", cfg.Skill.Dir, "skill.dir must be loaded from config")

	// --- Step 2: Assemble tools (register MCP + skills) ---
	tr := tools.NewDefaultToolRegistry()

	// 2a. Register MCP tools.
	require.NoError(t, registerMCPTools(ctx, cfg, tr))

	// 2b. Register skill tools.
	skillInfos := registerSkillTools(ctx, cfg, tr)
	require.Len(t, skillInfos, 2, "both skills must be registered")

	// --- Step 3: Verify MCP tools are in the registry ---
	mcpTool, err := tr.Get(ctx, mcp.NormalizeToolName("zhipu-web-search", "webSearchPro"))
	require.NoError(t, err, "MCP tool must be registered")
	assert.NotNil(t, mcpTool)
	assert.Contains(t, mcpTool.Description(), "Professional web search")

	// --- Step 4: Verify skill tools are in the registry ---
	for _, name := range []string{"pdf", "docx"} {
		tool, getErr := tr.Get(ctx, name)
		require.NoError(t, getErr, "skill %s must be registered as a tool", name)
		assert.NotNil(t, tool)
	}

	// --- Step 5: Verify SkillInfo contains descriptions ---
	var pdfInfo, docxInfo *core.SkillInfo
	for i := range skillInfos {
		switch skillInfos[i].Name {
		case "pdf":
			pdfInfo = &skillInfos[i]
		case "docx":
			docxInfo = &skillInfos[i]
		}
	}
	require.NotNil(t, pdfInfo, "pdf SkillInfo must exist")
	require.NotNil(t, docxInfo, "docx SkillInfo must exist")
	assert.Contains(t, pdfInfo.Description, "PDF files")
	assert.Contains(t, docxInfo.Description, "Word documents")

	// --- Step 6: Build system prompt and verify LLM-visible content ---
	builder := core.NewDefaultSystemPromptBuilder()
	prompt := builder.Build(ctx, core.SystemPromptOptions{
		Cwd:    tempDir,
		Skills: skillInfos,
	})

	// Skills section must be present with descriptions.
	assert.Contains(t, prompt, "<skills>")
	assert.Contains(t, prompt, "</skills>")
	assert.Contains(t, prompt, `name="pdf"`)
	assert.Contains(t, prompt, `name="docx"`)
	assert.Contains(t, prompt, "Use this skill whenever the user wants to do anything with PDF files",
		"pdf description must be in system prompt — this is the core fix")
	assert.Contains(t, prompt, "Create, read, and edit Word documents",
		"docx description must be in system prompt")

	// --- Step 7: Verify all tools are listed in the prompt ---
	toolDefs, err := tr.List(ctx)
	require.NoError(t, err)
	var toolNames []string
	for _, td := range toolDefs {
		toolNames = append(toolNames, td.Name())
	}
	// MCP tool name should appear (may be prefixed with mcp__).
	foundMCP := false
	for _, n := range toolNames {
		if strings.Contains(n, "webSearchPro") {
			foundMCP = true
			break
		}
	}
	assert.True(t, foundMCP, "MCP tool must be in tool list")
}

// TestE2E_RealConfigScenario_MCPDescriptionPropagated verifies that the MCP
// tool description from the remote server propagates through to the tool
// registry and is visible in the system prompt's tool listing.
func TestE2E_RealConfigScenario_MCPDescriptionPropagated(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Mock MCP server that returns a tool with a specific description.
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]any{ //nolint:errcheck
			"jsonrpc": "2.0",
			"id":      0,
			"result": map[string]any{
				"tools": []map[string]any{
					{
						"name":        "customSearch",
						"description": "A custom search engine for testing",
					},
				},
			},
		})
		//nolint:errcheck // test HTTP response
		_, _ = w.Write(body) //nolint:errcheck
	}))
	defer mcpSrv.Close()

	yamlContent := fmt.Sprintf(`mcp:
  servers:
    - name: test-server
      url: "%s"
`, mcpSrv.URL)
	require.NoError(t, os.WriteFile(".go-cli.yaml", []byte(yamlContent), 0o600))

	cfg, err := config.Load()
	require.NoError(t, err)
	require.Len(t, cfg.MCP.Servers, 1)

	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, registerMCPTools(ctx, cfg, tr))

	// Verify the MCP tool description propagated.
	tool, err := tr.Get(ctx, mcp.NormalizeToolName("test-server", "customSearch"))
	require.NoError(t, err)
	assert.Equal(t, "A custom search engine for testing", tool.Description())

	// Verify it appears in the system prompt's tool list.
	builder := core.NewDefaultSystemPromptBuilder()
	toolDefs, err := tr.List(ctx)
	require.NoError(t, err)
	prompt := builder.Build(ctx, core.SystemPromptOptions{
		Cwd:   tempDir,
		Tools: toolDefs,
	})
	assert.Contains(t, prompt, "customSearch",
		"MCP tool name must appear in the system prompt tool list")
}

// TestE2E_RealConfigScenario_AutoDiscoveryFallback verifies that when
// .go-cli.yaml has NO mcp/skill config, the auto-discovery kicks in.
// This tests the scenario the user might hit: they have .go-cli/skills/
// and .go-cli/mcp.json but forgot to add them to .go-cli.yaml.
func TestE2E_RealConfigScenario_AutoDiscoveryFallback(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// --- Setup: .go-cli.yaml with NO mcp or skill config ---
	yamlContent := `provider:
  name: mock
  model: test-model
`
	require.NoError(t, os.WriteFile(".go-cli.yaml", []byte(yamlContent), 0o600))

	// --- Setup: .go-cli/skills/ (should be auto-discovered) ---
	skillDir := filepath.Join(".go-cli", "skills", "auto-skill")
	require.NoError(t, os.MkdirAll(skillDir, 0o750))
	skillContent := `---
name: auto-skill
description: Auto-discovered skill without explicit config
category: test
prompt: Auto-discovered
---
Body.
`
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0o600))

	// --- Setup: .go-cli/mcp.json (should be auto-discovered) ---
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]any{ //nolint:errcheck
			"jsonrpc": "2.0",
			"id":      0,
			"result": map[string]any{
				"tools": []map[string]any{
					{"name": "autoTool", "description": "Auto-discovered MCP tool"},
				},
			},
		})
		//nolint:errcheck // test HTTP response
		_, _ = w.Write(body) //nolint:errcheck
	}))
	defer mcpSrv.Close()

	mcpConfig := map[string]any{
		"mcpServers": map[string]any{
			"auto-server": map[string]any{
				"url": mcpSrv.URL,
			},
		},
	}
	mcpData, _ := json.Marshal(mcpConfig) //nolint:errcheck
	require.NoError(t, os.WriteFile(".go-cli/mcp.json", mcpData, 0o600))

	// --- Step 1: Load config (no mcp/skill in config) ---
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.MCP.Servers, "config should have no MCP servers")
	assert.Empty(t, cfg.Skill.Dir, "config should have no skill dir")

	// --- Step 2: Auto-discover and register ---
	tr := tools.NewDefaultToolRegistry()

	// 2a. MCP auto-discovery.
	require.NoError(t, registerMCPTools(ctx, cfg, tr))
	autoTool, err := tr.Get(ctx, mcp.NormalizeToolName("auto-server", "autoTool"))
	require.NoError(t, err, "auto-discovered MCP tool must be registered")
	assert.Contains(t, autoTool.Description(), "Auto-discovered MCP tool")

	// 2b. Skill auto-discovery.
	infos := registerSkillTools(ctx, cfg, tr)
	require.Len(t, infos, 1, "auto-discovered skill must be registered")
	assert.Equal(t, "auto-skill", infos[0].Name)
	assert.Contains(t, infos[0].Description, "Auto-discovered skill")

	// --- Step 3: System prompt includes auto-discovered resources ---
	builder := core.NewDefaultSystemPromptBuilder()
	prompt := builder.Build(ctx, core.SystemPromptOptions{
		Cwd:    tempDir,
		Skills: infos,
	})
	assert.Contains(t, prompt, "auto-skill")
	assert.Contains(t, prompt, "Auto-discovered skill without explicit config")
}

// TestE2E_RealConfigScenario_ConfigWinsOverAutoDiscovery verifies that
// explicit config in .go-cli.yaml takes priority over auto-discovery files.
// When mcp/skill is in .go-cli.yaml, the .go-cli/mcp.json and .go-cli/skills
// auto-discovery paths are NOT consulted for MCP (but skills still use the
// configured dir). This prevents duplicate/conflicting registrations.
func TestE2E_RealConfigScenario_ConfigWinsOverAutoDiscovery(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	// Config server.
	configSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		//nolint:errcheck // test HTTP response
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":0,"result":{"tools":[{"name":"fromConfig","description":"From config"}]}}`)) //nolint:errcheck
	}))
	defer configSrv.Close()

	// Auto-discovery server (should NOT be used).
	autoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		//nolint:errcheck // test HTTP response
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":0,"result":{"tools":[{"name":"fromAuto","description":"From auto"}]}}`)) //nolint:errcheck
	}))
	defer autoSrv.Close()

	// .go-cli.yaml with MCP config.
	yamlContent := fmt.Sprintf(`mcp:
  servers:
    - name: config-server
      url: "%s"
`, configSrv.URL)
	require.NoError(t, os.WriteFile(".go-cli.yaml", []byte(yamlContent), 0o600))

	// .go-cli/mcp.json that would be auto-discovered (but should be ignored).
	require.NoError(t, os.MkdirAll(".go-cli", 0o750))
	autoConfig := map[string]any{
		"servers": []map[string]any{
			{"name": "auto-server", "url": autoSrv.URL},
		},
	}
	autoData, _ := json.Marshal(autoConfig) //nolint:errcheck
	require.NoError(t, os.WriteFile(".go-cli/mcp.json", autoData, 0o600))

	cfg, err := config.Load()
	require.NoError(t, err)

	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, registerMCPTools(ctx, cfg, tr))

	// Config server tool should be registered.
	tool, err := tr.Get(ctx, mcp.NormalizeToolName("config-server", "fromConfig"))
	require.NoError(t, err)
	assert.NotNil(t, tool)

	// Auto-discovered server tool should NOT be registered.
	_, err = tr.Get(ctx, mcp.NormalizeToolName("auto-server", "fromAuto"))
	assert.Error(t, err, "auto-discovered MCP server should be skipped when config has servers")
}
