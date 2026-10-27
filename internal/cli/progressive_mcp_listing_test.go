package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestProgressiveMCPListIsSafePreparedLocalMetadata(t *testing.T) {
	peer := newMCPGenerationPeer(t, []string{"PRIVATE_SCHEMA_BODY"}, "PRIVATE_REMOTE_RESULT", false, false, false)
	cfg := newMCPListTestConfig(t, []config.MCPServerConfig{
		{Name: "zeta", Command: "./PRIVATE_COMMAND", Args: []string{"PRIVATE_ARGUMENT"}, Env: map[string]string{"PRIVATE_ENV": "PRIVATE_VALUE"}},
		{Name: "alpha", URL: peer.server.URL},
	})
	exporter := &assemblyTraceExporter{}
	tracer := tracing.NewTracer("mcp-list-trace", exporter)
	root, ctx := tracer.Start(context.Background(), "cli.invocation", tracing.SpanKindInternal)
	defer root.End()
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	require.NotNil(t, assembly.progressiveMCP)
	assembly.progressiveMCP.mu.Lock()
	assembly.progressiveMCP.diagnostics["zeta"] = peer.server.URL + " PRIVATE_COMMAND PRIVATE_ENV PRIVATE_HEADER"
	assembly.progressiveMCP.mu.Unlock()
	logs := verify.NewLogCapturer()
	ctx = logs.Attach(ctx)
	t.Cleanup(logs.Detach)
	preparer, ok := assembly.ToolRegistry.(tools.ToolCallPreparer)
	require.True(t, ok)
	for _, test := range []struct {
		query string
		want  []MCPServerStatus
	}{
		{want: []MCPServerStatus{{Name: "alpha", State: "discovered"}, {Name: "zeta", State: "failed"}}},
		{query: "ALP", want: []MCPServerStatus{{Name: "alpha", State: "discovered"}}},
		{query: "PRIVATE_QUERY", want: []MCPServerStatus{}},
	} {
		invocation, err := preparer.PrepareToolCall(ctx, tools.ToolCall{ID: "metadata-" + test.query, Name: "mcp_list", Args: map[string]any{"query": test.query}})
		require.NoError(t, err)
		require.Equal(t, []tools.TrustedToolEffect{tools.EffectControl}, invocation.EffectSet().Values())
		require.Equal(t, tools.RegistrationSourceTrustedControl, invocation.ToolIdentity().RegistrationSource())
		result, err := invocation.Dispatch(ctx)
		require.NoError(t, err)
		var statuses []MCPServerStatus
		require.NoError(t, json.Unmarshal([]byte(result.Output), &statuses))
		require.Equal(t, test.want, statuses)
		for _, private := range []string{peer.server.URL, "PRIVATE_COMMAND", "PRIVATE_ARGUMENT", "PRIVATE_ENV", "PRIVATE_VALUE", "PRIVATE_HEADER", "PRIVATE_SCHEMA_BODY", "PRIVATE_REMOTE_RESULT", "PRIVATE_QUERY"} {
			require.NotContains(t, result.Output, private)
		}
		require.Equal(t, tools.CompletionInlinePipelineJoined, invocation.CompletionSignal().Proof())
		select {
		case <-invocation.CompletionSignal().Done():
		default:
			t.Fatal("metadata listing must finish its prepared pipeline synchronously")
		}
	}
	for _, args := range []map[string]any{{"server": "alpha"}, {"query": 1}, {"unexpected": true}} {
		_, err := preparer.PrepareToolCall(ctx, tools.ToolCall{Name: "mcp_list", Args: args})
		require.Error(t, err, "listing cannot accept an activation argument or invalid query")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	definition, err := assembly.ToolRegistry.Get(ctx, "mcp_list")
	require.NoError(t, err)
	result, err := definition.Execute(canceled, tools.ToolCall{Name: "mcp_list"})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, result)
	activation, err := preparer.PrepareToolCall(ctx, tools.ToolCall{Name: "mcp_discover", Args: map[string]any{}})
	require.NoError(t, err)
	require.Equal(t, []tools.TrustedToolEffect{tools.EffectUnknown}, activation.EffectSet().Values())
	_, err = activation.Dispatch(ctx)
	require.NoError(t, err)
	require.NotNil(t, activation.CompletionSignal().Done())
	require.Equal(t, tools.CompletionMCPRequestJoined, activation.CompletionSignal().Proof(), "activation has its own action-scoped joined request contract")
	require.Zero(t, peer.requests.Load())
	require.Zero(t, peer.lists.Load())
	require.Zero(t, peer.calls.Load())
	listingSpans := 0
	for _, span := range exporter.exportedSpans() {
		attributes := make(map[string]any, len(span.Attributes))
		for _, attribute := range span.Attributes {
			attributes[attribute.Key] = attribute.Value
		}
		if span.Name == "tool.call" && attributes["tool_name"] == "mcp_list" {
			listingSpans++
			require.Equal(t, true, attributes["success"])
			require.Contains(t, attributes, "result_count")
		}
		encoded, err := json.Marshal(span)
		require.NoError(t, err)
		for _, private := range []string{peer.server.URL, "PRIVATE_COMMAND", "PRIVATE_ENV", "PRIVATE_QUERY", "PRIVATE_SCHEMA_BODY"} {
			require.NotContains(t, string(encoded), private)
		}
	}
	require.Equal(t, 3, listingSpans)
	listingLogs := 0
	for _, entry := range logs.Entries() {
		if entry.Message == "mcp.list" {
			listingLogs++
			require.Equal(t, "mcp_list", entry.Fields["tool_name"])
			require.Equal(t, true, entry.Fields["success"])
			require.Contains(t, entry.Fields, "result_count")
		}
		encoded, err := json.Marshal(entry)
		require.NoError(t, err)
		for _, private := range []string{peer.server.URL, "PRIVATE_COMMAND", "PRIVATE_ENV", "PRIVATE_HEADER", "PRIVATE_QUERY", "PRIVATE_SCHEMA_BODY"} {
			require.NotContains(t, string(encoded), private)
		}
	}
	require.Equal(t, 3, listingLogs)
	// The concrete type and name alone do not mint the host's completion proof.
	require.NoError(t, assembly.ToolRegistry.Register(ctx, &mcpListTool{owner: assembly.progressiveMCP}))
	ordinary, err := preparer.PrepareToolCall(ctx, tools.ToolCall{Name: "mcp_list", Args: map[string]any{}})
	require.NoError(t, err)
	_, err = ordinary.Dispatch(ctx)
	require.NoError(t, err)
	require.Equal(t, []tools.TrustedToolEffect{tools.EffectUnknown}, ordinary.EffectSet().Values())
	require.Nil(t, ordinary.CompletionSignal().Done())
	require.Empty(t, ordinary.CompletionSignal().Proof())
}

func TestProgressiveMCPListHandlesEmptyAndUnavailableController(t *testing.T) {
	result, err := (&mcpListTool{owner: &progressiveMCP{}}).Execute(context.Background(), tools.ToolCall{ID: "empty", Name: "mcp_list"})
	require.NoError(t, err)
	require.Equal(t, "[]", result.Output)
	require.Equal(t, "empty", result.ToolCallID)
	for _, tool := range []*mcpListTool{nil, {}} {
		result, err := tool.Execute(context.Background(), tools.ToolCall{Name: "mcp_list"})
		require.ErrorContains(t, err, "listing is unavailable")
		require.Nil(t, result)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err = (&mcpListTool{owner: &progressiveMCP{}}).Execute(ctx, tools.ToolCall{Name: "mcp_list"})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, result)
}

func TestDurableMCPListContinuesAcrossOwnedRunsWithoutConnecting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	peer := newMCPGenerationPeer(t, []string{"PRIVATE_SCHEMA_BODY"}, "PRIVATE_REMOTE_RESULT", false, false, false)
	cfg := newMCPListTestConfig(t, []config.MCPServerConfig{{Name: "catalog", URL: peer.server.URL}})
	cfg.Runtime.Dynamic = true
	provider, script := newDurablePromptServer(t,
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"list-one","type":"function","function":{"name":"mcp_list","arguments":"{}"}}]}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"first catalog completed"}}]}`,
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"list-two","type":"function","function":{"name":"mcp_list","arguments":"{\"query\":\"cat\"}"}}]}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"second catalog completed"}}]}`,
	)
	defer provider.Close()
	cfg.Provider.BaseURL = provider.URL
	removeOwnedMCPListAuthority(t, cfg.Git.WorkDir)
	durable, err := newDurableCLISession(ctx, filepath.Join(cfg.Git.WorkDir, "sessions"), "mcp-list-durable", "", "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, durable.requestClose(context.Background())) })
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto), withDurableCLISession(durable))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	assembly.LoopAgent.WithToolSearchThreshold(0)
	var physicalID string
	var published session.PreparedSessionSnapshot
	for _, prompt := range []string{"first catalog", "second catalog"} {
		runCtx, sink, err := durable.bindRun(ctx)
		require.NoError(t, err)
		require.NoError(t, hydrateDurableRunHistory(assembly.Agent, sink))
		stream, err := assembly.Harness.Submit(runCtx, prompt)
		require.NoError(t, err)
		for range stream.Events() {
		}
		result, err := stream.Result()
		require.NoError(t, err)
		require.Contains(t, result.Content, prompt+" completed")
		sink.CloseRunCommit()
		published, err = durable.snapshotSelected()
		require.NoError(t, err)
		if physicalID == "" {
			physicalID = published.SessionID
		}
		require.Equal(t, physicalID, published.SessionID)
	}
	proofs := map[string]int{}
	receipts := map[string]int{}
	terminals := 0
	for _, entry := range published.DurableEntries {
		if entry.CheckpointKind == session.CheckpointRunTerminal {
			terminals++
		}
		if entry.ToolName != "mcp_list" {
			continue
		}
		if entry.ActionState == session.ActionStateExecution && entry.CompletionProof != "" {
			require.Equal(t, tools.CompletionInlinePipelineJoined, entry.CompletionProof)
			proofs[entry.ToolCallID]++
		}
		if entry.Type == session.EntryTypeTool && entry.ActionState == "" {
			require.Equal(t, string(core.ReceiptOutcomeSuccess), entry.ReceiptOutcome)
			receipts[entry.ToolCallID]++
		}
	}
	require.Equal(t, map[string]int{"list-one": 1, "list-two": 1}, proofs)
	require.Equal(t, map[string]int{"list-one": 1, "list-two": 1}, receipts)
	require.Equal(t, 2, terminals)
	require.Len(t, script.requestBodies(), 4)
	for _, body := range script.requestBodies() {
		require.Contains(t, body, "mcp_list", "actual provider catalog includes local MCP listing")
		require.NotContains(t, body, peer.server.URL)
		require.NotContains(t, body, "PRIVATE_SCHEMA_BODY")
	}
	require.Contains(t, script.requestBodies()[1], "catalog")
	require.Zero(t, peer.requests.Load())
	require.Zero(t, peer.lists.Load())
	require.Zero(t, peer.calls.Load())
}

func newMCPListTestConfig(t *testing.T, servers []config.MCPServerConfig, setup ...func(string)) *config.Config {
	t.Helper()
	base := newAssembleTestConfig(t)
	root, err := filepath.EvalSymlinks(base.Git.WorkDir)
	require.NoError(t, err)
	base.Git.WorkDir = root
	t.Chdir(root)
	for _, prepare := range setup {
		prepare(root)
	}
	base.MCP.Loading = "lazy"
	base.Skill.Dir = filepath.Join(root, ".go-cli", "skills")
	require.NoError(t, os.MkdirAll(base.Skill.Dir, 0700))
	for _, server := range servers {
		if server.Command == "./PRIVATE_COMMAND" {
			require.NoError(t, os.WriteFile(filepath.Join(root, "PRIVATE_COMMAND"), []byte("#!/bin/sh\nexit 91\n"), 0700))
		}
	}
	data, err := json.Marshal(map[string]any{"servers": servers})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, ".go-cli", "mcp.json"), data, 0600))
	manager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore())
	captured, err := config.CaptureProjectSources(context.Background(), root, nil)
	require.NoError(t, err)
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(context.Background(), captured))
	trusted, err := config.LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)
	require.NotNil(t, trusted.VerifiedProjectSources())
	// Keep the real loader's sealed project manifest and manager. Replace all
	// exported execution/storage settings with this fixture's explicit config,
	// so ambient global hooks, extensions, providers and stores cannot run here.
	fixture, err := json.Marshal(base)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(fixture, trusted))
	return trusted
}

func removeOwnedMCPListAuthority(t *testing.T, root string) {
	t.Helper()
	workspaceID, err := session.WorkspaceIDForCanonicalRoot(root)
	require.NoError(t, err)
	configDir, err := os.UserConfigDir()
	require.NoError(t, err)
	authorityDir := filepath.Join(configDir, "go-cli", "workspace-quarantine")
	for _, suffix := range []string{".jsonl", ".lock"} {
		_, err := os.Lstat(filepath.Join(authorityDir, workspaceID+suffix))
		require.ErrorIs(t, err, os.ErrNotExist, "fixture cannot borrow an existing authority")
	}
	t.Cleanup(func() {
		for _, pattern := range []string{workspaceID + ".jsonl", workspaceID + ".lock", workspaceID + ".jsonl.torn-*.tail", workspaceID + ".jsonl.init-*"} {
			paths, err := filepath.Glob(filepath.Join(authorityDir, pattern))
			require.NoError(t, err)
			for _, path := range paths {
				require.NoError(t, os.Remove(path))
			}
		}
	})
}
