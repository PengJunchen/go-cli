// Package e2e_20260802 contains end-to-end integration tests for the MCP
// HTTPClientAdapter and Skill loading pipelines. It exercises:
//  1. SSE-style MCP server handshake and tool listing
//  2. Plain JSON MCP server connection and tool listing
//  3. Full MCP tool execution through the tool registry
//  4. Skill loading from a directory with YAML frontmatter
//  5. Combined MCP + Skill pipeline in a single tool registry
package e2e_20260802

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/mcp"
	"github.com/pengjunchen/go-cli/internal/skill"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// ---------------------------------------------------------------------------
// Mock HTTP MCP Server
// ---------------------------------------------------------------------------

// mcpHTTPCallLog records a single tools/call invocation seen by the mock server.
type mcpHTTPCallLog struct {
	ToolName string
	Args     map[string]any
	Result   any
}

// mockHTTPMCPServer is an in-process MCP server that speaks the streamable
// HTTP and SSE transports over net/http/httptest. It supports:
//   - SSE-style handshake (GET returns an "endpoint" event)
//   - Plain JSON mode (no SSE handshake; POST returns raw JSON)
//   - SSE-wrapped POST responses
//
// It also records every tools/call so tests can assert on invocations.
type mockHTTPMCPServer struct {
	t            *testing.T
	sseHandshake bool // GET returns an "event: endpoint" line
	ssePost      bool // POST responses are SSE-formatted

	mu          sync.Mutex
	logs        []mcpHTTPCallLog
	tools       []map[string]any // declared tools for tools/list
	callHandler func(name string, args map[string]any) any
}

func newMockHTTPMCPServer(t *testing.T) *mockHTTPMCPServer {
	return &mockHTTPMCPServer{t: t}
}

func (s *mockHTTPMCPServer) declareTool(name, description string, inputSchema map[string]any) {
	s.tools = append(s.tools, map[string]any{
		"name":        name,
		"description": description,
		"inputSchema": inputSchema,
	})
}

func (s *mockHTTPMCPServer) callLog() []mcpHTTPCallLog {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]mcpHTTPCallLog, len(s.logs))
	copy(out, s.logs)
	return out
}

// handler dispatches GET (SSE handshake) and POST (JSON-RPC) requests.
func (s *mockHTTPMCPServer) handler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleGet(w, r)
	case http.MethodPost:
		s.handlePost(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *mockHTTPMCPServer) handleGet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	if s.sseHandshake {
		// Return an absolute POST-back URL so the adapter uses it directly
		// (it starts with "http" and skips the relative-resolution path).
		postURL := fmt.Sprintf("http://%s/mcp", r.Host)
		fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", postURL)
	}
	// When sseHandshake is false, we write nothing. The adapter's scanner
	// reaches EOF, endpoint stays empty, and falls back to cfg.URL.
}

func (s *mockHTTPMCPServer) handlePost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Errorf("mock mcp server: read body: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	var req struct {
		ID     int64           `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		s.t.Errorf("mock mcp server: decode request: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var result map[string]any
	switch req.Method {
	case "tools/list":
		result = map[string]any{"tools": s.tools}
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			s.t.Errorf("mock mcp server: decode tools/call params: %v", err)
			result = map[string]any{"content": "", "isError": true}
		} else {
			content := s.callTool(params.Name, params.Arguments)
			result = map[string]any{"content": content}
		}
	default:
		result = map[string]any{}
	}

	frame := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
	raw, err := json.Marshal(frame)
	if err != nil {
		s.t.Errorf("mock mcp server: marshal response: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if s.ssePost {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "id: %d\nevent: message\ndata: %s\n\n", req.ID, raw)
	} else {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}
}

func (s *mockHTTPMCPServer) callTool(name string, args map[string]any) any {
	var result any = "ok"
	if s.callHandler != nil {
		result = s.callHandler(name, args)
	}
	s.mu.Lock()
	s.logs = append(s.logs, mcpHTTPCallLog{ToolName: name, Args: args, Result: result})
	s.mu.Unlock()
	return result
}

// start creates and starts an httptest.Server wrapping this handler, returning
// the server for the caller to close on cleanup.
func (s *mockHTTPMCPServer) start() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handler)
	return httptest.NewServer(mux)
}

// =============================================================================
// 1. TestE2E_MCPHTTPAdapter_SSEConnectAndListTools
// =============================================================================

// TestE2E_MCPHTTPAdapter_SSEConnectAndListTools verifies that an
// HTTPClientAdapter can connect to an SSE-style MCP server (GET handshake
// returning an "endpoint" event, SSE-wrapped POST responses), list its tools,
// and have those tools registered into a tools.ToolRegistry via MCPToolAdapter.
func TestE2E_MCPHTTPAdapter_SSEConnectAndListTools(t *testing.T) {
	srv := newMockHTTPMCPServer(t)
	srv.sseHandshake = true // GET returns the endpoint event
	srv.ssePost = true      // POST responses are SSE-formatted
	srv.declareTool("get_weather", "Get the weather for a city", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"city": map[string]any{"type": "string"},
		},
	})
	srv.declareTool("get_time", "Get the current time", map[string]any{
		"type": "object",
	})

	httpSrv := srv.start()
	defer httpSrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	adapter := mcp.NewHTTPClientAdapter(mcp.MCPServerConfig{
		Name:      "weather-sse",
		URL:       httpSrv.URL,
		Transport: mcp.MCPTransportSSE,
	})

	// Connect should consume the SSE handshake and resolve the POST endpoint.
	require.NoError(t, adapter.Connect(ctx), "SSE connect should succeed")

	// List tools should parse both tools from the SSE-wrapped response.
	got, err := adapter.ListTools(ctx)
	require.NoError(t, err)
	require.Len(t, got, 2, "should list both declared tools")
	assert.Equal(t, "get_weather", got[0].Name)
	assert.Equal(t, "Get the weather for a city", got[0].Description)
	assert.Equal(t, "get_time", got[1].Name)
	// ArgsSchema is parsed from the "inputSchema" field.
	require.NotEmpty(t, got[0].ArgsSchema)
	assert.Equal(t, "object", got[0].ArgsSchema["type"])

	// Register each tool through MCPToolAdapter into a ToolRegistry and verify.
	tr := tools.NewDefaultToolRegistry()
	for _, tool := range got {
		adapter2 := mcp.NewMCPToolAdapter(adapter, tool)
		require.NoError(t, tr.Register(ctx, adapter2))
	}

	listed, err := tr.List(ctx)
	require.NoError(t, err)
	assert.Len(t, listed, 2)

	// The registered names are normalized to mcp__{server}__{tool}.
	expectedWeather := mcp.NormalizeToolName("weather-sse", "get_weather")
	expectedTime := mcp.NormalizeToolName("weather-sse", "get_time")
	names := map[string]bool{}
	for _, def := range listed {
		names[def.Name()] = true
	}
	assert.True(t, names[expectedWeather], "registry should contain %s", expectedWeather)
	assert.True(t, names[expectedTime], "registry should contain %s", expectedTime)

	// The adapter is still connected; ensure Disconnect is a no-op-safe call.
	require.NoError(t, adapter.Disconnect(ctx))
}

// =============================================================================
// 2. TestE2E_MCPHTTPAdapter_PlainJSONConnectAndListTools
// =============================================================================

// TestE2E_MCPHTTPAdapter_PlainJSONConnectAndListTools verifies the
// streamable-HTTP path: GET returns no SSE endpoint (the adapter falls back to
// the configured URL) and POST responses are plain JSON.
func TestE2E_MCPHTTPAdapter_PlainJSONConnectAndListTools(t *testing.T) {
	srv := newMockHTTPMCPServer(t)
	srv.sseHandshake = false // no endpoint event → fallback to cfg.URL
	srv.ssePost = false      // plain JSON responses
	srv.declareTool("echo", "Echo back the input", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"msg": map[string]any{"type": "string"},
		},
	})

	httpSrv := srv.start()
	defer httpSrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	adapter := mcp.NewHTTPClientAdapter(mcp.MCPServerConfig{
		Name:      "echo-json",
		URL:       httpSrv.URL,
		Transport: mcp.MCPTransportStreamableHTTP,
	})

	require.NoError(t, adapter.Connect(ctx), "plain JSON connect should succeed")

	got, err := adapter.ListTools(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "echo", got[0].Name)
	assert.Equal(t, "Echo back the input", got[0].Description)
	require.NotEmpty(t, got[0].ArgsSchema)

	// Register via MCPToolAdapter and verify the registry name.
	tr := tools.NewDefaultToolRegistry()
	toolAdapter := mcp.NewMCPToolAdapter(adapter, got[0])
	require.NoError(t, tr.Register(ctx, toolAdapter))
	assert.Equal(t, mcp.NormalizeToolName("echo-json", "echo"), toolAdapter.Name())

	listed, err := tr.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, toolAdapter.Name(), listed[0].Name())
}

// =============================================================================
// 3. TestE2E_MCPHTTPAdapter_CallToolThroughRegistry
// =============================================================================

// TestE2E_MCPHTTPAdapter_CallToolThroughRegistry exercises the full pipeline:
// HTTPClientAdapter → MCPToolAdapter → tools.ToolRegistry → Execute. The mock
// server handles both tools/list and tools/call, and the result must flow
// through the registry's Execute path back to the caller.
func TestE2E_MCPHTTPAdapter_CallToolThroughRegistry(t *testing.T) {
	srv := newMockHTTPMCPServer(t)
	srv.sseHandshake = false
	srv.ssePost = false
	srv.declareTool("calculate", "Run a calculation", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"expression": map[string]any{"type": "string"},
		},
	})
	srv.callHandler = func(name string, args map[string]any) any {
		expr := safeString(args["expression"])
		return fmt.Sprintf("result(%s) = 42", expr)
	}

	httpSrv := srv.start()
	defer httpSrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	adapter := mcp.NewHTTPClientAdapter(mcp.MCPServerConfig{
		Name: "calc-srv",
		URL:  httpSrv.URL,
	})
	require.NoError(t, adapter.Connect(ctx))

	// Discover tools and register the single tool into the registry.
	mcpTools, err := adapter.ListTools(ctx)
	require.NoError(t, err)
	require.Len(t, mcpTools, 1)

	toolAdapter := mcp.NewMCPToolAdapter(adapter, mcpTools[0])
	tr := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry)
	require.NoError(t, tr.Register(ctx, toolAdapter))

	toolName := toolAdapter.Name()
	require.Equal(t, "mcp__calc-srv__calculate", toolName)

	// Execute through the registry convenience method.
	result, err := tr.Execute(ctx, tools.ToolCall{
		ID:   "call-calc-1",
		Name: toolName,
		Args: map[string]any{"expression": "6 * 7"},
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "result(6 * 7) = 42", result.Output)
	assert.Equal(t, "call-calc-1", result.ToolCallID)

	// Verify the mock server observed the call.
	logs := srv.callLog()
	require.Len(t, logs, 1, "server should record exactly one tools/call")
	assert.Equal(t, "calculate", logs[0].ToolName)
	assert.Equal(t, "6 * 7", logs[0].Args["expression"])

	// A second execution should produce a fresh call log entry.
	_, err = tr.Execute(ctx, tools.ToolCall{
		ID:   "call-calc-2",
		Name: toolName,
		Args: map[string]any{"expression": "1 + 1"},
	})
	require.NoError(t, err)
	logs = srv.callLog()
	require.Len(t, logs, 2)
	assert.Equal(t, "1 + 1", logs[1].Args["expression"])

	// Calling an unknown tool should surface ErrToolNotFound.
	_, err = tr.Execute(ctx, tools.ToolCall{Name: "mcp__calc-srv__missing"})
	require.ErrorIs(t, err, tools.ErrToolNotFound)
}

// =============================================================================
// 4. TestE2E_SkillLoadingFromDirectory
// =============================================================================

// TestE2E_SkillLoadingFromDirectory creates a temp directory with .md skill
// files using YAML frontmatter, loads them with YAMLSkillLoader.LoadDir,
// registers them in a DefaultSkillRegistry, wraps each with SkillAdapter,
// registers into a tools.ToolRegistry, and verifies they can be listed and
// executed.
func TestE2E_SkillLoadingFromDirectory(t *testing.T) {
	tmpDir := t.TempDir()

	// Skill A: full frontmatter with tools, parameters and a prompt block.
	skillA := `---
name: deploy-helper
description: Helps deploy services
version: 1.2.0
category: ops
prompt: |
  You are a deployment assistant.
  Always verify the target environment.
tools:
  - bash
  - read
trigger_hint: "deploy service"
parameters:
  max_retries: 3
  dry_run: true
---
Deployment helper body.
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "deploy-helper.md"), []byte(skillA), 0o644))

	// Skill B: minimal frontmatter; the body becomes the prompt.
	skillB := `---
name: summarize
description: Summarize text content
---
Summarize the provided text concisely.`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "summarize.md"), []byte(skillB), 0o644))

	// A non-skill file that should be ignored by LoadDir.
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "notes.txt"), []byte("not a skill"), 0o644))

	ctx := context.Background()

	// Load all skill files from the directory.
	loader := skill.NewYAMLSkillLoader()
	defs, err := loader.LoadDir(ctx, tmpDir)
	require.NoError(t, err)
	assert.Len(t, defs, 2, "LoadDir should find exactly two .md skill files")

	// Register each definition in the skill registry.
	reg := skill.NewDefaultSkillRegistry()
	for _, d := range defs {
		require.NotNil(t, d)
		require.NoError(t, reg.Register(ctx, *d))
	}

	// Verify the registry holds both skills.
	all := reg.List(ctx)
	require.Len(t, all, 2)

	// Wrap each registered skill with SkillAdapter and register as a tool.
	tr := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry)
	for _, s := range all {
		adapter := skill.NewSkillAdapter(s)
		require.NoError(t, tr.Register(ctx, adapter))
	}

	listed, err := tr.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 2)

	// Look up "deploy-helper" and verify its description carries tool/param info.
	deployDef, err := tr.Get(ctx, "deploy-helper")
	require.NoError(t, err)
	assert.Equal(t, "deploy-helper", deployDef.Name())
	desc := deployDef.Description()
	assert.Contains(t, desc, "Helps deploy services")
	assert.Contains(t, desc, "tools: bash, read")
	assert.Contains(t, desc, "parameters: dry_run, max_retries")

	// Execute the skill through the registry and verify the output marker.
	result, err := tr.Execute(ctx, tools.ToolCall{
		ID:   "skill-exec-1",
		Name: "deploy-helper",
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "skill-exec-1", result.ToolCallID)
	assert.Contains(t, result.Output, "[skill deploy-helper]")
	assert.Contains(t, result.Output, "deployment assistant")

	// Execute the second skill (body-as-prompt) and verify its prompt flows.
	result2, err := tr.Execute(ctx, tools.ToolCall{
		ID:   "skill-exec-2",
		Name: "summarize",
	})
	require.NoError(t, err)
	assert.Contains(t, result2.Output, "[skill summarize]")
	assert.Contains(t, result2.Output, "Summarize the provided text")

	// Verify the category filter works on the skill registry.
	opsSkills := reg.List(ctx, "ops")
	require.Len(t, opsSkills, 1)
	assert.Equal(t, "deploy-helper", opsSkills[0].Name())

	// Verify trigger matching finds the deploy skill.
	matches := reg.Match(ctx, "deploy")
	require.Len(t, matches, 1)
	assert.Equal(t, "deploy-helper", matches[0].Name())
}

// =============================================================================
// 5. TestE2E_SkillAndMCPCombinedPipeline
// =============================================================================

// TestE2E_SkillAndMCPCombinedPipeline registers both MCP tools (served by an
// httptest mock server) and skill tools (loaded from a temp directory) into the
// same tools.ToolRegistry, then verifies all tools are accessible and can be
// executed through the registry.
func TestE2E_SkillAndMCPCombinedPipeline(t *testing.T) {
	// --- Set up the MCP HTTP mock server with two tools. ---
	srv := newMockHTTPMCPServer(t)
	srv.sseHandshake = true
	srv.ssePost = true
	srv.declareTool("search_docs", "Search the documentation", map[string]any{
		"type":       "object",
		"properties": map[string]any{"query": map[string]any{"type": "string"}},
	})
	srv.declareTool("count_words", "Count words in text", map[string]any{
		"type":       "object",
		"properties": map[string]any{"text": map[string]any{"type": "string"}},
	})
	srv.callHandler = func(name string, args map[string]any) any {
		switch name {
		case "search_docs":
			return "found 3 docs for: " + safeString(args["query"])
		case "count_words":
			return fmt.Sprintf("word_count=%d", len(safeString(args["text"])))
		default:
			return "unknown"
		}
	}

	httpSrv := srv.start()
	defer httpSrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Connect the HTTPClientAdapter and register its tools.
	adapter := mcp.NewHTTPClientAdapter(mcp.MCPServerConfig{
		Name: "docs-srv",
		URL:  httpSrv.URL,
	})
	require.NoError(t, adapter.Connect(ctx))

	mcpTools, err := adapter.ListTools(ctx)
	require.NoError(t, err)
	require.Len(t, mcpTools, 2)

	// --- Set up skill files in a temp directory. ---
	tmpDir := t.TempDir()
	skillContent := `---
name: explain
description: Explain a concept in simple terms
version: 1.0.0
category: teaching
prompt: |
  You are an expert teacher.
  Explain concepts using simple analogies.
tools:
  - read
trigger_hint: "explain concept"
---
Explain helper body.`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "explain.md"), []byte(skillContent), 0o644))

	loader := skill.NewYAMLSkillLoader()
	defs, err := loader.LoadDir(ctx, tmpDir)
	require.NoError(t, err)
	require.Len(t, defs, 1)

	skillReg := skill.NewDefaultSkillRegistry()
	require.NoError(t, skillReg.Register(ctx, *defs[0]))

	// --- Register both MCP and skill tools into the same ToolRegistry. ---
	tr := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry)
	for _, tool := range mcpTools {
		require.NoError(t, tr.Register(ctx, mcp.NewMCPToolAdapter(adapter, tool)))
	}
	for _, s := range skillReg.List(ctx) {
		require.NoError(t, tr.Register(ctx, skill.NewSkillAdapter(s)))
	}

	// The registry should hold all three tools.
	listed, err := tr.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 3, "registry should contain 2 MCP tools + 1 skill tool")

	// Build a set of registered names for assertion.
	names := make(map[string]bool, len(listed))
	for _, def := range listed {
		names[def.Name()] = true
	}
	searchName := mcp.NormalizeToolName("docs-srv", "search_docs")
	countName := mcp.NormalizeToolName("docs-srv", "count_words")
	assert.True(t, names[searchName], "registry should contain the search_docs MCP tool")
	assert.True(t, names[countName], "registry should contain the count_words MCP tool")
	assert.True(t, names["explain"], "registry should contain the explain skill tool")

	// Execute the first MCP tool (search_docs).
	resSearch, err := tr.Execute(ctx, tools.ToolCall{
		ID:   "combined-search",
		Name: searchName,
		Args: map[string]any{"query": "golang testing"},
	})
	require.NoError(t, err)
	assert.Equal(t, "combined-search", resSearch.ToolCallID)
	assert.Contains(t, resSearch.Output, "found 3 docs for: golang testing")

	// Execute the second MCP tool (count_words).
	resCount, err := tr.Execute(ctx, tools.ToolCall{
		ID:   "combined-count",
		Name: countName,
		Args: map[string]any{"text": "hello world foo"},
	})
	require.NoError(t, err)
	assert.Contains(t, resCount.Output, "word_count=")

	// Execute the skill tool.
	resExplain, err := tr.Execute(ctx, tools.ToolCall{
		ID:   "combined-explain",
		Name: "explain",
	})
	require.NoError(t, err)
	assert.Contains(t, resExplain.Output, "[skill explain]")
	assert.Contains(t, resExplain.Output, "expert teacher")

	// Verify the MCP server recorded both tool calls.
	logs := srv.callLog()
	require.Len(t, logs, 2, "server should record two tools/call invocations")
	assert.Equal(t, "search_docs", logs[0].ToolName)
	assert.Equal(t, "golang testing", logs[0].Args["query"])
	assert.Equal(t, "count_words", logs[1].ToolName)
	assert.Equal(t, "hello world foo", logs[1].Args["text"])

	// Verify the skill registry still serves its own contract independently.
	explainDef, ok := skillReg.Get(ctx, "explain")
	require.True(t, ok)
	assert.Equal(t, "teaching", explainDef.Category())
}
