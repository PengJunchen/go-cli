package cli

import (
	"bytes"
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
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
)

func TestDurablePromptSubagentOwnsPhysicalSessionAndContext(t *testing.T) {
	for _, mode := range []string{"independent", "inherit"} {
		t.Run(mode, func(t *testing.T) {
			configureDurableCLISessionTest(t)
			t.Chdir(t.TempDir())
			server, script := newDurablePromptServer(t,
				`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"delegate-child","type":"function","function":{"name":"dispatch_subagent","arguments":"{\"prompt\":\"child task\",\"context_mode\":\"`+mode+`\",\"max_turns\":2}"}}]}}]}`,
				`{"choices":[{"message":{"role":"assistant","content":"child real answer"}}]}`,
				`{"choices":[{"message":{"role":"assistant","content":"parent real answer"}}]}`,
			)
			defer server.Close()
			store := filepath.Join(t.TempDir(), "store")
			out := &bytes.Buffer{}
			cmd := newPromptCmd(out)
			require.NoError(t, cmd.Run(t.Context(), durablePromptConfig(t, server.URL, store), []string{"-approve=auto", "parent current context canary"}))
			requests := script.requestBodies()
			require.Len(t, requests, 3, "parent delegates through the actual assembled tool and real provider")
			require.Contains(t, requests[1], "child task")
			if mode == "inherit" {
				require.Contains(t, requests[1], "parent current context canary")
			} else {
				require.NotContains(t, requests[1], "parent current context canary")
			}
			require.Contains(t, requests[2], "child real answer")
			parentID := physicalSessionIDFromOutput(t, out.String())
			files, err := filepath.Glob(filepath.Join(store, "sessions", "*.jsonl"))
			require.NoError(t, err)
			require.Len(t, files, 2, "each child owns a separate physical JSONL")
			for _, file := range files {
				id := strings.TrimSuffix(filepath.Base(file), ".jsonl")
				resumed, err := newDurableCLISession(context.Background(), store, "inspect", id, "")
				require.NoError(t, err, "all child and parent file locks must have been released")
				snapshot, err := resumed.snapshotSelected()
				require.NoError(t, err)
				require.NoError(t, resumed.requestClose(context.Background()))
				if id != parentID {
					require.NotEmpty(t, snapshot.PublishedView.Messages)
					require.Contains(t, snapshot.PublishedView.Messages[0].Content, "child task")
					require.Equal(t, "child real answer", snapshot.PublishedView.Messages[len(snapshot.PublishedView.Messages)-1].Content)
					data, err := os.ReadFile(file)
					require.NoError(t, err)
					require.Contains(t, string(data), "parent_session_id", "child input retains informational lineage")
					parentData, err := os.ReadFile(filepath.Join(store, "sessions", parentID+".jsonl"))
					require.NoError(t, err)
					foundReceipt := false
					for _, line := range strings.Split(strings.TrimSpace(string(parentData)), "\n") {
						var row session.SessionEntry
						require.NoError(t, json.Unmarshal([]byte(line), &row))
						if row.Type == session.EntryTypeTool && row.ActionState == "" && row.ToolName == "dispatch_subagent" {
							require.Contains(t, row.Content, "Child physical session ID (physical_session_id): "+id)
							foundReceipt = true
						}
					}
					require.True(t, foundReceipt, "the persisted parent receipt must expose the independently resumable child")
					for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
						var row struct {
							SessionID string `json:"session_id"`
						}
						require.NoError(t, json.Unmarshal([]byte(line), &row))
						require.Equal(t, id, row.SessionID)
					}
				}
			}
		})
	}
}

func TestDurablePromptParallelSubagentsInheritTopLevelContextWithoutPrompt(t *testing.T) {
	configureDurableCLISessionTest(t)
	t.Chdir(t.TempDir())
	server, script := newDurablePromptServer(t,
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"parallel","type":"function","function":{"name":"dispatch_subagent","arguments":"{\"parallel\":true,\"context_mode\":\"inherit\",\"tasks\":[{\"prompt\":\"first child\"},{\"prompt\":\"second child\"}]}"}}]}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"parallel child answer"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"parallel child answer"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"parent complete"}}]}`,
	)
	defer server.Close()
	store := filepath.Join(t.TempDir(), "store")
	require.NoError(t, newPromptCmd(io.Discard).Run(t.Context(), durablePromptConfig(t, server.URL, store), []string{"-approve=auto", "parallel inherited canary"}))
	requests := script.requestBodies()
	require.Len(t, requests, 4)
	for _, request := range requests[1:3] {
		require.Contains(t, request, "parallel inherited canary")
	}
	files, err := filepath.Glob(filepath.Join(store, "sessions", "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, files, 3)
}

func TestDurableSubagentWriteLivesOnlyInChildAndResumesExactly(t *testing.T) {
	configureDurableCLISessionTest(t)
	workspace := t.TempDir()
	t.Chdir(workspace)
	target := filepath.Join(workspace, "child-marker.txt")
	args, err := json.Marshal(map[string]any{"path": target, "content": "child persisted effect"})
	require.NoError(t, err)
	encoded, err := json.Marshal(string(args))
	require.NoError(t, err)
	server, script := newDurablePromptServer(t,
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"delegate","type":"function","function":{"name":"dispatch_subagent","arguments":"{\"prompt\":\"write child marker\",\"tools\":[\"write\"]}"}}]}}]}`,
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"child-write","type":"function","function":{"name":"write","arguments":`+string(encoded)+`}}]}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"child wrote marker"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"parent finished"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"child resumed exactly"}}]}`,
	)
	defer server.Close()
	store := filepath.Join(t.TempDir(), "store")
	cfg := durablePromptConfig(t, server.URL, store)
	cfg.Sandbox.AllowedPaths = []string{workspace}
	out := &bytes.Buffer{}
	runErr := newPromptCmd(out).Run(t.Context(), cfg, []string{"-approve=auto", "delegate write"})
	if runErr != nil {
		paths, _ := filepath.Glob(filepath.Join(store, "sessions", "*.jsonl"))
		for _, path := range paths {
			data, _ := os.ReadFile(path)
			for _, line := range strings.Split(string(data), "\n") {
				if strings.Contains(line, `"type":"tool"`) || strings.Contains(line, `"record_kind":"action`) {
					t.Logf("durable evidence %s", line)
				}
			}
		}
	}
	require.NoError(t, runErr)
	data, err := os.ReadFile(target)
	if err != nil {
		for _, body := range script.requestBodies() {
			var decoded map[string]json.RawMessage
			_ = json.Unmarshal([]byte(body), &decoded)
			t.Logf("provider messages: %s", decoded["messages"])
		}
	}
	require.NoError(t, err)
	require.Equal(t, "child persisted effect", string(data))
	parentID := physicalSessionIDFromOutput(t, out.String())
	files, err := filepath.Glob(filepath.Join(store, "sessions", "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, files, 2)
	var childID string
	for _, file := range files {
		id := strings.TrimSuffix(filepath.Base(file), ".jsonl")
		rows, err := os.ReadFile(file)
		require.NoError(t, err)
		if id == parentID {
			require.NotContains(t, string(rows), `"tool_call_id":"child-write"`)
		} else {
			childID = id
			require.Contains(t, string(rows), `"tool_call_id":"child-write"`)
		}
	}
	require.NotEmpty(t, childID)
	require.NoError(t, newPromptCmd(io.Discard).Run(t.Context(), cfg, []string{"-approve=auto", "--resume-session", childID, "continue this child"}))
	requests := script.requestBodies()
	require.Len(t, requests, 5)
	require.Contains(t, requests[4], "child-write")
	require.Contains(t, requests[4], "child wrote marker")
	require.NotContains(t, requests[4], "parent finished")
}

func TestManagedSubagentSendAcrossMultipleRunsAndFinalSeal(t *testing.T) {
	configureDurableCLISessionTest(t)
	t.Chdir(t.TempDir())
	var mu sync.Mutex
	var bodies []string
	started := make(chan int, 3)
	release := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		mu.Lock()
		index := len(bodies)
		bodies = append(bodies, string(body))
		mu.Unlock()
		if index >= len(release) {
			t.Errorf("unexpected request %d", index)
			http.Error(w, "unexpected", 500)
			return
		}
		started <- index
		select {
		case <-release[index]:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"follow-up %d complete"}}]}`, index)
	}))
	defer server.Close()
	state := newManagedSubagentTestState(t, durablePromptConfig(t, server.URL, filepath.Join(t.TempDir(), "store")), ApproveAuto)
	factory := core.NewManagedSubAgentFactory(state.runManagedSubAgent)
	sub, err := factory.Create(t.Context(), "send-child", core.SubAgentConfig{})
	require.NoError(t, err)
	events, err := sub.Run(t.Context(), "first child request")
	require.NoError(t, err)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range events {
		}
	}()
	for i := 0; i < 3; i++ {
		select {
		case index := <-started:
			require.Equal(t, i, index)
		case <-time.After(5 * time.Second):
			t.Fatal("child provider did not start")
		}
		if i < 2 {
			require.NoError(t, sub.Send(t.Context(), []string{"second child request", "third child request"}[i]))
		}
		close(release[i])
	}
	_, err = sub.Wait(t.Context())
	require.NoError(t, err)
	<-drained
	require.ErrorIs(t, sub.Send(t.Context(), "too late"), core.ErrSubAgentNotRunning)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, bodies, 3)
	require.Contains(t, bodies[1], "second child request")
	require.Contains(t, bodies[2], "third child request")
	childID := sub.(interface{ PhysicalSessionID() string }).PhysicalSessionID()
	require.NotEmpty(t, childID)
	resumed, err := newDurableCLISession(t.Context(), state.durableSession.storeDirectory, "inspect", childID, "")
	require.NoError(t, err)
	snapshot, err := resumed.snapshotSelected()
	require.NoError(t, err)
	require.Len(t, snapshot.PublishedView.Messages, 6)
	require.NoError(t, resumed.requestClose(t.Context()))
}

func newManagedSubagentTestState(t *testing.T, cfg *config.Config, mode ApproveMode) *assembleState {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	durable, err := newDurableCLISession(ctx, cfg.Session.StorePath, "test-parent", "", "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, durable.requestClose(context.Background())) })
	return &assembleState{rc: cfg, ac: assembleConfig{maxTokens: defaultMaxTokens, approveMode: mode}, providerName: "test", modelName: "test-model", planCtrl: core.NewDefaultPlanModeController(), lifetimeCtx: ctx, durableSession: durable, managedChildren: newManagedChildGroup()}
}

func TestManagedSubagentParentCloseCancelsAndJoinsChild(t *testing.T) {
	configureDurableCLISessionTest(t)
	t.Chdir(t.TempDir())
	started := make(chan struct{})
	requestDone := make(chan struct{})
	releaseHandler := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
		case <-releaseHandler:
		}
		close(requestDone)
	}))
	defer server.Close()
	defer close(releaseHandler)
	store := filepath.Join(t.TempDir(), "store")
	cfg := durablePromptConfig(t, server.URL, store)
	durable, err := newDurableCLISession(t.Context(), store, "parent", "", "")
	require.NoError(t, err)
	assembly, err := AssembleAgent(t.Context(), cfg, "test", "test-model", io.Discard, WithApproveMode(ApproveAuto), withDurableCLISession(durable))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	dispatched := make(chan error, 1)
	go func() {
		_, err := assembly.Dispatcher.Dispatch(t.Context(), core.SubagentTask{ID: "cancelled-child", Prompt: "wait for cancellation"})
		dispatched <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not start")
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, assembly.Close(closeCtx))
	select {
	case err := <-dispatched:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("dispatcher did not join cancelled child")
	}
	select {
	case <-requestDone:
	case <-time.After(5 * time.Second):
		t.Fatal("provider input remained live")
	}
	files, err := filepath.Glob(filepath.Join(store, "sessions", "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, files, 2)
	for _, file := range files {
		id := strings.TrimSuffix(filepath.Base(file), ".jsonl")
		resumed, err := newDurableCLISession(t.Context(), store, "inspect", id, "")
		require.NoError(t, err)
		require.NoError(t, resumed.requestClose(t.Context()))
	}
}

func TestManagedSubagentChildApprovalAndWhitelistDenyEffects(t *testing.T) {
	for _, mode := range []string{"approval", "whitelist", "plan"} {
		t.Run(mode, func(t *testing.T) {
			configureDurableCLISessionTest(t)
			workspace := t.TempDir()
			t.Chdir(workspace)
			target := filepath.Join(workspace, "must-not-exist")
			args, err := json.Marshal(map[string]any{"path": target, "content": "forbidden effect"})
			require.NoError(t, err)
			encoded, err := json.Marshal(string(args))
			require.NoError(t, err)
			server, script := newDurablePromptServer(t, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"denied-write","type":"function","function":{"name":"write","arguments":`+string(encoded)+`}}]}}]}`, `{"choices":[{"message":{"role":"assistant","content":"denial observed"}}]}`)
			defer server.Close()
			cfg := durablePromptConfig(t, server.URL, filepath.Join(t.TempDir(), "store"))
			cfg.Sandbox.AllowedPaths = []string{workspace}
			approve := ApproveAuto
			if mode == "approval" {
				approve = ApproveDeny
			}
			state := newManagedSubagentTestState(t, cfg, approve)
			if mode == "plan" {
				require.NoError(t, state.planCtrl.Enter(t.Context(), "parent research only"))
			}
			factory := core.NewManagedSubAgentFactory(state.runManagedSubAgent)
			task := core.SubAgentConfig{Name: "restricted-child"}
			if mode == "whitelist" {
				task.Tools = []string{"read"}
			}
			sub, err := factory.Create(t.Context(), task.Name, task)
			require.NoError(t, err)
			events, err := sub.Run(t.Context(), "try the write")
			require.NoError(t, err)
			for range events {
			}
			_, err = sub.Wait(t.Context())
			if mode == "plan" {
				require.ErrorIs(t, err, core.ErrPlanModeBlocked)
				require.Empty(t, script.requestBodies())
			} else if mode == "whitelist" {
				require.ErrorContains(t, err, "not disclosed")
				require.Len(t, script.requestBodies(), 1)
			} else {
				require.NoError(t, err)
				require.Len(t, script.requestBodies(), 2)
			}
			_, err = os.Stat(target)
			require.True(t, os.IsNotExist(err))
		})
	}
}

func TestDurableSubagentOwnsLazyMCPConnectionAndExecution(t *testing.T) {
	for _, discover := range []bool{false, true} {
		name := "explicit-remote-whitelist"
		if discover {
			name = "child-discovery"
		}
		t.Run(name, func(t *testing.T) {
			configureDurableCLISessionTest(t)
			peer := newMCPGenerationPeer(t, []string{"lookup"}, "child actual MCP result", false, false, false)
			cfg := newMCPListTestConfig(t, []config.MCPServerConfig{{Name: "catalog", URL: peer.server.URL}})
			allowed := []string{"mcp__catalog__lookup"}
			if discover {
				allowed = append(allowed, "mcp_discover")
			}
			responses := []string{durableApprovalToolResponse(t, "delegate-mcp", "dispatch_subagent", map[string]any{"prompt": "ask the child MCP peer", "tools": allowed})}
			if discover {
				responses = append(responses, durableApprovalToolResponse(t, "child-discover", "mcp_discover", map[string]any{"server": "catalog"}))
			}
			responses = append(responses, durableApprovalToolResponse(t, "child-remote", "mcp__catalog__lookup", map[string]any{}), `{"choices":[{"message":{"role":"assistant","content":"child MCP complete"}}]}`, `{"choices":[{"message":{"role":"assistant","content":"parent MCP complete"}}]}`)
			provider, script := newDurablePromptServer(t, responses...)
			defer provider.Close()
			cfg.Provider.BaseURL = provider.URL
			cfg.Session.StorePath = filepath.Join(t.TempDir(), "sessions")
			require.NoError(t, newPromptCmd(io.Discard).Run(t.Context(), cfg, []string{"-approve=auto", "delegate remote request"}))
			require.EqualValues(t, 1, peer.calls.Load(), "the independent child must reach the real MCP HTTP peer")
			requests := script.requestBodies()
			require.Len(t, requests, len(responses))
			files, err := filepath.Glob(filepath.Join(cfg.Session.StorePath, "sessions", "*.jsonl"))
			require.NoError(t, err)
			require.Len(t, files, 2)
			remoteJournals := 0
			for _, file := range files {
				data, err := os.ReadFile(file)
				require.NoError(t, err)
				if strings.Contains(string(data), `"tool_name":"mcp__catalog__lookup"`) {
					remoteJournals++
					require.Contains(t, string(data), "child actual MCP result")
				}
			}
			require.Equal(t, 1, remoteJournals, "only the child's own physical session owns the remote effect")
		})
	}
}
