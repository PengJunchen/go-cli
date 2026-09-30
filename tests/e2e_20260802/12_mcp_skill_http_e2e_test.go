// Package e2e_20260802 contains end-to-end integration tests for the MCP
// HTTPClientAdapter and Skill loading pipelines. It exercises:
//  1. Explicit rejection of the unsupported legacy SSE transport
//  2. Streamable HTTP JSON-RPC initialize, tool listing and tool execution
//  3. Full MCP tool execution through the tool registry
//  4. Skill loading from a directory with YAML frontmatter
//  5. Combined MCP + Skill pipeline in a single tool registry
package e2e_20260802 //nolint:staticcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

func validMCPJSONRPCRequestID(raw json.RawMessage) bool {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch value.(type) {
	case string, float64:
		return true
	default:
		return false
	}
}

// mockHTTPMCPServer is an in-process Streamable HTTP server for protocol
// version 2025-06-18. It validates JSON-RPC request IDs and negotiated headers,
// acknowledges the initialized notification with HTTP 202, and returns JSON
// response envelopes.
//
// It also records every tools/call so tests can assert on invocations.
type mockHTTPMCPServer struct {
	t *testing.T

	mu          sync.Mutex
	logs        []mcpHTTPCallLog
	methods     []string
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

func (s *mockHTTPMCPServer) methodLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.methods...)
}

// handler dispatches GET (SSE handshake) and POST (JSON-RPC) requests.
func (s *mockHTTPMCPServer) handler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handlePost(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *mockHTTPMCPServer) handlePost(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") != "application/json" ||
		!strings.Contains(r.Header.Get("Accept"), "application/json") ||
		!strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.t.Errorf("invalid Streamable HTTP headers: content-type=%q accept=%q", r.Header.Get("Content-Type"), r.Header.Get("Accept"))
		http.Error(w, "invalid Streamable HTTP headers", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Errorf("mock mcp server: read body: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if decodeErr := json.Unmarshal(body, &req); decodeErr != nil {
		s.t.Errorf("mock mcp server: decode request: %v", decodeErr)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if req.JSONRPC != "2.0" {
		s.t.Errorf("unexpected JSON-RPC version %q", req.JSONRPC)
		http.Error(w, "invalid JSON-RPC request", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.methods = append(s.methods, req.Method)
	s.mu.Unlock()

	var result map[string]any
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil || !validMCPJSONRPCRequestID(req.ID) || params.ProtocolVersion != mcp.LatestProtocolVersion || r.Header.Get("MCP-Protocol-Version") != "" {
			s.t.Errorf("invalid 2025-06-18 initialize request")
			http.Error(w, "invalid initialize request", http.StatusBadRequest)
			return
		}
		result = map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "mock", "version": "1.0"},
		}
	case "notifications/initialized":
		if len(req.ID) != 0 || r.Header.Get("MCP-Protocol-Version") != mcp.LatestProtocolVersion {
			s.t.Errorf("invalid initialized notification")
			http.Error(w, "invalid initialized notification", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	case "tools/list":
		if !validMCPJSONRPCRequestID(req.ID) || r.Header.Get("MCP-Protocol-Version") != mcp.LatestProtocolVersion {
			s.t.Errorf("invalid tools/list request")
			http.Error(w, "invalid tools/list request", http.StatusBadRequest)
			return
		}
		result = map[string]any{"tools": s.tools}
	case "tools/call":
		if !validMCPJSONRPCRequestID(req.ID) || r.Header.Get("MCP-Protocol-Version") != mcp.LatestProtocolVersion {
			s.t.Errorf("invalid tools/call request")
			http.Error(w, "invalid tools/call request", http.StatusBadRequest)
			return
		}
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if paramsErr := json.Unmarshal(req.Params, &params); paramsErr != nil {
			s.t.Errorf("mock mcp server: decode tools/call params: %v", paramsErr)
			result = map[string]any{
				"content": []map[string]any{{"type": "text", "text": "invalid tools/call params"}},
				"isError": true,
			}
		} else {
			content := s.callTool(params.Name, params.Arguments)
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": fmt.Sprint(content)}}}
		}
	default:
		result = map[string]any{}
	}

	frame := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(req.ID), "result": result}
	raw, err := json.Marshal(frame)
	if err != nil {
		s.t.Errorf("mock mcp server: marshal response: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw) //nolint:errcheck,gosec
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
// 1. TestE2E_MCPHTTPAdapterRejectsLegacySSEBeforeNetwork
// =============================================================================

// TestE2E_MCPHTTPAdapter_LegacySSERejectedBeforeNetwork verifies that the
// unsupported legacy SSE transport is rejected before endpoint probing.
func TestE2E_MCPHTTPAdapterRejectsLegacySSEBeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer httpSrv.Close()

	adapter := mcp.NewHTTPClientAdapter(mcp.MCPServerConfig{
		Name:      "weather-sse",
		URL:       httpSrv.URL,
		Transport: mcp.MCPTransportSSE,
	})
	err := adapter.Connect(context.Background())
	require.ErrorIs(t, err, mcp.ErrUnsupportedHTTPTransport)
	assert.Zero(t, requests.Load())
}

// =============================================================================
// 2. TestE2E_MCPHTTPAdapter_StreamableHTTPConnectAndListTools
// =============================================================================

// TestE2E_MCPHTTPAdapter_StreamableHTTPConnectAndListTools verifies the
// 2025-06-18 initialize/initialized exchange and JSON-RPC tools/list flow.
func TestE2E_MCPHTTPAdapter_StreamableHTTPConnectAndListTools(t *testing.T) {
	srv := newMockHTTPMCPServer(t)
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

	require.NoError(t, adapter.Connect(ctx), "Streamable HTTP connect should succeed")

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
	assert.Equal(t, []string{"initialize", "notifications/initialized", "tools/list"}, srv.methodLog())
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
		Name:      "calc-srv",
		URL:       httpSrv.URL,
		Transport: mcp.MCPTransportStreamableHTTP,
	})
	require.NoError(t, adapter.Connect(ctx))

	// Discover tools and register the single tool into the registry.
	mcpTools, err := adapter.ListTools(ctx)
	require.NoError(t, err)
	require.Len(t, mcpTools, 1)

	toolAdapter := mcp.NewMCPToolAdapter(adapter, mcpTools[0])
	tr := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck,gosec
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
	assert.Contains(t, result.Output, "result(6 * 7) = 42")
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
	assert.Equal(t, []string{
		"initialize", "notifications/initialized", "tools/list", "tools/call", "tools/call",
	}, srv.methodLog())

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
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "deploy-helper.md"), []byte(skillA), 0o600))

	// Skill B: minimal frontmatter; the body becomes the prompt.
	skillB := `---
name: summarize
description: Summarize text content
---
Summarize the provided text concisely.`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "summarize.md"), []byte(skillB), 0o600))

	// A non-skill file that should be ignored by LoadDir.
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "notes.txt"), []byte("not a skill"), 0o600))

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
	tr := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
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
	assert.Contains(t, desc, "suggested tools (not enforced): bash, read")
	assert.Contains(t, desc, "legacy parameter metadata (not accepted as arguments): dry_run, max_retries")

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
		Name:      "docs-srv",
		URL:       httpSrv.URL,
		Transport: mcp.MCPTransportStreamableHTTP,
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
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "explain.md"), []byte(skillContent), 0o600))

	loader := skill.NewYAMLSkillLoader()
	defs, err := loader.LoadDir(ctx, tmpDir)
	require.NoError(t, err)
	require.Len(t, defs, 1)

	skillReg := skill.NewDefaultSkillRegistry()
	require.NoError(t, skillReg.Register(ctx, *defs[0]))

	// --- Register both MCP and skill tools into the same ToolRegistry. ---
	tr := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
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
