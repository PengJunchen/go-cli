package cli

import (
	"bytes"
	"context"
	"encoding/json"
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

	"github.com/pengjunchen/go-cli/internal/session"
)

func sequentialCLIResponse(t *testing.T, id, name string, args map[string]any) string {
	t.Helper()
	inner, err := json.Marshal(args)
	require.NoError(t, err)
	body, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(inner)}}}}}}})
	require.NoError(t, err)
	return string(body)
}

type sequentialRoleRequest struct {
	role string
	body string
}

// Route by the actual physical child's current user prompt and tool receipt,
// rather than assigning responses by a global request index.
func sequentialRoleServer(t *testing.T, target string) (*httptest.Server, func() []sequentialRoleRequest) {
	t.Helper()
	parent := sequentialCLIResponse(t, "parent-seq", "dispatch_subagent", map[string]any{"execution_mode": "sequential", "tasks": []any{map[string]any{"prompt": "writer stage", "role": "tester", "tools": []string{"write"}}, map[string]any{"prompt": "reader stage", "role": "researcher", "tools": []string{"grep"}}}})
	write := sequentialCLIResponse(t, "child-write", "write", map[string]any{"path": target, "content": "L2_SEQUENTIAL_CANARY\n"})
	grep := sequentialCLIResponse(t, "child-grep", "grep", map[string]any{"path": ".", "pattern": "L2_SEQUENTIAL_CANARY"})
	var mu sync.Mutex
	var requests []sequentialRoleRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read request", http.StatusBadRequest)
			return
		}
		var request struct {
			Messages []struct {
				Role       string `json:"role"`
				Content    any    `json:"content"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			http.Error(w, "decode request", http.StatusBadRequest)
			return
		}
		role, hasReceipt := "parent", false
		for _, m := range request.Messages {
			content, _ := m.Content.(string)
			if m.Role == "user" && strings.Contains(content, "writer stage") {
				role = "writer"
			}
			if m.Role == "user" && strings.Contains(content, "reader stage") {
				role = "reader"
			}
		}
		for _, m := range request.Messages {
			if m.Role == "tool" && ((role == "parent" && m.ToolCallID == "parent-seq") || (role == "writer" && m.ToolCallID == "child-write") || (role == "reader" && m.ToolCallID == "child-grep")) {
				hasReceipt = true
			}
		}
		mu.Lock()
		requests = append(requests, sequentialRoleRequest{role: role, body: string(body)})
		mu.Unlock()
		response := parent
		if role == "writer" {
			response = write
			if hasReceipt {
				response = `{"choices":[{"message":{"role":"assistant","content":"WRITER_PRIVATE_ANSWER"}}]}`
			}
		}
		if role == "reader" {
			response = grep
			if hasReceipt {
				response = `{"choices":[{"message":{"role":"assistant","content":"reader finished"}}]}`
			}
		}
		if role == "parent" && hasReceipt {
			response = `{"choices":[{"message":{"role":"assistant","content":"parent finished"}}]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response)
	}))
	return server, func() []sequentialRoleRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]sequentialRoleRequest(nil), requests...)
	}
}

func TestSequentialManagedChildrenDurableWriterThenGrep(t *testing.T) {
	configureDurableCLISessionTest(t)
	workspace := t.TempDir()
	t.Chdir(workspace)
	target := filepath.Join(workspace, "marker.txt")
	store := filepath.Join(t.TempDir(), "store")
	server, getRequests := sequentialRoleServer(t, target)
	defer server.Close()
	cfg := durablePromptConfig(t, server.URL, store)
	cfg.Sandbox.AllowedPaths = []string{workspace}
	out := &bytes.Buffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := newPromptCmd(out).Run(ctx, cfg, []string{"-approve=auto", "parent sequential contract"})
	require.NoError(t, err)
	requests := getRequests()
	require.Len(t, requests, 6, "parent plus two real sequential children require six model requests")
	var roles []string
	for _, request := range requests {
		roles = append(roles, request.role)
	}
	require.Equal(t, []string{"parent", "writer", "writer", "reader", "reader", "parent"}, roles)
	require.NotContains(t, requests[3].body, "WRITER_PRIVATE_ANSWER", "child answers are not automatically injected into later child")
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "L2_SEQUENTIAL_CANARY\n", string(data))
	parentID := physicalSessionIDFromOutput(t, out.String())
	files, err := filepath.Glob(filepath.Join(store, "sessions", "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, files, 3, "parent and each managed child need independent physical sessions")
	type actionRef struct {
		SessionID  string `json:"session_id"`
		ActionID   string `json:"action_id"`
		ToolCallID string `json:"tool_call_id"`
	}
	type indexAction struct {
		Action      actionRef `json:"action"`
		NonBlocking bool      `json:"non_blocking_delegation"`
	}
	type indexRow struct {
		Seq       int           `json:"seq"`
		Kind      string        `json:"kind"`
		SessionID string        `json:"session_id"`
		Actions   []indexAction `json:"actions"`
		Action    *actionRef    `json:"action"`
	}
	type actionJournal struct {
		intent, proof, receipt, resolution, proofEntry string
	}
	journal := map[string]*actionJournal{
		"child-write": {},
		"child-grep":  {},
	}
	var writerID, readerID string
	for _, f := range files {
		id := strings.TrimSuffix(filepath.Base(f), ".jsonl")
		raw, err := os.ReadFile(f)
		require.NoError(t, err)
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			var row session.SessionEntry
			require.NoError(t, json.Unmarshal([]byte(line), &row))
			if evidence := journal[row.ToolCallID]; evidence != nil {
				switch {
				case row.ActionState == session.ActionStateIntent:
					evidence.intent = row.ActionID
				case row.ActionState == session.ActionStateExecution && row.CompletionProof != "":
					evidence.proof = row.ActionID
					evidence.proofEntry = row.ID
				case row.ReceiptOutcome != "":
					evidence.receipt = row.ActionID
				case row.CheckpointKind == session.CheckpointExecutionResolution && row.ExecutionResolution != nil:
					evidence.resolution = row.ExecutionResolution.ActionID
					require.Equal(t, row.ActionID, row.ExecutionResolution.ActionID)
					require.Equal(t, evidence.proofEntry, row.ExecutionResolution.EvidenceRef)
				}
			}
			if row.Type == session.EntryTypeTool && row.ReceiptOutcome != "" && row.ToolCallID == "child-write" {
				writerID = id
				require.NotEmpty(t, row.ActionID)
				require.Equal(t, "success", string(row.ReceiptOutcome))
			}
			if row.Type == session.EntryTypeTool && row.ReceiptOutcome != "" && row.ToolCallID == "child-grep" {
				readerID = id
				require.NotEmpty(t, row.ActionID)
				require.Equal(t, "success", string(row.ReceiptOutcome))
				require.Contains(t, row.Content, "L2_SEQUENTIAL_CANARY")
			}
		}
	}
	for _, callID := range []string{"child-write", "child-grep"} {
		evidence := journal[callID]
		require.NotEmpty(t, evidence.intent, callID)
		require.NotEmpty(t, evidence.proofEntry, callID)
		require.Equal(t, evidence.intent, evidence.proof, callID)
		require.Equal(t, evidence.intent, evidence.receipt, callID)
		require.Equal(t, evidence.intent, evidence.resolution, callID)
	}
	require.NotEmpty(t, writerID)
	require.NotEmpty(t, readerID)
	require.NotEqual(t, parentID, writerID)
	require.NotEqual(t, parentID, readerID)
	require.NotEqual(t, writerID, readerID)
	configDir, err := os.UserConfigDir()
	require.NoError(t, err)
	indexes, err := filepath.Glob(filepath.Join(configDir, "go-cli", "workspace-quarantine", "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, indexes, 1)
	raw, err := os.ReadFile(indexes[0])
	require.NoError(t, err)
	var writerDispatch, writerResolved, readerDispatch, readerResolved int
	var parentNonBlocking bool
	var writerActionID, readerActionID string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var r indexRow
		require.NoError(t, json.Unmarshal([]byte(line), &r))
		if r.Kind == "dispatch_ready" {
			for _, a := range r.Actions {
				switch a.Action.ToolCallID {
				case "parent-seq":
					parentNonBlocking = a.NonBlocking && a.Action.SessionID == parentID
				case "child-write":
					writerDispatch = r.Seq
					writerActionID = a.Action.ActionID
					require.Equal(t, writerID, a.Action.SessionID)
				case "child-grep":
					readerDispatch = r.Seq
					readerActionID = a.Action.ActionID
					require.Equal(t, readerID, a.Action.SessionID)
				}
			}
		}
		if r.Kind == "execution_resolution" && r.Action != nil && r.Action.ToolCallID == "child-write" {
			writerResolved = r.Seq
			require.Equal(t, writerID, r.Action.SessionID)
			require.Equal(t, writerActionID, r.Action.ActionID)
		}
		if r.Kind == "execution_resolution" && r.Action != nil && r.Action.ToolCallID == "child-grep" {
			readerResolved = r.Seq
			require.Equal(t, readerID, r.Action.SessionID)
			require.Equal(t, readerActionID, r.Action.ActionID)
		}
	}
	require.True(t, parentNonBlocking, "parent delegation marker is nonblocking")
	require.NotEmpty(t, writerActionID)
	require.NotEmpty(t, readerActionID)
	require.Greater(t, writerResolved, writerDispatch)
	require.Greater(t, readerDispatch, writerResolved, "writer durable resolution must precede reader dispatch")
	require.Equal(t, journal["child-write"].intent, writerActionID)
	require.Equal(t, journal["child-grep"].intent, readerActionID)
	require.Greater(t, readerResolved, readerDispatch)
	for _, f := range files {
		id := strings.TrimSuffix(filepath.Base(f), ".jsonl")
		resumed, err := newDurableCLISession(ctx, store, "inspect", id, "")
		require.NoError(t, err)
		snapshot, err := resumed.snapshotSelected()
		require.NoError(t, err)
		pending, err := session.InspectPendingActions(snapshot)
		require.NoError(t, err)
		require.Empty(t, pending)
		require.NoError(t, resumed.requestClose(context.Background()))
	}
}
