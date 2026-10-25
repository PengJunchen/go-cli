package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestAssembledLoopDetectorStopsDurableRunAtFrozenAttemptThreshold(t *testing.T) {
	configureDurableCLISessionTest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	workspaceRoot, err := os.Getwd()
	require.NoError(t, err)
	writeDir, err := os.MkdirTemp(workspaceRoot, ".loop-detector-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(writeDir) })
	writePath := filepath.Join(writeDir, "result.txt")
	writeArgs, err := json.Marshal(map[string]string{"path": writePath, "content": "once"})
	require.NoError(t, err)

	var providerRequests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestNumber := providerRequests.Add(1)
		callID := "display-call-one"
		if requestNumber == 2 {
			callID = "display-call-two"
		}
		response, marshalErr := json.Marshal(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []any{map[string]any{
						"id":   callID,
						"type": "function",
						"function": map[string]any{
							"name":      "write",
							"arguments": string(writeArgs),
						},
					}},
				},
			}},
		})
		if marshalErr != nil {
			http.Error(w, "failed to marshal fixture response", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(response)
	}))
	defer provider.Close()

	storeDirectory := filepath.Join(t.TempDir(), "sessions")
	durableSession, err := newDurableCLISession(ctx, storeDirectory, "loop-detector", "", "")
	require.NoError(t, err)
	cfg := newE2ETestConfig(t, provider.URL)
	cfg.Git.WorkDir = workspaceRoot
	cfg.Sandbox.AllowedPaths = []string{workspaceRoot}
	cfg.Production.LoopDetector.SameToolCallThreshold = 2
	cfg.Production.LoopDetector.Disposition = "terminate"
	auditEnabled := true
	cfg.Production.Audit.Enabled = &auditEnabled
	cfg.Production.Audit.Path = filepath.Join(t.TempDir(), "audit.jsonl")
	assembly, err := AssembleAgent(
		ctx,
		cfg,
		cfg.Provider.Name,
		cfg.Provider.Model,
		io.Discard,
		WithApproveMode(ApproveAuto),
		withDurableCLISession(durableSession),
	)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	t.Cleanup(func() { _ = durableSession.requestClose(context.WithoutCancel(ctx)) })

	runCtx := bindActiveLoopDurableRun(ctx, t, assembly, durableSession)
	stream, err := assembly.Harness.Submit(runCtx, "write the same marker twice")
	require.NoError(t, err)
	for range stream.Events() {
	}
	_, runErr := stream.Result()
	require.ErrorIs(t, runErr, core.ErrLoopDetected)
	require.NotErrorIs(t, runErr, context.Canceled, "detector-first termination must remain a typed error")
	require.Equal(t, int32(2), providerRequests.Load(), "the detector stops before a third provider request")

	statusSource, ok := stream.(core.RunStatusSource)
	require.True(t, ok)
	status := statusSource.RunStatusHandle().Snapshot()
	require.Equal(t, core.RunOutcomeError, status.Outcome, "live run status follows the detector-first error cause")

	content, err := os.ReadFile(writePath)
	require.NoError(t, err)
	require.Equal(t, "once", string(content), "the threshold-triggering write was not dispatched")
	audit, err := os.ReadFile(cfg.Production.Audit.Path)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(audit), "\n"), "only the first write reached the execution wrapper")

	snapshot, err := durableSession.snapshotSelected()
	require.NoError(t, err)
	receipts := make(map[string][]session.SessionEntry)
	intents := make(map[string]int)
	var terminal session.SessionEntry
	for _, entry := range snapshot.DurableEntries {
		if entry.Type != session.EntryTypeTool {
			if entry.CheckpointKind == session.CheckpointRunTerminal {
				terminal = entry
			}
			continue
		}
		if entry.ReceiptOutcome != "" {
			receipts[entry.ToolCallID] = append(receipts[entry.ToolCallID], entry)
		}
		if entry.ActionState == session.ActionStateIntent {
			intents[entry.ToolCallID]++
		}
	}
	require.Len(t, receipts, 2, "each assistant tool call gets one known canonical receipt")
	require.Len(t, receipts["display-call-one"], 1)
	require.Len(t, receipts["display-call-two"], 1)
	require.Equal(t, string(core.ReceiptOutcomeSuccess), receipts["display-call-one"][0].ReceiptOutcome)
	require.Equal(t, string(core.ReceiptOutcomeInterrupted), receipts["display-call-two"][0].ReceiptOutcome)
	require.Equal(t, session.DispatchStateNotDispatched, receipts["display-call-two"][0].DispatchState)
	require.Equal(t, 1, intents["display-call-one"])
	require.Zero(t, intents["display-call-two"], "the stopped attempt has no action intent")
	require.Equal(t, core.RunOutcomeError, terminal.RunOutcome)
}

func TestAssembledLoopDetectorStopsParallelDurableBatchBeforeDispatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	toolArgs, err := json.Marshal(map[string]string{"value": "same"})
	require.NoError(t, err)
	executorProbe := &activeLoopExecutorProbe{entered: make(chan string, 3)}

	var providerRequests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		response, marshalErr := json.Marshal(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []any{
						activeLoopToolCallObject("parallel-call-one", "active_loop_probe", toolArgs),
						activeLoopToolCallObject("parallel-call-two", "active_loop_probe", toolArgs),
						activeLoopToolCallObject("parallel-call-three", "active_loop_probe", toolArgs),
					},
				},
			}},
		})
		if marshalErr != nil {
			http.Error(w, "failed to marshal fixture response", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(response)
	}))
	defer provider.Close()

	assembly, durableSession, _ := newActiveLoopDurableAssembly(ctx, t, provider.URL, "terminate", 2)
	// The probe has a synchronous lifetime. Declare that host contract so this
	// fixture reaches the loop detector rather than unsupported-tool preflight.
	registrar, ok := assembly.ToolRegistry.(tools.TrustedInlineControlRegistrar)
	require.True(t, ok)
	require.NoError(t, registrar.RegisterTrustedInlineControl(ctx, executorProbe))
	runCtx := bindActiveLoopDurableRun(ctx, t, assembly, durableSession)
	stream, err := assembly.Harness.Submit(runCtx, "repeat the write in parallel")
	require.NoError(t, err)
	for range stream.Events() {
	}
	_, runErr := stream.Result()
	require.ErrorIs(t, runErr, core.ErrLoopDetected)
	require.Equal(t, int32(1), providerRequests.Load())
	require.Empty(t, executorProbe.entered, "no registered executor entry occurred after the batch hit the preparation threshold")

	statusSource, ok := stream.(core.RunStatusSource)
	require.True(t, ok)
	status := statusSource.RunStatusHandle().Snapshot()
	snapshot, err := durableSession.snapshotSelected()
	require.NoError(t, err)
	receipts := make(map[string][]session.SessionEntry)
	intents := make(map[string]int)
	var terminal *session.SessionEntry
	for index := range snapshot.DurableEntries {
		entry := &snapshot.DurableEntries[index]
		if entry.Type == session.EntryTypeTool {
			if entry.ReceiptOutcome != "" {
				receipts[entry.ToolCallID] = append(receipts[entry.ToolCallID], *entry)
			}
			if entry.ActionState == session.ActionStateIntent {
				intents[entry.ToolCallID]++
			}
		}
		if entry.CheckpointKind == session.CheckpointRunTerminal {
			terminal = entry
		}
	}
	require.Len(t, receipts, 3)
	for _, callID := range []string{"parallel-call-one", "parallel-call-two", "parallel-call-three"} {
		require.Len(t, receipts[callID], 1, "parallel assistant tool calls each receive one canonical receipt")
	}
	require.Equal(t, 1, intents["parallel-call-one"], "the earlier prepared action has its durable intent before cancellation")
	require.Zero(t, intents["parallel-call-two"], "the threshold-triggering action is not dispatched")
	require.Zero(t, intents["parallel-call-three"], "later calls stop preparing after the detector cause")
	for _, callID := range []string{"parallel-call-two", "parallel-call-three"} {
		require.Equal(t, string(core.ReceiptOutcomeInterrupted), receipts[callID][0].ReceiptOutcome, callID)
		require.Equal(t, session.DispatchStateNotDispatched, receipts[callID][0].DispatchState, callID)
	}
	firstOutcome := receipts["parallel-call-one"][0].ReceiptOutcome
	require.Contains(t, []string{string(core.ReceiptOutcomeInterrupted), string(core.ReceiptOutcomeUnknown)}, firstOutcome,
		"the earlier reservation is interrupted if joined before the cancellation snapshot, otherwise retains a conservative unknown reservation state")
	if firstOutcome == string(core.ReceiptOutcomeInterrupted) {
		require.Equal(t, session.DispatchStateNotDispatched, receipts["parallel-call-one"][0].DispatchState)
	} else {
		require.Equal(t, session.DispatchStateDispatched, receipts["parallel-call-one"][0].DispatchState,
			"unknown earlier attempt must retain its dispatched reservation state")
	}
	require.NotNil(t, terminal)
	if firstOutcome == string(core.ReceiptOutcomeUnknown) {
		require.Equal(t, core.RunOutcomeUnknown, status.Outcome, "an unknown earlier reservation outranks the detector error")
		require.Equal(t, core.RunOutcomeUnknown, terminal.RunOutcome)
	} else {
		require.Equal(t, core.RunOutcomeError, status.Outcome)
		require.Equal(t, core.RunOutcomeError, terminal.RunOutcome)
	}
}

func TestAssembledLoopDetectorCallerCancellationWinsOnCanonicalDurablePath(t *testing.T) {
	ctx, timeoutCancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer timeoutCancel()
	callerCtx, callerCancel := context.WithCancel(ctx)
	defer callerCancel()
	workspaceRoot, err := os.Getwd()
	require.NoError(t, err)
	writeDir, err := os.MkdirTemp(workspaceRoot, ".loop-detector-caller-first-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(writeDir) })
	writePath := filepath.Join(writeDir, "result.txt")
	writeArgs, err := json.Marshal(map[string]string{"path": writePath, "content": "once"})
	require.NoError(t, err)

	var providerRequests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(activeLoopToolCallResponse("caller-first-call", writeArgs))
	}))
	defer provider.Close()

	assembly, durableSession, _ := newActiveLoopDurableAssembly(callerCtx, t, provider.URL, "terminate", 1)
	assembly.LoopAgent.SetToolInterceptor(func(toolName, toolCallID string, _ map[string]any) error {
		if toolName == "write" && toolCallID == "caller-first-call" {
			// This runs synchronously inside the canonical pre-tool boundary, before
			// the frozen-attempt observer can apply its threshold-one termination.
			callerCancel()
		}
		return nil
	})
	runCtx := bindActiveLoopDurableRun(callerCtx, t, assembly, durableSession)
	stream, err := assembly.Harness.Submit(runCtx, "write the marker")
	require.NoError(t, err)
	for range stream.Events() {
	}
	_, runErr := stream.Result()
	require.ErrorIs(t, runErr, context.Canceled)
	require.NotErrorIs(t, runErr, core.ErrLoopDetected, "caller cancellation happened before the would-be threshold trigger")
	require.Equal(t, int32(1), providerRequests.Load())

	statusSource, ok := stream.(core.RunStatusSource)
	require.True(t, ok)
	require.Equal(t, core.RunOutcomeCancelled, statusSource.RunStatusHandle().Snapshot().Outcome)
	_, err = os.Stat(writePath)
	require.ErrorIs(t, err, os.ErrNotExist, "caller-first cancellation must not dispatch the trigger tool")

	snapshot, err := durableSession.snapshotSelected()
	require.NoError(t, err)
	var receipt *session.SessionEntry
	var terminal *session.SessionEntry
	for index := range snapshot.DurableEntries {
		entry := &snapshot.DurableEntries[index]
		if entry.Type == session.EntryTypeTool && entry.ReceiptOutcome != "" {
			require.Nil(t, receipt, "one assistant call has exactly one canonical receipt")
			receipt = entry
		}
		if entry.CheckpointKind == session.CheckpointRunTerminal {
			terminal = entry
		}
	}
	require.NotNil(t, receipt)
	require.Equal(t, "caller-first-call", receipt.ToolCallID)
	require.Equal(t, string(core.ReceiptOutcomeInterrupted), receipt.ReceiptOutcome)
	require.Equal(t, session.DispatchStateNotDispatched, receipt.DispatchState)
	require.NotNil(t, terminal)
	require.Equal(t, core.RunOutcomeCancelled, terminal.RunOutcome)
}

func TestAssembledLoopDetectorWarnsContinuesAndInjectsNextTurnReminder(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	workspaceRoot, err := os.Getwd()
	require.NoError(t, err)
	writeDir, err := os.MkdirTemp(workspaceRoot, ".loop-detector-warn-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(writeDir) })
	writePath := filepath.Join(writeDir, "result.txt")
	readArgs, err := json.Marshal(map[string]string{"path": writePath})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(writePath, []byte("once"), 0o600))
	listArgs, err := json.Marshal(map[string]string{"path": workspaceRoot})
	require.NoError(t, err)

	var providerRequests atomic.Int32
	reminderPayload := make(chan string, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestNumber := providerRequests.Add(1)
		var requestBody struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.NewDecoder(request.Body).Decode(&requestBody)
		w.Header().Set("Content-Type", "application/json")
		if requestNumber <= 2 {
			_, _ = w.Write(activeLoopToolCallResponseNamed(
				"warn-call-"+string(rune('0'+requestNumber)), "read", readArgs, string(rune('a'+requestNumber)),
			))
			return
		}
		if requestNumber == 3 {
			_, _ = w.Write(activeLoopToolCallResponseNamed("warn-call-3", "ls", listArgs, "c"))
			return
		}
		if requestNumber == 5 {
			payload, _ := json.Marshal(requestBody.Messages)
			reminderPayload <- string(payload)
		}
		content := "done"
		if requestNumber == 5 {
			content = "still done"
		}
		_, _ = w.Write(activeLoopFinalResponse(content))
	}))
	defer provider.Close()

	assembly, durableSession, _ := newActiveLoopDurableAssembly(ctx, t, provider.URL, "", 2)
	// This test exercises loop-detector continuity across durable turns. Keep
	// its long-lived fixture inside the request envelope so it does not invoke
	// an unrelated configured Unified/Micro compaction path.
	assembly.LoopAgent.SetMidTurnCompaction(assembly.MidTurn, assembly.Compactor, assembly.Estimator, 50000)
	runCtx := bindActiveLoopDurableRun(ctx, t, assembly, durableSession)
	firstRun, err := assembly.Harness.Submit(runCtx, "read the same marker repeatedly")
	require.NoError(t, err)
	for range firstRun.Events() {
	}
	_, err = firstRun.Result()
	require.NoError(t, err, "warn disposition lets later identical tool attempts proceed")
	require.Equal(t, int32(4), providerRequests.Load(), "the run continues through the later ls call and final model answer")
	content, err := os.ReadFile(writePath)
	require.NoError(t, err)
	require.Equal(t, "once", string(content))

	snapshot, err := durableSession.snapshotSelected()
	require.NoError(t, err)
	receipts := 0
	for _, entry := range snapshot.DurableEntries {
		if entry.Type == session.EntryTypeTool && entry.ReceiptOutcome != "" {
			receipts++
			require.Equal(t, string(core.ReceiptOutcomeSuccess), entry.ReceiptOutcome)
			require.Equal(t, session.DispatchStateDispatched, entry.DispatchState)
		}
	}
	require.Equal(t, 3, receipts, "every post-warning execution still receives its canonical durable receipt")

	secondRunCtx := bindActiveLoopDurableRun(ctx, t, assembly, durableSession)
	secondRun, err := assembly.Harness.Submit(secondRunCtx, "continue")
	require.NoError(t, err)
	for range secondRun.Events() {
	}
	_, err = secondRun.Result()
	require.NoError(t, err)
	require.Equal(t, int32(5), providerRequests.Load())
	select {
	case payload := <-reminderPayload:
		require.Contains(t, payload, "Loop detected: identical prepared tool action repeated 2 times (threshold 2). Please try a different approach.")
	case <-time.After(time.Second):
		t.Fatal("next assembled turn did not inject the loop reminder")
	}
}

func newActiveLoopDurableAssembly(ctx context.Context, t *testing.T, providerURL, disposition string, threshold int) (*AgentAssembly, *durableCLISession, string) {
	t.Helper()
	configureDurableCLISessionTest(t)
	workspaceRoot, err := os.Getwd()
	require.NoError(t, err)
	durableSession, err := newDurableCLISession(ctx, filepath.Join(t.TempDir(), "sessions"), "loop-detector", "", "")
	require.NoError(t, err)
	cfg := newE2ETestConfig(t, providerURL)
	cfg.Git.WorkDir = workspaceRoot
	cfg.Sandbox.AllowedPaths = []string{workspaceRoot}
	cfg.Production.LoopDetector.SameToolCallThreshold = threshold
	cfg.Production.LoopDetector.Disposition = disposition
	assembly, err := AssembleAgent(
		ctx,
		cfg,
		cfg.Provider.Name,
		cfg.Provider.Model,
		io.Discard,
		WithApproveMode(ApproveAuto),
		withDurableCLISession(durableSession),
	)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	t.Cleanup(func() { _ = durableSession.requestClose(context.WithoutCancel(ctx)) })
	return assembly, durableSession, workspaceRoot
}

func bindActiveLoopDurableRun(ctx context.Context, t *testing.T, assembly *AgentAssembly, durableSession *durableCLISession) context.Context {
	t.Helper()
	runCtx, sink, err := durableSession.bindRun(ctx)
	require.NoError(t, err)
	if err := hydrateDurableRunHistory(assembly.Agent, sink); err != nil {
		sink.CloseRunCommit()
		require.NoError(t, err)
	}
	t.Cleanup(sink.CloseRunCommit)
	return runCtx
}

func activeLoopToolCallResponse(callID string, arguments []byte, assistantContent ...string) []byte {
	return activeLoopToolCallResponseNamed(callID, "write", arguments, assistantContent...)
}

func activeLoopToolCallResponseNamed(callID, toolName string, arguments []byte, assistantContent ...string) []byte {
	var content any
	if len(assistantContent) > 0 {
		content = assistantContent[0]
	}
	return activeLoopToolCallResponseWithContent(callID, toolName, arguments, content)
}

func activeLoopToolCallResponseWithContent(callID, toolName string, arguments []byte, content any) []byte {
	response, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role": "assistant", "content": content,
				"tool_calls": []any{activeLoopToolCallObject(callID, toolName, arguments)},
			},
		}},
	})
	return response
}

func activeLoopToolCallObject(callID, toolName string, arguments []byte) map[string]any {
	return map[string]any{
		"id": callID, "type": "function",
		"function": map[string]any{"name": toolName, "arguments": string(arguments)},
	}
}

func activeLoopFinalResponse(content string) []byte {
	response, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": content},
		}},
	})
	return response
}

type activeLoopExecutorProbe struct {
	entered chan string
}

func (*activeLoopExecutorProbe) Name() string { return "active_loop_probe" }
func (*activeLoopExecutorProbe) Description() string {
	return "Records entry into the registered test executor."
}
func (*activeLoopExecutorProbe) Parameters() any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"value": map[string]any{"type": "string"}},
		"required":   []string{"value"},
	}
}

func (probe *activeLoopExecutorProbe) Execute(_ context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	probe.entered <- call.ID
	return &tools.ToolResult{Output: "entered"}, nil
}
