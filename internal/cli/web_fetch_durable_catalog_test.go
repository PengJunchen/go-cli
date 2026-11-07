package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func closeWebFetchCatalogAssembly(t *testing.T, assembly *AgentAssembly) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, assembly.Close(ctx))
}

func closeWebFetchCatalogSession(t *testing.T, durable *durableCLISession) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, durable.requestClose(ctx))
}

type webFetchCatalogWire struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func TestDurableBuiltinWebFetchCatalogMatchesExecutableCapability(t *testing.T) {
	configureDurableCLISessionTest(t)
	for _, tc := range []struct {
		name    string
		durable bool
	}{
		{name: "durable hides built-in", durable: true},
		{name: "non-durable retains built-in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			root := t.TempDir()
			cfg := newAssembleTestConfig(t)
			cfg.Git.WorkDir = root
			cfg.WebSearch.Provider = "fetch"
			opts := []AssembleOption{WithApproveMode(ApproveAuto)}
			if tc.durable {
				durable, err := newDurableCLISession(ctx, filepath.Join(root, "history"), "catalog", "", "")
				require.NoError(t, err)
				t.Cleanup(func() { closeWebFetchCatalogSession(t, durable) })
				opts = append(opts, withDurableCLISession(durable))
			}
			assembly, err := AssembleAgent(ctx, cfg, "openai", "fixture", io.Discard, opts...)
			require.NoError(t, err)
			t.Cleanup(func() { closeWebFetchCatalogAssembly(t, assembly) })
			definitions, err := assembly.ToolRegistry.List(ctx)
			require.NoError(t, err)
			names := toolNames(definitions)
			fetch, fetchErr := assembly.ToolRegistry.Get(ctx, "web_fetch")
			if tc.durable {
				assert.NotContains(t, names, "web_fetch")
				require.ErrorIs(t, fetchErr, tools.ErrToolNotFound)
			} else {
				assert.Contains(t, names, "web_fetch")
				require.NoError(t, fetchErr)
				preparer := assembly.ToolRegistry.(tools.ToolCallPreparer)
				prepared, prepareErr := preparer.PrepareToolCall(ctx, tools.ToolCall{Name: "web_fetch", Args: map[string]any{"url": "http://127.0.0.1:1/"}})
				require.NoError(t, prepareErr)
				assert.Equal(t, tools.RegistrationSourceTrustedNetworkRead, prepared.ToolIdentity().RegistrationSource())
				supported, supportErr := tools.PreparedCompletionSupported(ctx, prepared, nil)
				require.NoError(t, supportErr)
				assert.False(t, supported, "retained non-durable fetch must not gain a forged completion proof")
				_, executeErr := fetch.Execute(ctx, tools.ToolCall{Name: "web_fetch", Args: map[string]any{"url": "http://127.0.0.1:1/"}})
				require.ErrorIs(t, executeErr, tools.ErrPrivateIP, "default SSRF protection remains active")
			}
			search, err := assembly.ToolRegistry.Get(ctx, "tool_search")
			require.NoError(t, err)
			found, err := search.Execute(ctx, tools.ToolCall{Name: "tool_search", Args: map[string]any{"query": "web_fetch"}})
			require.NoError(t, err)
			if tc.durable {
				assert.NotContains(t, found.Output, "web_fetch\t")
			} else {
				assert.Contains(t, found.Output, "web_fetch\t")
			}
			_, err = assembly.ToolRegistry.Get(ctx, "web_search")
			require.NoError(t, err, "configured WebSearch registration is unchanged")
		})
	}
}

func TestDurableBuiltinWebFetchAbsentFromWireAndForcedCallNeverDispatches(t *testing.T) {
	configureDurableCLISessionTest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	root := t.TempDir()
	t.Chdir(root)
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetRequests.Add(1)
		_, _ = io.WriteString(w, "unexpected")
	}))
	defer target.Close()
	server, script := newDurablePromptServer(t,
		durableApprovalToolResponse(t, "search-web", "tool_search", map[string]any{"query": "web_fetch"}),
		durableApprovalToolResponse(t, "forced-web", "web_fetch", map[string]any{"url": target.URL}),
		`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`,
	)
	defer server.Close()
	store := filepath.Join(root, "history")
	cfg := durablePromptConfig(t, server.URL, store)
	cfg.Git.WorkDir = root
	var output bytes.Buffer
	require.NoError(t, newPromptCmd(&output).Run(ctx, cfg, []string{"--approve", "auto", "Use tool_search to look for web_fetch and then report."}))
	bodies := script.requestBodies()
	require.Len(t, bodies, 3)
	for _, body := range bodies {
		var wire webFetchCatalogWire
		require.NoError(t, json.Unmarshal([]byte(body), &wire))
		for _, definition := range wire.Tools {
			assert.NotEqual(t, "web_fetch", definition.Function.Name, "builtin cannot be advertised in any request")
		}
	}
	var second webFetchCatalogWire
	require.NoError(t, json.Unmarshal([]byte(bodies[1]), &second))
	for _, message := range second.Messages {
		if message.Role == "tool" {
			assert.NotContains(t, message.Content, "web_fetch\t", "tool_search must not discover hidden builtin")
		}
	}
	assert.Zero(t, targetRequests.Load(), "forced call must not reach the HTTP target")
	physicalID := physicalSessionIDFromOutput(t, output.String())
	resumed, err := newDurableCLISession(ctx, store, "inspect", physicalID, "")
	require.NoError(t, err)
	t.Cleanup(func() { closeWebFetchCatalogSession(t, resumed) })
	snapshot, err := resumed.snapshotSelected()
	require.NoError(t, err)
	pending, err := session.InspectPendingActions(snapshot)
	require.NoError(t, err)
	assert.Empty(t, pending)
	var seen bool
	for _, row := range snapshot.DurableEntries {
		if row.ToolCallID != "forced-web" {
			continue
		}
		assert.Empty(t, row.CompletionProof, "an absent tool cannot mint a completion proof")
		assert.Empty(t, row.ActionID, "an absent tool must fail before action dispatch")
		if row.Type == session.EntryTypeTool {
			seen = true
			assert.Equal(t, "not_dispatched", string(row.DispatchState))
			assert.Equal(t, "error", row.ReceiptOutcome)
			assert.True(t, strings.Contains(strings.ToLower(row.Content), "not found") || strings.Contains(strings.ToLower(row.Content), "unknown"), row.Content)
		}
	}
	require.True(t, seen, "forced unadvertised call must have a durable rejection receipt")
}

func TestDurableWebFetchDoesNotRemoveCustomSameName(t *testing.T) {
	configureDurableCLISessionTest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	root := t.TempDir()
	t.Chdir(root)
	previous := approval.GetTrustManager()
	manager := approval.NewDefaultTrustManager(nil)
	require.NoError(t, manager.TrustProject(ctx, root))
	approval.RegisterTrustManager(manager)
	t.Cleanup(func() { approval.RegisterTrustManager(previous) })
	cfg := newAssembleTestConfig(t)
	cfg.Git.WorkDir = root
	cfg.Tools.CustomTools = []config.CustomToolConfig{{Name: "web_fetch", Description: "custom local fetch marker", Command: []string{"/usr/bin/printf", "local"}, WorkingDir: root}}
	durable, err := newDurableCLISession(ctx, filepath.Join(root, "history"), "custom", "", "")
	require.NoError(t, err)
	t.Cleanup(func() { closeWebFetchCatalogSession(t, durable) })
	assembly, err := AssembleAgent(ctx, cfg, "openai", "fixture", io.Discard, WithApproveMode(ApproveAuto), withDurableCLISession(durable))
	require.NoError(t, err)
	t.Cleanup(func() { closeWebFetchCatalogAssembly(t, assembly) })
	definition, err := assembly.ToolRegistry.Get(ctx, "web_fetch")
	require.NoError(t, err)
	assert.Contains(t, definition.Description(), "custom local fetch marker")
	prepared, err := assembly.ToolRegistry.(tools.ToolCallPreparer).PrepareToolCall(ctx, tools.ToolCall{Name: "web_fetch", Args: map[string]any{"input": "x"}})
	require.NoError(t, err)
	assert.NotEqual(t, tools.RegistrationSourceTrustedNetworkRead, prepared.ToolIdentity().RegistrationSource())
}

func TestDurableWebFetchDoesNotRemoveMCPRemoteNamedWebFetch(t *testing.T) {
	configureDurableCLISessionTest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	peer := newMCPGenerationPeer(t, []string{"web_fetch"}, "unused", false, false, false)
	cfg := newMCPListTestConfig(t, []config.MCPServerConfig{{Name: "catalog", URL: peer.server.URL}})
	durable, err := newDurableCLISession(ctx, filepath.Join(cfg.Git.WorkDir, "history"), "mcp-web-fetch", "", "")
	require.NoError(t, err)
	t.Cleanup(func() { closeWebFetchCatalogSession(t, durable) })
	assembly, err := AssembleAgent(ctx, cfg, "openai", "fixture", io.Discard, WithApproveMode(ApproveAuto), withDurableCLISession(durable))
	require.NoError(t, err)
	t.Cleanup(func() { closeWebFetchCatalogAssembly(t, assembly) })
	require.NoError(t, assembly.LoadMCPServer(ctx, "catalog"))
	_, err = assembly.ToolRegistry.Get(ctx, "web_fetch")
	require.ErrorIs(t, err, tools.ErrToolNotFound)
	definition, err := assembly.ToolRegistry.Get(ctx, "mcp__catalog__web_fetch")
	require.NoError(t, err)
	assert.Equal(t, "mcp__catalog__web_fetch", definition.Name())
	search, err := assembly.ToolRegistry.Get(ctx, "tool_search")
	require.NoError(t, err)
	found, err := search.Execute(ctx, tools.ToolCall{Name: "tool_search", Args: map[string]any{"query": "web_fetch"}})
	require.NoError(t, err)
	assert.Contains(t, found.Output, "mcp__catalog__web_fetch\t")
	assert.Zero(t, peer.calls.Load(), "listing must not execute the MCP tool")
}
