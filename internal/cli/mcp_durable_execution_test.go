package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
)

func TestDurableMCPRequestCompletesAcrossOwnedRuns(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		for _, lazy := range []bool{false, true} {
			t.Run(fmtMCPExecutionCase(parallel, lazy), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				peer := newMCPGenerationPeer(t, []string{"lookup"}, "actual remote result", false, false, false)
				cfg := newMCPListTestConfig(t, []config.MCPServerConfig{{Name: "catalog", URL: peer.server.URL}})
				cfg.Tools.Parallel = &parallel
				responses := []string{}
				if lazy {
					responses = append(responses, mcpPreflightToolResponse(t, "mcp_discover", `{"server":"catalog"}`))
				}
				responses = append(responses,
					mcpPreflightToolResponse(t, "mcp__catalog__lookup", `{}`),
					`{"choices":[{"message":{"role":"assistant","content":"first completed"}}]}`,
					`{"choices":[{"message":{"role":"assistant","content":"second completed"}}]}`,
				)
				provider, _ := newDurablePromptServer(t, responses...)
				defer provider.Close()
				cfg.Provider.BaseURL = provider.URL
				removeOwnedMCPListAuthority(t, cfg.Git.WorkDir)
				durable, err := newDurableCLISession(ctx, filepath.Join(cfg.Git.WorkDir, "sessions"), "mcp-execution", "", "")
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, durable.requestClose(context.Background())) })
				assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto), withDurableCLISession(durable))
				require.NoError(t, err)
				t.Cleanup(assembly.Cleanup)
				assembly.LoopAgent.WithToolSearchThreshold(0)
				if !lazy {
					require.NoError(t, assembly.LoadMCPServer(ctx, "catalog"))
				}
				var snapshot session.PreparedSessionSnapshot
				for _, prompt := range []string{"first", "second"} {
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
					snapshot, err = durable.snapshotSelected()
					require.NoError(t, err)
				}
				require.EqualValues(t, 1, peer.calls.Load(), "a durable tool call must reach the actual MCP peer once")
				remoteReceipt, proofs := 0, 0
				for _, row := range snapshot.DurableEntries {
					if row.ToolName != "mcp__catalog__lookup" {
						continue
					}
					if row.Type == session.EntryTypeTool && row.ActionState == "" {
						remoteReceipt++
						require.Equal(t, core.DispatchStateDispatched, row.DispatchState)
						require.Equal(t, string(core.ReceiptOutcomeSuccess), row.ReceiptOutcome)
						require.Contains(t, row.Content, "actual remote result")
					}
					if row.ActionState == session.ActionStateExecution && row.CompletionProof != "" {
						proofs++
					}
				}
				require.Equal(t, 1, remoteReceipt)
				require.Positive(t, proofs)
			})
		}
	}
}

func fmtMCPExecutionCase(parallel, lazy bool) string {
	name := "sequential"
	if parallel {
		name = "parallel"
	}
	if lazy {
		name += "-lazy"
	} else {
		name += "-activated"
	}
	return name
}

func TestDurableMCPUnknownRequestDoesNotReplayAfterResume(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		for _, mode := range []string{"cancel", "disconnect"} {
			t.Run(fmtMCPExecutionCase(parallel, false)+"-"+mode, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				peer := newMCPGenerationPeer(t, []string{"lookup"}, "not returned", false, false, false)
				started := make(chan struct{})
				original := peer.server.Config.Handler
				peer.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost {
						original.ServeHTTP(w, r)
						return
					}
					body, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					r.Body = io.NopCloser(bytes.NewReader(body))
					var request struct {
						Method string `json:"method"`
					}
					require.NoError(t, json.Unmarshal(body, &request))
					if request.Method != "tools/call" {
						original.ServeHTTP(w, r)
						return
					}
					peer.calls.Add(1)
					close(started)
					if mode == "cancel" {
						<-r.Context().Done()
						return
					}
					conn, _, err := w.(http.Hijacker).Hijack()
					require.NoError(t, err)
					require.NoError(t, conn.Close())
				})
				cfg := newMCPListTestConfig(t, []config.MCPServerConfig{{Name: "catalog", URL: peer.server.URL}})
				cfg.Tools.Parallel = &parallel
				provider, script := newDurablePromptServer(t, durableApprovalToolResponse(t, "unknown-call", "mcp__catalog__lookup", map[string]any{}))
				defer provider.Close()
				cfg.Provider.BaseURL = provider.URL
				removeOwnedMCPListAuthority(t, cfg.Git.WorkDir)
				directory := filepath.Join(cfg.Git.WorkDir, "sessions")
				durable, err := newDurableCLISession(ctx, directory, "mcp-unknown", "", "")
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, durable.requestClose(context.Background())) })
				assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto), withDurableCLISession(durable))
				require.NoError(t, err)
				t.Cleanup(assembly.Cleanup)
				assembly.LoopAgent.WithToolSearchThreshold(0)
				require.NoError(t, assembly.LoadMCPServer(ctx, "catalog"))
				runCtx, sink, err := durable.bindRun(ctx)
				require.NoError(t, err)
				callCtx, stopCall := context.WithCancel(runCtx)
				defer stopCall()
				stream, err := assembly.Harness.Submit(callCtx, "perform one remote action")
				require.NoError(t, err)
				if mode == "cancel" {
					select {
					case <-started:
						stopCall()
					case <-ctx.Done():
						t.Fatal("MCP call was not dispatched")
					}
				}
				for range stream.Events() {
				}
				_, err = stream.Result()
				require.Error(t, err)
				sink.CloseRunCommit()
				snapshot, err := durable.snapshotSelected()
				require.NoError(t, err)
				unknownReceipts := 0
				for _, row := range snapshot.DurableEntries {
					if row.ToolCallID != "unknown-call" {
						continue
					}
					if row.Type == session.EntryTypeTool && row.ActionState == "" {
						unknownReceipts++
						require.Equal(t, core.DispatchStateDispatched, row.DispatchState)
						require.Equal(t, string(core.ReceiptOutcomeUnknown), row.ReceiptOutcome)
					}
					require.Empty(t, row.CompletionProof, "unknown remote outcomes must retain the unresolved marker")
					require.Nil(t, row.ExecutionResolution)
				}
				require.Equal(t, 1, unknownReceipts)
				require.EqualValues(t, 1, peer.calls.Load())
				require.Len(t, script.requestBodies(), 1, "unknown outcomes must not solicit an automatic retry")
				require.NoError(t, durable.requestClose(ctx))
				reopened, err := newDurableCLISession(ctx, directory, "mcp-resumed", snapshot.SessionID, "")
				require.NoError(t, err)
				defer func() { require.NoError(t, reopened.requestClose(context.Background())) }()
				resumed, err := reopened.snapshotSelected()
				require.NoError(t, err)
				pending, err := session.InspectPendingActions(resumed)
				require.NoError(t, err)
				require.NotEmpty(t, pending, "the unknown action must remain available for operator reconciliation")
				require.EqualValues(t, 1, peer.calls.Load(), "resume must never replay tools/call")
				require.Len(t, script.requestBodies(), 1)
			})
		}
	}
}
