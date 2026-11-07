package core_test

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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type grepBudgetWire struct {
	Messages []struct {
		Role       string `json:"role"`
		Content    string `json:"content"`
		ToolCallID string `json:"tool_call_id"`
		ToolCalls  []struct {
			ID string `json:"id"`
		} `json:"tool_calls"`
	} `json:"messages"`
}

// Exercises actual Grep, durable receipt sync, prepared envelope admission,
// native provider encoding and loopback dispatch in a complete run.
func TestGrepBudgetDurableReceiptsReachNextProviderRequest(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	root := filepath.Join(home, "workspace")
	for _, d := range []string{"internal/compaction", "internal/llm"} {
		dir := filepath.Join(root, d)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "matches.go"), []byte(strings.Repeat("// Summary Thinking Reasoning\n", 130)), 0o600))
	}
	workspaceID := "grep-budget-durable-workspace"
	index, prepared, sink := prepareRunCommitSession(t, home, workspaceID, "grep budget durable")
	preparedOpen := true
	t.Cleanup(func() {
		if preparedOpen {
			_ = prepared.Close()
		}
	})
	before := make(chan session.PreparedSessionSnapshot, 1)
	requests := make(chan grepBudgetWire, 2)
	var mu sync.Mutex
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request grepBudgetWire
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		requests <- request
		mu.Lock()
		count++
		attempt := count
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if attempt == 1 {
			calls := []map[string]any{}
			for i, args := range []map[string]any{
				{"path": filepath.Join(root, "internal/compaction"), "pattern": "Summary|summary|Generate"},
				{"path": filepath.Join(root, "internal/llm"), "pattern": "[Tt]hinking|[Rr]easoning"},
				{"path": filepath.Join(root, "internal"), "glob": "*.go", "pattern": "[Tt]hinking|[Rr]easoning"},
			} {
				encoded, _ := json.Marshal(args)
				calls = append(calls, map[string]any{"index": i, "id": fmt.Sprintf("grep-%d", i+1), "type": "function", "function": map[string]any{"name": "grep", "arguments": string(encoded)}})
			}
			chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"role": "assistant", "tool_calls": calls}}}})
			_, _ = fmt.Fprintf(w, "data: %s\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", chunk)
			return
		}
		snapshot, err := prepared.Snapshot()
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		before <- snapshot
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"accepted\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	registry := llm.NewProviderRegistry()
	require.NoError(t, registry.Register(llm.NewOpenAIProvider(llm.WithNativeModels([]llm.ModelInfo{{Name: "grep-budget-loopback"}}), llm.WithNativeHTTPClient(server.Client()))))
	resolved, err := registry.SnapshotResolver().ResolveModel(ctx, llm.ModelRequest{ProviderID: "openai", ModelID: "grep-budget-loopback", Endpoint: server.URL})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(ctx)
	require.NoError(t, err)
	defer cleanup()
	base := tools.NewDeferredToolRegistryAdapter(tools.NewDefaultToolRegistry())
	require.NoError(t, tools.RegisterDefaults(ctx, base, tools.WithRegisteredBuiltinWhitelist([]string{"grep"}), tools.WithRegisteredPathWhitelist([]string{root}), tools.WithRegisteredCanonicalWorkspaceRoot(root)))
	toolRegistry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, toolRegistry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	require.NoError(t, toolRegistry.SetPreparedToolStages(tools.PreparedToolStages{}))
	loop := core.NewLoopAgent(core.WithLLM(model), core.WithTools(toolRegistry), core.WithSystemPrompt("Search these files"))
	loop.SetMidTurnCompaction(nil, compaction.NewUnifiedCompactor(), compaction.NewCompositeTokenEstimator(0), 128000)
	agent := core.NewAgentImpl("grep-budget", loop)
	source, err := sink.(core.RunCommitCompactionSource).CompactionSourceSnapshot()
	require.NoError(t, err)
	require.NoError(t, agent.SetHistoryFromSourceSnapshot(source))
	harness := core.NewHarnessImpl(agent)
	stream, err := harness.Submit(core.WithRunCommit(ctx, sink, core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID, CanonicalWorkspaceRoot: root}), "inspect the workspace")
	require.NoError(t, err)
	for range stream.Events() {
	}
	_, err = stream.Result()
	require.NoError(t, err)
	mu.Lock()
	total := count
	mu.Unlock()
	require.Equal(t, 2, total)
	<-requests
	second := <-requests
	snapshot := <-before
	var assistantIDs []string
	wireReceipts := make(map[string]string)
	for _, m := range second.Messages {
		for _, call := range m.ToolCalls {
			assistantIDs = append(assistantIDs, call.ID)
		}
		if m.Role == "tool" {
			wireReceipts[m.ToolCallID] = m.Content
		}
	}
	require.Equal(t, []string{"grep-1", "grep-2", "grep-3"}, assistantIDs)
	require.Len(t, wireReceipts, 3)
	persisted := make(map[string]string)
	for _, row := range snapshot.DurableEntries {
		require.NotEqual(t, session.EntryTypeCompaction, row.Type, "fitting must not create a projection")
		if row.Type == session.EntryTypeTool && row.ReceiptOutcome != "" {
			require.Equal(t, core.ReceiptOutcomeSuccess, row.ReceiptOutcome)
			_, duplicate := persisted[row.ToolCallID]
			require.False(t, duplicate)
			persisted[row.ToolCallID] = row.Content
		}
	}
	require.Equal(t, wireReceipts, persisted, "provider receives the exact already-synced receipts")
	for id, output := range persisted {
		require.Contains(t, output, "[results truncated]", id)
		body, _, ok := strings.Cut(output, "\n[results truncated]")
		require.True(t, ok)
		require.LessOrEqual(t, len(body)+1, 50*1024)
		require.LessOrEqual(t, len(strings.Split(body, "\n")), 100)
	}
	live, err := prepared.Snapshot()
	require.NoError(t, err)
	require.NoError(t, prepared.Close())
	preparedOpen = false
	reopened, err := session.NewDurableJSONLSessionPreparer(filepath.Join(home, "durable"), index).PrepareResume(ctx, live.SessionID)
	require.NoError(t, err)
	defer reopened.Close()
	restored, err := reopened.Snapshot()
	require.NoError(t, err)
	after := make(map[string]string)
	for _, row := range restored.DurableEntries {
		if row.Type == session.EntryTypeTool && row.ReceiptOutcome != "" {
			after[row.ToolCallID] = row.Content
		}
	}
	require.Equal(t, persisted, after, "reopened durable receipt identities and bytes remain unchanged")
	t.Logf("loopback_requests=%d persisted_receipts=%d reopened_receipts=%d", total, len(persisted), len(after))
}
