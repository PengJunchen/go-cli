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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/verify"
)

type processSessionLongRunWriter struct {
	mu sync.Mutex
	bytes.Buffer
}

func (writer *processSessionLongRunWriter) Write(value []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.Buffer.Write(value)
}

func (writer *processSessionLongRunWriter) String() string {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.Buffer.String()
}

type processSessionLongRunProvider struct {
	mu        sync.Mutex
	requests  []string
	turn      int
	sessionID string
	errors    []string
}

func newProcessSessionLongRunProvider(t *testing.T) (*httptest.Server, *processSessionLongRunProvider) {
	t.Helper()
	provider := &processSessionLongRunProvider{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read provider request", http.StatusBadRequest)
			return
		}
		var request struct {
			Messages []struct {
				Role       string          `json:"role"`
				Content    json.RawMessage `json:"content"`
				ToolCallID string          `json:"tool_call_id"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			http.Error(w, "decode provider request", http.StatusBadRequest)
			return
		}

		provider.mu.Lock()
		provider.requests = append(provider.requests, string(body))
		response := `{"choices":[{"message":{"role":"assistant","content":"[]"}}]}`
		if !strings.Contains(string(body), "Analyze the following conversation") {
			switch provider.turn {
			case 0:
				response = durableApprovalToolResponse(t, "process-start", "process_session", map[string]any{
					"operation": "start",
					"command":   "printf '%s' \"$$\" > " + shellSingleQuote("session-leader.pid") + "; yes x | head -c 50000; exec sleep 60",
				})
			case 1:
				provider.sessionID = processSessionIDFromProviderMessages(t, request.Messages)
				response = durableApprovalToolResponse(t, "process-read", "process_session", map[string]any{
					"operation": "read", "session_id": provider.sessionID, "offset": 0, "max_bytes": 60000,
				})
			case 2:
				if provider.sessionID == "" || !strings.Contains(string(body), "Active process sessions (use these exact IDs with process_session read, status, or stop)") || !strings.Contains(string(body), provider.sessionID) {
					provider.errors = append(provider.errors, "compacted provider request lost the manager-issued session ID roster")
				}
				response = durableApprovalToolResponse(t, "process-read-after-compaction", "process_session", map[string]any{
					"operation": "read", "session_id": provider.sessionID, "offset": 0, "max_bytes": 128,
				})
			case 3:
				response = durableApprovalToolResponse(t, "process-status", "process_session", map[string]any{
					"operation": "status", "session_id": provider.sessionID,
				})
			case 4:
				response = durableApprovalToolResponse(t, "ordinary-bash", "bash", map[string]any{"command": "printf ordinary-bash-ok"})
			case 5:
				response = `{"choices":[{"message":{"role":"assistant","content":"process session lifecycle complete"},"finish_reason":"stop"}]}`
			default:
				provider.errors = append(provider.errors, fmt.Sprintf("unexpected model request %d", provider.turn))
			}
			provider.turn++
		}
		provider.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, response); err != nil {
			provider.mu.Lock()
			provider.errors = append(provider.errors, "write provider response: "+err.Error())
			provider.mu.Unlock()
		}
	}))
	return server, provider
}

func processSessionIDFromProviderMessages(t *testing.T, messages []struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
}) string {
	t.Helper()
	for _, message := range messages {
		if message.Role != "tool" || message.ToolCallID != "process-start" {
			continue
		}
		var output string
		require.NoError(t, json.Unmarshal(message.Content, &output))
		var result struct {
			SessionID string `json:"session_id"`
		}
		require.NoError(t, json.Unmarshal([]byte(output), &result))
		require.NotEmpty(t, result.SessionID)
		return result.SessionID
	}
	t.Fatal("provider request omitted the process-session start receipt")
	return ""
}

func (provider *processSessionLongRunProvider) snapshot() ([]string, []string, string, int) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]string(nil), provider.requests...), append([]string(nil), provider.errors...), provider.sessionID, provider.turn
}

func TestProcessSessionSurvivesCompactionFinalHandoffAndJoinsOnRelease(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	configureDurableCLISessionTest(t)
	root := t.TempDir()
	t.Chdir(root)
	store := filepath.Join(root, "sessions")
	server, provider := newProcessSessionLongRunProvider(t)
	defer server.Close()
	cfg := durablePromptConfig(t, server.URL, store)
	cfg.Git.WorkDir = root
	cfg.Compaction.Strategy = "truncating"
	cfg.Compaction.MaxTokens = 4096
	cfg.Model.SetMaxTokens(256)
	cfg.Agent.SystemPrompt = "Use process_session to manage a long-running local command. After checking it, run the requested ordinary shell check and report completion."
	cfg.Sandbox.AllowedPaths = []string{root}

	writer := &processSessionLongRunWriter{}
	runCtx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		runDone <- newPromptCmd(writer).Run(runCtx, cfg, []string{"--output=json", "--approve=auto", "--no-sandbox", "Start a background process, inspect its output and status after compaction, run a normal bash check, and finish."})
	}()

	handoffDeadline := time.NewTimer(45 * time.Second)
	defer handoffDeadline.Stop()
	handoffTicker := time.NewTicker(20 * time.Millisecond)
	defer handoffTicker.Stop()
	for !strings.Contains(writer.String(), `"kind":"process_session_handoff"`) {
		select {
		case runErr := <-runDone:
			require.NoError(t, runErr, "prompt failed before delivering the active process-session handoff; output: %s", writer.String())
			t.Fatal("prompt exited without emitting an active process-session handoff")
		case <-handoffDeadline.C:
			t.Fatal("the CLI did not deliver its final and active-session handoff before waiting for release")
		case <-handoffTicker.C:
		}
	}
	lines := strings.Split(strings.TrimSpace(writer.String()), "\n")
	require.NotEmpty(t, lines)
	var handoff struct {
		Kind        string   `json:"kind"`
		Version     int      `json:"version"`
		Status      string   `json:"status"`
		OwnerID     string   `json:"owner_id"`
		RunID       string   `json:"run_id"`
		WorkspaceID string   `json:"workspace_id"`
		SessionIDs  []string `json:"session_ids"`
	}
	var finalIndex, handoffIndex = -1, -1
	for index, line := range lines {
		var event map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		var kind string
		_ = json.Unmarshal(event["kind"], &kind)
		if kind == "process_session_handoff" {
			require.NoError(t, json.Unmarshal([]byte(line), &handoff))
			handoffIndex = index
		}
		var content string
		_ = json.Unmarshal(event["content"], &content)
		if strings.Contains(content, "process session lifecycle complete") {
			finalIndex = index
		}
	}
	require.Equal(t, "process_session_handoff", handoff.Kind)
	require.Equal(t, 1, handoff.Version)
	require.Equal(t, "delivered-active", handoff.Status)
	require.NotEmpty(t, handoff.OwnerID)
	require.NotEmpty(t, handoff.RunID)
	require.NotEmpty(t, handoff.WorkspaceID)
	require.Len(t, handoff.SessionIDs, 1)
	require.GreaterOrEqual(t, handoffIndex, 0)
	require.GreaterOrEqual(t, finalIndex, 0)
	require.Less(t, finalIndex, handoffIndex, "handoff is emitted only after the assistant final")

	requests, providerErrors, sessionID, turns := provider.snapshot()
	require.Empty(t, providerErrors)
	require.Equal(t, 6, turns)
	require.Len(t, requests, 6)
	require.Equal(t, sessionID, handoff.SessionIDs[0])
	require.Contains(t, requests[2], compaction.DurableTruncationMarker, "large read output must cross the configured local compaction budget")
	var compactedRequest struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal([]byte(requests[2]), &compactedRequest))
	var foundRoster bool
	for _, message := range compactedRequest.Messages {
		var content string
		if json.Unmarshal(message.Content, &content) == nil &&
			strings.Contains(content, "Active process sessions (use these exact IDs") && strings.Contains(content, sessionID) {
			foundRoster = true
		}
	}
	require.True(t, foundRoster, "the ID must remain available to the model after real durable compaction")
	for _, body := range requests {
		require.Equal(t, 256, maxTokensFromProviderBody(t, body))
	}

	marker := filepath.Join(root, "session-leader.pid")
	pidBytes, err := os.ReadFile(marker)
	require.NoError(t, err)
	leaderPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	require.NoError(t, err)
	require.Positive(t, leaderPID)
	require.NoError(t, syscall.Kill(leaderPID, 0), "host probe must find the delivered process alive after the assistant final")

	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGUSR1))
	select {
	case err := <-runDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("CLI invocation did not release and join the process session")
	}
	groupErr := syscall.Kill(-leaderPID, 0)
	require.ErrorIs(t, groupErr, syscall.ESRCH, "owned process group must be absent after the invocation exits")

	physicalSessions, err := session.ListSessionCatalog(t.Context(), store)
	require.NoError(t, err)
	require.Len(t, physicalSessions, 1)
	reopened, err := newDurableCLISession(t.Context(), store, "inspect-process-session", physicalSessions[0].SessionID, "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.requestClose(context.Background())) })
	snapshot, err := reopened.snapshotSelected()
	require.NoError(t, err)
	rows, err := session.InspectPendingActions(snapshot)
	require.NoError(t, err)
	require.Empty(t, rows, "the delivered process-session start must be durably closed and resolved")
	var delivered bool
	var stopped bool
	var outputReadAfterCompaction bool
	var statusAfterCompaction bool
	var ordinaryBashAfterCompaction bool
	for _, row := range snapshot.DurableEntries {
		if row.ProcessSessionDelivery != nil && row.ProcessSessionDelivery.RunID == handoff.RunID {
			delivered = true
		}
		if row.ProcessSessionLease != nil && row.ProcessSessionLease.SessionID == sessionID &&
			row.ProcessSessionLease.Phase == "stopped" && row.ProcessSessionLease.GroupAbsent && row.ProcessSessionLease.LeaderWaited && row.ProcessSessionLease.ReadersJoined {
			stopped = true
		}
		if row.Type == session.EntryTypeTool && row.ToolCallID == "process-read-after-compaction" {
			var readResult struct {
				Output string `json:"output"`
			}
			if json.Unmarshal([]byte(row.Content), &readResult) == nil && strings.Count(readResult.Output, "x\n") >= 50 {
				outputReadAfterCompaction = true
			}
		}
		if row.Type == session.EntryTypeTool && row.ToolCallID == "process-status" {
			var statusResult struct {
				Status string `json:"status"`
			}
			if json.Unmarshal([]byte(row.Content), &statusResult) == nil && statusResult.Status == "running" {
				statusAfterCompaction = true
			}
		}
		if row.Type == session.EntryTypeTool && row.ToolCallID == "ordinary-bash" && strings.Contains(row.Content, "ordinary-bash-ok") {
			ordinaryBashAfterCompaction = true
		}
	}
	require.True(t, delivered, "final terminal must bind the accepted live session set")
	require.True(t, stopped, "release must persist positive group and reader closure evidence")
	require.True(t, outputReadAfterCompaction, "the process output must remain readable after compaction")
	require.True(t, statusAfterCompaction, "the model must be able to inspect process status after compaction")
	require.True(t, ordinaryBashAfterCompaction, "the same run must admit ordinary Bash while its process lease is live")
}

func maxTokensFromProviderBody(t *testing.T, body string) int {
	t.Helper()
	var request struct {
		MaxTokens int `json:"max_tokens"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &request))
	return request.MaxTokens
}
