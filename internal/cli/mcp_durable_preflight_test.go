package cli

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
)

func TestDurableMCPUnsupportedDispatchContinuesWithoutNewConnection(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		for _, remote := range []bool{false, true} {
			name := "sequential-activation"
			if parallel {
				name = "parallel-activation"
			}
			if remote {
				name += "-remote"
			}
			t.Run(name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				peer := newMCPGenerationPeer(t, []string{"lookup"}, "must never be called", false, false, false)
				cfg := newMCPListTestConfig(t, []config.MCPServerConfig{{Name: "catalog", URL: peer.server.URL}})
				cfg.Tools.Parallel = &parallel
				blockedName, blockedArgs := "mcp_discover", `{"server":"catalog"}`
				if remote {
					blockedName, blockedArgs = "mcp__catalog__lookup", `{}`
				}
				responses := []string{
					mcpPreflightToolResponse(t, blockedName, blockedArgs),
					`{"choices":[{"message":{"role":"assistant","content":"first blocked completed"}}]}`,
					`{"choices":[{"message":{"role":"assistant","content":"second continued completed"}}]}`,
				}
				provider, script := newDurablePromptServer(t, responses...)
				defer provider.Close()
				cfg.Provider.BaseURL = provider.URL
				removeOwnedMCPListAuthority(t, cfg.Git.WorkDir)
				durable, err := newDurableCLISession(ctx, filepath.Join(cfg.Git.WorkDir, "sessions"), "mcp-preflight", "", "")
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, durable.requestClose(context.Background())) })
				assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto), withDurableCLISession(durable))
				require.NoError(t, err)
				t.Cleanup(assembly.Cleanup)
				assembly.LoopAgent.WithToolSearchThreshold(0)
				if remote {
					require.NoError(t, assembly.LoadMCPServer(ctx, "catalog"), "an explicit idle activation precedes the remote-call preflight")
				}
				beforeRequests := peer.requests.Load()
				loaded, watchers := len(assembly.progressiveMCP.loaded), len(assembly.progressiveMCP.watchers)
				var physicalID string
				var published session.PreparedSessionSnapshot
				for _, prompt := range []string{"first blocked", "second continued"} {
					runCtx, sink, err := durable.bindRun(ctx)
					require.NoError(t, err)
					require.NoError(t, hydrateDurableRunHistory(assembly.Agent, sink))
					stream, err := assembly.Harness.Submit(runCtx, prompt)
					require.NoError(t, err)
					for range stream.Events() {
					}
					result, err := stream.Result()
					require.NoError(t, err)
					require.Equal(t, prompt+" completed", result.Content)
					sink.CloseRunCommit()
					published, err = durable.snapshotSelected()
					require.NoError(t, err)
					if physicalID == "" {
						physicalID = published.SessionID
					}
					require.Equal(t, physicalID, published.SessionID)
				}
				blockedReceipts, terminals := 0, 0
				for _, entry := range published.DurableEntries {
					if entry.CheckpointKind == session.CheckpointRunTerminal {
						terminals++
						require.Equal(t, core.RunOutcomeSuccess, entry.RunOutcome)
					}
					if entry.ToolCallID != "blocked-call" {
						continue
					}
					require.Empty(t, entry.ActionState, "unsupported calls never reserve an intent or execution")
					require.Empty(t, entry.ActionID)
					require.Equal(t, core.DispatchStateNotDispatched, entry.DispatchState)
					require.Equal(t, string(core.ReceiptOutcomeError), entry.ReceiptOutcome)
					require.Contains(t, entry.Content, "completion")
					blockedReceipts++
				}
				require.Equal(t, 1, blockedReceipts)
				require.Equal(t, 2, terminals)
				require.Len(t, script.requestBodies(), 3)
				require.Equal(t, beforeRequests, peer.requests.Load(), "durable unsupported dispatch performs no MCP I/O")
				require.Zero(t, peer.calls.Load())
				require.Len(t, assembly.progressiveMCP.loaded, loaded)
				require.Len(t, assembly.progressiveMCP.watchers, watchers)
				if !remote {
					require.Zero(t, beforeRequests, "activation rejection happens before Connect or tools/list")
				}
			})
		}
	}
}

func mcpPreflightToolResponse(t *testing.T, blockedName, blockedArgs string) string {
	t.Helper()
	response, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
		"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "blocked-call", "type": "function", "function": map[string]any{"name": blockedName, "arguments": blockedArgs}},
			map[string]any{"id": "safe-list", "type": "function", "function": map[string]any{"name": "mcp_list", "arguments": "{}"}},
		},
	}}}})
	require.NoError(t, err)
	return string(response)
}
