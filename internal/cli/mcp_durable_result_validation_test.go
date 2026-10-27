package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
)

const mcpInvalidResultEnv = "GO_CLI_TEST_MCP_INVALID_RESULT"
const mcpInvalidResultWireEnv = "GO_CLI_TEST_MCP_INVALID_RESULT_WIRE"

func TestMCPInvalidToolResultStdioPeer(t *testing.T) {
	rawResult := os.Getenv(mcpInvalidResultEnv)
	if rawResult == "" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			continue
		}
		if len(request.ID) == 0 {
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "validation-peer", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "lookup", "inputSchema": mockMCPInputSchema()}}}
		case "tools/call":
			file, err := os.OpenFile(os.Getenv(mcpInvalidResultWireEnv), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.WriteString(file, "tools/call\n")
			if closeErr := file.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", request.ID, rawResult)
			if err != nil {
				t.Fatal(err)
			}
			continue
		default:
			continue
		}
		frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintln(os.Stdout, string(frame)); err != nil {
			t.Fatal(err)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestDurableMCPMalformedResultRemainsUnknownAfterResume(t *testing.T) {
	for _, transport := range []string{"json", "sse", "stdio"} {
		for index, rawResult := range []string{`{}`, `{"content":[],"isError":"false"}`, `{"content":[{"type":"text","text":3}]}`, `{"content":[],"content":[]}`, `{"content":[],"IsError":false}`,
			`{"content":[],"_meta":42}`,
			`{"content":[{"type":"text","text":"ok","annotations":{"priority":"high"}}]}`,
			`{"content":[{"type":"resource_link","uri":"x://x","name":"ok","size":"huge"}]}`} {
			t.Run(fmt.Sprintf("%s/%d", transport, index), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				wireFile := filepath.Join(t.TempDir(), "calls.log")
				var calls func() int
				var server config.MCPServerConfig
				if transport == "stdio" {
					executable, err := os.Executable()
					require.NoError(t, err)
					server = config.MCPServerConfig{Name: "catalog", Command: "./mcp-test-peer.sh", Args: []string{"-test.run=^TestMCPInvalidToolResultStdioPeer$"}, Env: map[string]string{mcpInvalidResultEnv: rawResult, mcpInvalidResultWireEnv: wireFile, "GO_CLI_TEST_MCP_EXECUTABLE": executable}}
					calls = func() int {
						body, err := os.ReadFile(wireFile)
						require.NoError(t, err)
						return strings.Count(string(body), "tools/call")
					}
				} else {
					peer := newMCPGenerationPeer(t, []string{"lookup"}, "unused", false, false, false)
					original := peer.server.Config.Handler
					peer.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method != http.MethodPost {
							original.ServeHTTP(w, r)
							return
						}
						body, err := io.ReadAll(r.Body)
						if err != nil {
							http.Error(w, "read", 400)
							return
						}
						r.Body = io.NopCloser(bytes.NewReader(body))
						var request struct {
							ID     json.RawMessage `json:"id"`
							Method string          `json:"method"`
						}
						if json.Unmarshal(body, &request) != nil || request.Method != "tools/call" {
							original.ServeHTTP(w, r)
							return
						}
						peer.calls.Add(1)
						w.Header().Set("Mcp-Session-Id", "generation-session")
						frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, request.ID, rawResult)
						if transport == "sse" {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = fmt.Fprintf(w, "event:message\ndata:%s\n\n", frame)
						} else {
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(w, frame)
						}
					})
					server = config.MCPServerConfig{Name: "catalog", URL: peer.server.URL}
					calls = func() int { return int(peer.calls.Load()) }
				}
				cfg := newMCPListTestConfig(t, []config.MCPServerConfig{server}, func(root string) {
					if transport == "stdio" {
						require.NoError(t, os.WriteFile(filepath.Join(root, "mcp-test-peer.sh"), []byte("#!/bin/sh\nexec \"$GO_CLI_TEST_MCP_EXECUTABLE\" \"$@\"\n"), 0700))
					}
				})
				provider, script := newDurablePromptServer(t, durableApprovalToolResponse(t, "malformed-call", "mcp__catalog__lookup", map[string]any{}))
				defer provider.Close()
				cfg.Provider.BaseURL = provider.URL
				removeOwnedMCPListAuthority(t, cfg.Git.WorkDir)
				directory := filepath.Join(cfg.Git.WorkDir, "sessions")
				durable, err := newDurableCLISession(ctx, directory, "malformed-result", "", "")
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, durable.requestClose(context.Background())) })
				assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto), withDurableCLISession(durable))
				require.NoError(t, err)
				t.Cleanup(assembly.Cleanup)
				assembly.LoopAgent.WithToolSearchThreshold(0)
				require.NoError(t, assembly.LoadMCPServer(ctx, "catalog"))
				runCtx, sink, err := durable.bindRun(ctx)
				require.NoError(t, err)
				stream, err := assembly.Harness.Submit(runCtx, "perform one action")
				require.NoError(t, err)
				for range stream.Events() {
				}
				_, err = stream.Result()
				require.Error(t, err)
				sink.CloseRunCommit()
				snapshot, err := durable.snapshotSelected()
				require.NoError(t, err)
				receipts := 0
				for _, row := range snapshot.DurableEntries {
					if row.ToolCallID != "malformed-call" {
						continue
					}
					require.Empty(t, row.CompletionProof)
					require.Nil(t, row.ExecutionResolution)
					if row.Type == session.EntryTypeTool && row.ActionState == "" {
						receipts++
						require.Equal(t, core.DispatchStateDispatched, row.DispatchState)
						require.Equal(t, string(core.ReceiptOutcomeUnknown), row.ReceiptOutcome)
					}
				}
				require.Equal(t, 1, receipts)
				require.Equal(t, 1, calls())
				require.Len(t, script.requestBodies(), 1)
				require.NoError(t, durable.requestClose(ctx))
				reopened, err := newDurableCLISession(ctx, directory, "malformed-resumed", snapshot.SessionID, "")
				require.NoError(t, err)
				defer func() { require.NoError(t, reopened.requestClose(context.Background())) }()
				resumed, err := reopened.snapshotSelected()
				require.NoError(t, err)
				pending, err := session.InspectPendingActions(resumed)
				require.NoError(t, err)
				require.NotEmpty(t, pending)
				require.Equal(t, 1, calls(), "resume must not replay an untrusted tools/call result")
				require.Len(t, script.requestBodies(), 1)
			})
		}
	}
}
