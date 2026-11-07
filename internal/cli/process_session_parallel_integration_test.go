package cli

import (
	"context"
	"encoding/json"
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

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

type processSessionParallelCall struct {
	id   string
	name string
	args map[string]any
}

func processSessionParallelToolResponse(calls ...processSessionParallelCall) string {
	encodedCalls := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		args, err := json.Marshal(call.args)
		if err != nil {
			return `{"choices":[{"message":{"role":"assistant","content":"fixture encoding failed"}}]}`
		}
		encodedCalls = append(encodedCalls, map[string]any{
			"id": call.id, "type": "function",
			"function": map[string]any{"name": call.name, "arguments": string(args)},
		})
	}
	response, err := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"message": map[string]any{"role": "assistant", "tool_calls": encodedCalls},
	}}})
	if err != nil {
		return `{"choices":[{"message":{"role":"assistant","content":"fixture encoding failed"}}]}`
	}
	return string(response)
}

type processSessionParallelProvider struct {
	mu                sync.Mutex
	mode              string
	root              string
	initial           string
	status            string
	childFind         string
	requests          []string
	errors            []string
	parentTurns       int
	childTurns        int
	childReadOK       bool
	childWriteBlocked bool
	childReadPath     string
	childWritePath    string
	priorStopOK       bool
	ordinaryAfterStop bool
	naturalBashOK     bool
}

func newProcessSessionParallelProvider(t *testing.T, root, mode string) (*httptest.Server, *processSessionParallelProvider) {
	t.Helper()
	marker := filepath.Join(root, "parallel-session-leader.pid")
	command := "printf '%s' \"$$\" > " + shellSingleQuote(marker) + "; exec sleep 60"
	if mode == "natural" {
		command = "printf '%s' \"$$\" > " + shellSingleQuote(marker) + "; exit 1"
	}
	if mode == "natural-later-bash" {
		command = "printf '%s' \"$$\" > " + shellSingleQuote(marker) + "; sleep 2; exit 1"
	}
	ordinary := "while [ ! -f " + shellSingleQuote(marker) + " ]; do sleep 0.01; done; printf parallel-bash-after-start"
	initial := processSessionParallelToolResponse(
		processSessionParallelCall{id: "process-start", name: "process_session", args: map[string]any{"operation": "start", "command": command}},
		processSessionParallelCall{id: "ordinary-bash", name: "bash", args: map[string]any{"command": ordinary}},
	)
	if mode == "delegate" {
		initial = processSessionParallelToolResponse(
			processSessionParallelCall{id: "process-start", name: "process_session", args: map[string]any{"operation": "start", "command": command}},
			processSessionParallelCall{id: "delegate-child", name: "dispatch_subagent", args: map[string]any{
				"prompt": "Read the supplied file, then attempt the requested write and report both outcomes.", "role": "implementer", "tools": []string{"read", "write"},
			}},
		)
	}
	if mode == "separate-stop" {
		initial = processSessionParallelToolResponse(
			processSessionParallelCall{id: "process-start", name: "process_session", args: map[string]any{"operation": "start", "command": command}},
		)
	}
	if mode == "natural-later-bash" {
		initial = processSessionParallelToolResponse(
			processSessionParallelCall{id: "process-start", name: "process_session", args: map[string]any{"operation": "start", "command": command}},
		)
	}
	provider := &processSessionParallelProvider{
		mode: mode, root: root, initial: initial,
		childFind:      sequentialCLIResponse(t, "unused-child-find", "find", map[string]any{"path": root, "pattern": "never-match"}),
		childReadPath:  filepath.Join(root, "child-readable.txt"),
		childWritePath: filepath.Join(root, "child-write-must-be-blocked.txt"),
	}
	if err := os.WriteFile(provider.childReadPath, []byte("child readable marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(provider.serveHTTP))
	return server, provider
}

func (provider *processSessionParallelProvider) serveHTTP(w http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(w, "read provider request", http.StatusBadRequest)
		return
	}
	var decoded struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		http.Error(w, "decode provider request", http.StatusBadRequest)
		return
	}
	var userContent []string
	toolCalls := make(map[string]bool)
	toolResults := make(map[string]string)
	for _, message := range decoded.Messages {
		if message.Role == "user" {
			var content string
			if json.Unmarshal(message.Content, &content) == nil {
				userContent = append(userContent, content)
			}
		}
		if message.Role == "tool" {
			toolCalls[message.ToolCallID] = true
			var content string
			if json.Unmarshal(message.Content, &content) == nil {
				toolResults[message.ToolCallID] = content
			} else {
				toolResults[message.ToolCallID] = string(message.Content)
			}
		}
	}
	childRequest := false
	for _, content := range userContent {
		if strings.Contains(content, "Delegation lineage (audit data only") {
			childRequest = true
			break
		}
	}

	provider.mu.Lock()
	provider.requests = append(provider.requests, string(body))
	response := `{"choices":[{"message":{"role":"assistant","content":"parallel process-session fixture complete"}}]}`
	if strings.Contains(string(body), "Analyze the following conversation") {
		response = `{"choices":[{"message":{"role":"assistant","content":"[]"}}]}`
	} else if childRequest {
		switch provider.childTurns {
		case 0:
			response = durableApprovalToolResponseUnsafe("child-read", "read", map[string]any{"path": provider.childReadPath})
		case 1:
			provider.childReadOK = toolCalls["child-read"] && strings.Contains(toolResults["child-read"], "child readable marker")
			if !provider.childReadOK {
				provider.errors = append(provider.errors, "child stock read did not return its marker before the mutation attempt")
			}
			response = durableApprovalToolResponseUnsafe("child-write", "write", map[string]any{
				"path": provider.childWritePath, "content": "child write must stay blocked while the parent lease is live",
			})
		default:
			writeOutput := strings.ToLower(toolResults["child-write"])
			provider.childWriteBlocked = toolCalls["child-write"] &&
				(strings.Contains(writeOutput, "quarant") || strings.Contains(writeOutput, "denied") || strings.Contains(writeOutput, "blocked"))
			if !provider.childWriteBlocked {
				provider.errors = append(provider.errors, "child write was not positively rejected while the parent lease remained live")
			}
			response = `{"choices":[{"message":{"role":"assistant","content":"child task complete: read succeeded; write was blocked"}}]}`
		}
		provider.childTurns++
	} else {
		switch provider.parentTurns {
		case 0:
			response = provider.initial
		case 1:
			if provider.mode == "separate-stop" {
				sessionID := processSessionResultIDFromRawMessages(decoded.Messages)
				if sessionID == "" {
					provider.errors = append(provider.errors, "separate stop request omitted the process-start session ID")
					break
				}
				response = processSessionParallelToolResponse(
					processSessionParallelCall{id: "process-stop", name: "process_session", args: map[string]any{"operation": "stop", "session_id": sessionID}},
					processSessionParallelCall{id: "ordinary-after-stop", name: "bash", args: map[string]any{"command": "printf parallel-bash-after-stop"}},
				)
			} else if provider.mode == "natural-later-bash" {
				response = processSessionParallelToolResponse(
					processSessionParallelCall{id: "ordinary-after-natural-exit", name: "bash", args: map[string]any{"command": "sleep 3; printf later-bash-after-natural-exit"}},
				)
			} else if provider.mode == "bash" || provider.mode == "natural" {
				sessionID := processSessionResultIDFromRawMessages(decoded.Messages)
				if sessionID == "" {
					provider.errors = append(provider.errors, "parent provider turn omitted the process-start session ID")
					break
				}
				response = durableApprovalToolResponseUnsafe("session-status", "process_session", map[string]any{"operation": "status", "session_id": sessionID})
			} else {
				childOutput := strings.ToLower(toolResults["delegate-child"])
				provider.childWriteBlocked = toolCalls["delegate-child"] &&
					(strings.Contains(childOutput, "quarant") || strings.Contains(childOutput, "denied") || strings.Contains(childOutput, "blocked"))
				if !provider.childWriteBlocked {
					provider.errors = append(provider.errors, "parent did not receive a positive child mutation denial")
				}
				response = `{"choices":[{"message":{"role":"assistant","content":"Child read succeeded and the attempted write was blocked"}}]}`
			}
		case 2:
			if provider.mode == "separate-stop" {
				stopOutput := strings.ToLower(toolResults["process-stop"])
				provider.priorStopOK = toolCalls["process-stop"] && strings.Contains(stopOutput, "stopped")
				provider.ordinaryAfterStop = toolCalls["ordinary-after-stop"] && strings.Contains(toolResults["ordinary-after-stop"], "parallel-bash-after-stop")
				if !provider.priorStopOK || !provider.ordinaryAfterStop {
					provider.errors = append(provider.errors, "separate stop and ordinary Bash results were not both delivered")
				}
			} else if provider.mode == "natural-later-bash" {
				provider.naturalBashOK = toolCalls["ordinary-after-natural-exit"] &&
					strings.Contains(toolResults["ordinary-after-natural-exit"], "later-bash-after-natural-exit")
				if !provider.naturalBashOK {
					provider.errors = append(provider.errors, "ordinary Bash result was not delivered after natural process exit")
				}
			}
		}
		provider.parentTurns++
	}
	provider.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, response)
}

func durableApprovalToolResponseUnsafe(id, name string, args map[string]any) string {
	encoded, err := json.Marshal(args)
	if err != nil {
		return `{"choices":[{"message":{"role":"assistant","content":"fixture encoding failed"}}]}`
	}
	response, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
		"role": "assistant", "tool_calls": []any{map[string]any{
			"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(encoded)},
		}},
	}}}})
	if err != nil {
		return `{"choices":[{"message":{"role":"assistant","content":"fixture encoding failed"}}]}`
	}
	return string(response)
}

func processSessionResultIDFromRawMessages(messages []struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
}) string {
	for _, message := range messages {
		if message.Role != "tool" || message.ToolCallID != "process-start" {
			continue
		}
		var output string
		if json.Unmarshal(message.Content, &output) != nil {
			return ""
		}
		var result struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(output), &result) != nil {
			return ""
		}
		return result.SessionID
	}
	return ""
}

func (provider *processSessionParallelProvider) snapshot() ([]string, []string, int) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]string(nil), provider.requests...), append([]string(nil), provider.errors...), provider.parentTurns
}

func (provider *processSessionParallelProvider) childOutcomes() (bool, bool) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.childReadOK, provider.childWriteBlocked
}

func TestParallelProcessSessionStartIsTypedAndDeliveredBeforeOrdinaryTool(t *testing.T) {
	assertParallelProcessSessionLifecycle(t, "bash")
}

func TestParallelProcessSessionStartIsTypedAndDeliveredBeforeManagedDelegation(t *testing.T) {
	assertParallelProcessSessionLifecycle(t, "delegate")
}

func TestParallelNaturalProcessSessionExitClosesWithoutHandoff(t *testing.T) {
	assertParallelProcessSessionLifecycle(t, "natural")
}

func TestProcessSessionStopAndOrdinaryToolAfterStartInSeparateResponses(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	configureDurableCLISessionTest(t)
	root := t.TempDir()
	t.Chdir(root)
	store := filepath.Join(root, "sessions")
	server, provider := newProcessSessionParallelProvider(t, root, "separate-stop")
	defer server.Close()
	cfg := durablePromptConfig(t, server.URL, store)
	cfg.Git.WorkDir = root
	cfg.Agent.SystemPrompt = "Start the requested process session, then stop it and complete the ordinary shell check."
	cfg.Sandbox.AllowedPaths = []string{root}
	writer := &processSessionLongRunWriter{}
	runCtx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		runDone <- newPromptCmd(writer).Run(runCtx, cfg, []string{"--output=json", "--approve=auto", "--no-sandbox", "Start the process, then stop it and run the shell check."})
	}()
	select {
	case runErr := <-runDone:
		require.NoError(t, runErr, "process stop in the next assistant response must resolve the prior start action")
	case <-time.After(20 * time.Second):
		t.Fatal("separate-response process stop did not finish")
	}
	require.NotContains(t, writer.String(), `"kind":"process_session_handoff"`)
	requests, providerErrors, parentTurns := provider.snapshot()
	require.Empty(t, providerErrors)
	require.Equal(t, 3, parentTurns)
	require.GreaterOrEqual(t, len(requests), 3)
	require.Contains(t, writer.String(), "parallel-bash-after-stop")
	require.True(t, provider.priorStopOK, "the explicit stop must succeed in the later response")
	require.True(t, provider.ordinaryAfterStop, "the ordinary Bash sibling must run after stop")

	catalog, err := session.ListSessionCatalog(t.Context(), store)
	require.NoError(t, err)
	require.Len(t, catalog, 1)
	rows := readParallelProcessSessionJournal(t, store, catalog[0].SessionID)
	var startResolutionSeq, stopReceiptSeq, ordinaryReceiptSeq uint64
	var startActionID string
	for _, row := range rows {
		if row.Type == session.EntryTypeTool && row.ActionState == session.ActionStateIntent && row.ToolCallID == "process-start" {
			startActionID = row.ActionID
		}
		if row.CheckpointKind == session.CheckpointExecutionResolution && row.ExecutionResolution != nil &&
			row.ExecutionResolution.ActionID == startActionID && startActionID != "" {
			startResolutionSeq = row.Seq
		}
		if row.Type == session.EntryTypeTool && row.ActionState == "" {
			switch row.ToolCallID {
			case "process-stop":
				stopReceiptSeq = row.Seq
				require.Equal(t, core.ReceiptOutcomeSuccess, row.ReceiptOutcome)
			case "ordinary-after-stop":
				ordinaryReceiptSeq = row.Seq
				require.Equal(t, core.ReceiptOutcomeSuccess, row.ReceiptOutcome)
			}
		}
	}
	require.Positive(t, startResolutionSeq, "the prior start must receive its exact durable closure resolution")
	require.Positive(t, stopReceiptSeq)
	require.Positive(t, ordinaryReceiptSeq)
	require.Less(t, startResolutionSeq, ordinaryReceiptSeq, "the old closure may cross the open response group, but ordinary work waits for the stop segment")
	marker := filepath.Join(root, "parallel-session-leader.pid")
	pidBytes, err := os.ReadFile(marker)
	require.NoError(t, err)
	leaderPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	require.NoError(t, err)
	require.ErrorIs(t, syscall.Kill(-leaderPID, 0), syscall.ESRCH)
	reopened, err := newDurableCLISession(t.Context(), store, "inspect-separate-stop", catalog[0].SessionID, "")
	require.NoError(t, err)
	defer func() { require.NoError(t, reopened.requestClose(context.Background())) }()
	snapshot, err := reopened.snapshotSelected()
	require.NoError(t, err)
	pending, err := session.InspectPendingActions(snapshot)
	require.NoError(t, err)
	require.Empty(t, pending)
}

func TestNaturalProcessSessionExitDuringLaterBashResolvesPriorRun(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	configureDurableCLISessionTest(t)
	root := t.TempDir()
	t.Chdir(root)
	store := filepath.Join(root, "sessions")
	server, provider := newProcessSessionParallelProvider(t, root, "natural-later-bash")
	defer server.Close()
	cfg := durablePromptConfig(t, server.URL, store)
	cfg.Git.WorkDir = root
	cfg.Agent.SystemPrompt = "Start the requested local process, then run the ordinary Bash check."
	cfg.Sandbox.AllowedPaths = []string{root}
	writer := &processSessionLongRunWriter{}
	runCtx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		runDone <- newPromptCmd(writer).Run(runCtx, cfg, []string{"--output=json", "--approve=auto", "--no-sandbox", "Start the process, then run the shell check."})
	}()
	select {
	case runErr := <-runDone:
		require.NoError(t, runErr, "natural process exit during later Bash must resolve the prior start action")
	case <-time.After(15 * time.Second):
		t.Fatal("later Bash did not finish after the process exited naturally")
	}
	require.NotContains(t, writer.String(), `"kind":"process_session_handoff"`)
	requests, providerErrors, parentTurns := provider.snapshot()
	require.Empty(t, providerErrors)
	require.Equal(t, 3, parentTurns)
	require.GreaterOrEqual(t, len(requests), 3)
	require.True(t, provider.naturalBashOK)
	require.Contains(t, writer.String(), "later-bash-after-natural-exit")

	catalog, err := session.ListSessionCatalog(t.Context(), store)
	require.NoError(t, err)
	require.Len(t, catalog, 1)
	rows := readParallelProcessSessionJournal(t, store, catalog[0].SessionID)
	var startActionID string
	var startReceiptSeq, stoppedSeq, startResolutionSeq, bashReceiptSeq uint64
	for _, row := range rows {
		if row.Type == session.EntryTypeTool && row.ActionState == session.ActionStateIntent && row.ToolCallID == "process-start" {
			startActionID = row.ActionID
		}
		if row.Type == session.EntryTypeTool && row.ToolCallID == "process-start" && row.ActionState == "" {
			startReceiptSeq = row.Seq
		}
		if row.ProcessSessionLease != nil && row.ProcessSessionLease.Phase == tools.ProcessSessionLeaseStopped {
			stoppedSeq = row.Seq
		}
		if row.CheckpointKind == session.CheckpointExecutionResolution && row.ExecutionResolution != nil && row.ExecutionResolution.ActionID == startActionID && startActionID != "" {
			startResolutionSeq = row.Seq
		}
		if row.Type == session.EntryTypeTool && row.ToolCallID == "ordinary-after-natural-exit" && row.ActionState == "" {
			bashReceiptSeq = row.Seq
			require.Equal(t, core.ReceiptOutcomeSuccess, row.ReceiptOutcome)
		}
	}
	require.Positive(t, startReceiptSeq)
	require.Greater(t, stoppedSeq, startReceiptSeq)
	require.Greater(t, startResolutionSeq, startReceiptSeq)
	require.Greater(t, bashReceiptSeq, startResolutionSeq, "the prior process closure must be synced before the later Bash group closes")
	marker := filepath.Join(root, "parallel-session-leader.pid")
	pidBytes, err := os.ReadFile(marker)
	require.NoError(t, err)
	leaderPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	require.NoError(t, err)
	require.ErrorIs(t, syscall.Kill(-leaderPID, 0), syscall.ESRCH)
	reopened, err := newDurableCLISession(t.Context(), store, "inspect-natural-later-bash", catalog[0].SessionID, "")
	require.NoError(t, err)
	defer func() { require.NoError(t, reopened.requestClose(context.Background())) }()
	snapshot, err := reopened.snapshotSelected()
	require.NoError(t, err)
	pending, err := session.InspectPendingActions(snapshot)
	require.NoError(t, err)
	require.Empty(t, pending)
}

func assertParallelProcessSessionLifecycle(t *testing.T, mode string) {
	t.Helper()
	defer verify.AssertNoGoroutineLeak(t)()
	configureDurableCLISessionTest(t)
	root := t.TempDir()
	t.Chdir(root)
	store := filepath.Join(root, "sessions")
	server, provider := newProcessSessionParallelProvider(t, root, mode)
	defer server.Close()
	cfg := durablePromptConfig(t, server.URL, store)
	cfg.Git.WorkDir = root
	cfg.Agent.SystemPrompt = "Manage the requested background server with process_session, then finish the other requested operation."
	cfg.Sandbox.AllowedPaths = []string{root}
	writer := &processSessionLongRunWriter{}
	runCtx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	runDone := make(chan error, 1)
	go func() {
		runDone <- newPromptCmd(writer).Run(runCtx, cfg, []string{"--output=json", "--approve=auto", "--no-sandbox", "Start a local background server and complete the requested parallel operation."})
	}()
	runFinished := false
	defer func() {
		cancel()
		if !runFinished {
			select {
			case <-runDone:
			case <-time.After(10 * time.Second):
				t.Error("parallel process-session CLI did not join after cancellation")
			}
		}
	}()
	if mode == "natural" {
		select {
		case runErr := <-runDone:
			runFinished = true
			require.NoError(t, runErr)
		case <-time.After(10 * time.Second):
			t.Fatal("naturally exited process-session did not let the CLI finish")
		}
		require.NotContains(t, writer.String(), `"kind":"process_session_handoff"`, "a dead process must not be delivered as active")
		requests, providerErrors, parentTurns := provider.snapshot()
		require.Empty(t, providerErrors)
		require.Equal(t, 3, parentTurns)
		require.GreaterOrEqual(t, len(requests), 3)
		require.Contains(t, writer.String(), "parallel-bash-after-start")
		catalog, err := session.ListSessionCatalog(t.Context(), store)
		require.NoError(t, err)
		require.Len(t, catalog, 1)
		rows := readParallelProcessSessionJournal(t, store, catalog[0].SessionID)
		var gatedSeq, stoppedSeq, receiptSeq, actionSeq, ordinarySeq, resolutionSeq, latestLeaseSeq uint64
		var latestLeasePhase tools.ProcessSessionLeasePhase
		var stoppedLeaseID, closureRef, startActionID string
		for _, row := range rows {
			if row.ProcessSessionLease != nil {
				if row.Seq > latestLeaseSeq {
					latestLeaseSeq = row.Seq
					latestLeasePhase = row.ProcessSessionLease.Phase
				}
				switch row.ProcessSessionLease.Phase {
				case tools.ProcessSessionLeaseGated:
					gatedSeq = row.Seq
				case tools.ProcessSessionLeaseStopped:
					stoppedSeq = row.Seq
					stoppedLeaseID = row.ID
				}
			}
			if row.Type == session.EntryTypeTool && row.ActionState == session.ActionStateIntent && row.ToolCallID == "process-start" {
				startActionID = row.ActionID
			}
			if row.CheckpointKind == session.CheckpointExecutionResolution && row.ExecutionResolution != nil && row.ExecutionResolution.ActionID == startActionID {
				resolutionSeq = row.Seq
				closureRef = row.ExecutionResolution.ProcessSessionClosureRef
			}
			if row.Type == session.EntryTypeTool && row.ToolCallID == "process-start" && row.ActionState == "" {
				receiptSeq = row.Seq
			}
			if row.CheckpointKind == session.CheckpointProcessSessionAction && row.ProcessSessionAction != nil && row.ProcessSessionAction.Operation == "start" {
				actionSeq = row.Seq
			}
			if row.Type == session.EntryTypeTool && row.ToolCallID == "ordinary-bash" && row.ActionState == "" {
				ordinarySeq = row.Seq
				require.Equal(t, core.ReceiptOutcomeSuccess, row.ReceiptOutcome)
			}
		}
		require.Positive(t, gatedSeq)
		require.Positive(t, stoppedSeq, "the collector's positive close must be durable")
		require.Greater(t, stoppedSeq, gatedSeq)
		require.Greater(t, actionSeq, receiptSeq, "the start action checkpoint must follow its successful receipt")
		require.Greater(t, resolutionSeq, receiptSeq, "natural closure resolution must follow the real start receipt")
		require.Greater(t, resolutionSeq, actionSeq, "natural closure resolution must follow the exact start action record")
		require.Equal(t, stoppedLeaseID, closureRef, "resolution must cite the exact positive Stopped lease")
		require.Equal(t, tools.ProcessSessionLeaseStopped, latestLeasePhase, "no Active lease may be published after Stopped")
		require.Greater(t, ordinarySeq, resolutionSeq, "the next parallel phase must wait for durable closure resolution")
		marker := filepath.Join(root, "parallel-session-leader.pid")
		pidBytes, err := os.ReadFile(marker)
		require.NoError(t, err)
		leaderPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
		require.NoError(t, err)
		require.ErrorIs(t, syscall.Kill(-leaderPID, 0), syscall.ESRCH)
		reopened, err := newDurableCLISession(t.Context(), store, "inspect-natural-exit", catalog[0].SessionID, "")
		require.NoError(t, err)
		defer func() { require.NoError(t, reopened.requestClose(context.Background())) }()
		snapshot, err := reopened.snapshotSelected()
		require.NoError(t, err)
		pending, err := session.InspectPendingActions(snapshot)
		require.NoError(t, err)
		require.Empty(t, pending)
		return
	}
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for !strings.Contains(writer.String(), `"kind":"process_session_handoff"`) {
		select {
		case runErr := <-runDone:
			runFinished = true
			require.NoError(t, runErr, "CLI ended before a typed process-session handoff: %s", writer.String())
			t.Fatal("CLI exited without an active process-session handoff")
		case <-deadline.C:
			t.Fatal("parallel process-session tool batch did not finish and hand off")
		case <-ticker.C:
		}
	}
	var handoff tools.ProcessSessionHandoff
	for _, line := range strings.Split(strings.TrimSpace(writer.String()), "\n") {
		if strings.Contains(line, `"kind":"process_session_handoff"`) {
			require.NoError(t, json.Unmarshal([]byte(line), &handoff))
			break
		}
	}
	require.Equal(t, "delivered-active", handoff.Status)
	require.NotEmpty(t, handoff.SessionIDs)
	requests, providerErrors, parentTurns := provider.snapshot()
	require.Empty(t, providerErrors)
	if mode == "bash" {
		require.Equal(t, 3, parentTurns)
		require.GreaterOrEqual(t, len(requests), 3)
		require.Contains(t, writer.String(), "parallel-bash-after-start")
	} else {
		require.Equal(t, 2, parentTurns)
		require.GreaterOrEqual(t, len(requests), 4, "the child must read and attempt a write before the parent final")
		require.Contains(t, writer.String(), "Child read succeeded and the attempted write was blocked")
		readOK, writeBlocked := provider.childOutcomes()
		require.True(t, readOK, "child must complete its stock scoped read while the parent's process session remains live")
		require.True(t, writeBlocked, "child mutation must remain blocked without borrowing the parent's process-session capability")
		_, writeErr := os.Stat(provider.childWritePath)
		require.ErrorIs(t, writeErr, os.ErrNotExist, "blocked child mutation must not create a file")
	}

	marker := filepath.Join(root, "parallel-session-leader.pid")
	pidBytes, err := os.ReadFile(marker)
	require.NoError(t, err)
	leaderPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	require.NoError(t, err)
	require.Positive(t, leaderPID)
	require.NoError(t, syscall.Kill(leaderPID, 0), "host must retain the process after the final answer")

	catalog, err := session.ListSessionCatalog(t.Context(), store)
	require.NoError(t, err)
	physicalID := ""
	for _, candidate := range catalog {
		contents, readErr := os.ReadFile(filepath.Join(store, "sessions", candidate.SessionID+".jsonl"))
		if readErr != nil {
			continue
		}
		for _, line := range strings.Split(string(contents), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var row session.SessionEntry
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				t.Fatalf("decode session journal row: %v", err)
			}
			if row.ProcessSessionLease != nil && row.ProcessSessionLease.RunID == handoff.RunID {
				physicalID = candidate.SessionID
				break
			}
		}
		if physicalID != "" {
			break
		}
	}
	require.NotEmpty(t, physicalID, "the captured handoff run must identify the parent physical session")
	rows := readParallelProcessSessionJournal(t, store, physicalID)
	var gatedSeq, activeSeq, startReceiptSeq, completedOtherSeq uint64
	for _, row := range rows {
		if row.ProcessSessionLease != nil && row.ProcessSessionLease.SessionID == handoff.SessionIDs[0] {
			switch row.ProcessSessionLease.Phase {
			case "gated":
				if gatedSeq == 0 {
					gatedSeq = row.Seq
				}
			case "active":
				activeSeq = row.Seq
			}
		}
		if row.Type == session.EntryTypeTool && row.ToolCallID == "process-start" && row.ActionState == "" {
			startReceiptSeq = row.Seq
		}
		if row.Type == session.EntryTypeTool && row.ActionState == "" && ((mode == "bash" && row.ToolCallID == "ordinary-bash") || (mode == "delegate" && row.ToolCallID == "delegate-child")) {
			completedOtherSeq = row.Seq
			if mode == "bash" {
				require.Equal(t, "success", row.ReceiptOutcome, "later parallel Bash segment must return its actual result")
			} else {
				require.Equal(t, "error", row.ReceiptOutcome, "the delegated child must report its blocked write as an error")
				require.Contains(t, strings.ToLower(row.Content), "quarant", "the parent must receive the child's durable mutation denial")
			}
		}
	}
	require.Positive(t, gatedSeq, "process code must be preceded by a durable gated identity")
	require.Greater(t, startReceiptSeq, gatedSeq)
	require.Greater(t, activeSeq, startReceiptSeq, "the active lease is durable only after the start receipt")
	require.Greater(t, completedOtherSeq, activeSeq, "the next tool segment begins after the process-session lease is accepted")

	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGUSR1))
	select {
	case runErr := <-runDone:
		runFinished = true
		require.NoError(t, runErr)
	case <-time.After(10 * time.Second):
		t.Fatal("CLI did not release and join the parallel process session")
	}
	require.ErrorIs(t, syscall.Kill(-leaderPID, 0), syscall.ESRCH)
	closedCatalog, err := session.ListSessionCatalog(t.Context(), store)
	require.NoError(t, err)
	expectedSessions := 1
	if mode == "delegate" {
		expectedSessions++
	}
	require.Len(t, catalog, expectedSessions)
	require.Len(t, closedCatalog, expectedSessions)
	reopened, err := newDurableCLISession(t.Context(), store, "inspect-parallel-process", physicalID, "")
	require.NoError(t, err)
	defer func() { require.NoError(t, reopened.requestClose(context.Background())) }()
	closedSnapshot, err := reopened.snapshotSelected()
	require.NoError(t, err)
	pending, err := session.InspectPendingActions(closedSnapshot)
	require.NoError(t, err)
	require.Empty(t, pending, "host release writes a positive closure before the session becomes idle")
}

func readParallelProcessSessionJournal(t *testing.T, store, physicalID string) []session.SessionEntry {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(store, "sessions", physicalID+".jsonl"))
	require.NoError(t, err)
	var rows []session.SessionEntry
	for _, line := range strings.Split(string(contents), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row session.SessionEntry
		require.NoError(t, json.Unmarshal([]byte(line), &row))
		rows = append(rows, row)
	}
	return rows
}
