package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tui"
	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestDurableREPLUnknownUserCancellationRequiresExplicitNewSession(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	configureDurableCLISessionTest(t)

	testRoot := t.TempDir()
	helperDir := filepath.Join(testRoot, "bin")
	require.NoError(t, os.MkdirAll(helperDir, 0o700))
	startedPath := filepath.Join(testRoot, "grep.started")
	finishedPath := filepath.Join(testRoot, "grep.finished")
	searchHelper := filepath.Join(helperDir, "rg")
	helperSource := "#!/bin/sh\nprintf started > " + startedPath + "\nsleep 60\nprintf finished > " + finishedPath + "\n"
	require.NoError(t, os.WriteFile(searchHelper, []byte(helperSource), 0o700))
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	arguments, err := json.Marshal(map[string]string{"pattern": "LIVE_FACT_ALPHA", "path": "."})
	require.NoError(t, err)
	firstReply, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"id":   "call_cancelled_grep",
					"type": "function",
					"function": map[string]any{
						"name":      "grep",
						"arguments": string(arguments),
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
	})
	require.NoError(t, err)

	var primaryRequests atomic.Int32
	var requestMu sync.Mutex
	var requestBodies []string
	firstRequestSeen := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("read provider request: %v", readErr)
			http.Error(w, "unable to read request", http.StatusBadRequest)
			return
		}
		requestMu.Lock()
		requestBodies = append(requestBodies, string(body))
		requestMu.Unlock()
		if strings.Contains(string(body), "Analyze the following conversation") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"[]"}}]}`)
			return
		}

		requestNumber := primaryRequests.Add(1)
		var response []byte
		switch requestNumber {
		case 1:
			select {
			case firstRequestSeen <- struct{}{}:
			default:
			}
			response = firstReply
		case 2:
			response = []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"fresh session answer"},"finish_reason":"stop"}]}`)
		default:
			t.Errorf("unexpected primary provider request %d", requestNumber)
			response = []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"unexpected"},"finish_reason":"stop"}]}`)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, writeErr := w.Write(response); writeErr != nil {
			t.Errorf("write provider response: %v", writeErr)
		}
	}))
	defer server.Close()

	storeDirectory := filepath.Join(testRoot, "sessions")
	cfg := durablePromptConfig(server.URL, storeDirectory)
	lineEditor := &recoveryGatedLineEditor{results: []lineReadResult{
		{line: "inspect the workspace"},
		{line: "this prompt must remain fenced"},
		{line: "/session new"},
		{line: "fresh prompt after recovery"},
		{line: exitCommand},
	}, blockAt: 2, blocked: make(chan struct{}), proceed: make(chan struct{})}
	var output bytes.Buffer
	cmd := newInteractiveCmd(nil, &output)
	cmd.lineEditor = lineEditor
	repl := &REPLSession{
		cmd:                  cmd,
		out:                  &output,
		lineEditor:           lineEditor,
		extraAssembleOptions: []AssembleOption{WithApproveMode(ApproveAuto)},
	}
	runCtx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- repl.Run(runCtx, cfg, []string{"--no-onboarding"}) }()

	select {
	case <-firstRequestSeen:
	case <-runCtx.Done():
		t.Fatalf("interactive run ended before the first provider request: %v", runCtx.Err())
	}
	require.NoError(t, waitForTestFile(runCtx, startedPath), "the dispatched grep helper must start before user cancellation")
	oldSessionID := physicalSessionIDFromAssembly(t, repl.assembly)
	require.NotEmpty(t, oldSessionID)
	require.NotNil(t, repl.app)
	select {
	case <-repl.app.ProgramReady():
	case <-runCtx.Done():
		t.Fatalf("TUI program did not become ready: %v", runCtx.Err())
	}
	program := repl.app.Program()
	require.NotNil(t, program)
	program.Send(tea.KeyMsg{Type: tea.KeyEsc})
	select {
	case <-lineEditor.blocked:
	case <-runCtx.Done():
		t.Fatalf("REPL did not reach the recovery command after cancellation: %v", runCtx.Err())
	}
	require.NotNil(t, repl.assembly)
	require.NotNil(t, repl.assembly.durableSession)
	index := repl.assembly.durableSession.index
	workspaceID := repl.assembly.durableSession.workspaceID
	quarantineBatch := unknownMutationWorkspaceBatch(workspaceID, "external-session", "external-run", "external-batch", "external-action", "external-call")
	dispatch, err := index.BeginDispatchBatch(runCtx, quarantineBatch)
	require.NoError(t, err)
	require.Equal(t, session.WorkspaceDispatchCommittedNow, dispatch)
	close(lineEditor.proceed)

	select {
	case runErr := <-runDone:
		require.NoError(t, runErr, "an explicitly canceled, quiescent unknown run should return to the REPL for /session new")
	case <-runCtx.Done():
		t.Fatalf("interactive REPL did not return to input after cancellation: %v", runCtx.Err())
	}

	assert.Contains(t, strings.ToLower(output.String()), "interrupted")
	assert.Contains(t, strings.ToLower(output.String()), "unknown")
	assert.Contains(t, output.String(), "/session new")
	assert.Contains(t, output.String(), "no new model request was sent")
	assert.NoFileExists(t, finishedPath, "the canceled helper must not run its post-sleep side effect")
	assert.EqualValues(t, 2, primaryRequests.Load(), "the canceled run is never replayed and text stays fenced until /session new")

	newSessionID := newPhysicalSessionIDFromOutput(t, output.String())
	assert.NotEqual(t, oldSessionID, newSessionID, "explicit recovery must select a new physical session")
	newSession, err := newDurableCLISession(t.Context(), storeDirectory, "recovery-review", newSessionID, "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, newSession.requestClose(context.Background())) })
	selected, err := newSession.snapshotSelected()
	require.NoError(t, err)
	assert.Equal(t, newSessionID, selected.SessionID)
	assert.Equal(t, core.RunOutcomeSuccess, durableTerminalOutcome(selected, selectedRunID(selected)))
	require.Contains(t, messageContents(selected), "fresh prompt after recovery")
	require.Contains(t, messageContents(selected), "fresh session answer")
	require.NotContains(t, messageContents(selected), "inspect the workspace", "the new session must not inherit the canceled transcript")

	oldSession, err := newDurableCLISession(t.Context(), storeDirectory, "recovery-review", oldSessionID, "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, oldSession.requestClose(context.Background())) })
	oldSnapshot, err := oldSession.snapshotSelected()
	require.NoError(t, err)
	oldRunID := selectedRunID(oldSnapshot)
	require.NotEmpty(t, oldRunID)
	pending, err := session.InspectPendingActions(oldSnapshot)
	require.NoError(t, err)
	assert.Empty(t, pending, "the joined local-resource resolution closes the old action's resource risk without rewriting its unknown receipt")
	var receiptCount, localQuiescenceResolutionCount int
	var receiptActionID, resolutionActionID string
	for _, row := range oldSnapshot.DurableEntries {
		if row.Type == session.EntryTypeTool && row.RunID == oldRunID && row.ActionID != "" && row.ActionState == "" && row.ReceiptOutcome == core.ReceiptOutcomeUnknown {
			receiptCount++
			receiptActionID = row.ActionID
		}
		if row.Type == session.EntryTypeCheckpoint && row.CheckpointKind == session.CheckpointExecutionResolution &&
			row.RunID == oldRunID && row.ExecutionResolution != nil && row.ExecutionResolution.Evidence == core.ResolutionLocalResourcesStopped {
			localQuiescenceResolutionCount++
			resolutionActionID = row.ExecutionResolution.ActionID
		}
	}
	assert.Equal(t, 1, receiptCount, "the original action keeps exactly one unknown receipt")
	assert.Equal(t, 1, localQuiescenceResolutionCount, "the durable local-quiescence proof remains attached to the original action")
	assert.Equal(t, receiptActionID, resolutionActionID, "the local-quiescence proof belongs to the original unknown receipt")
	assert.Equal(t, core.RunOutcomeUnknown, durableTerminalOutcome(oldSnapshot, oldRunID), "the original run terminal remains unknown")

	mutationAttempt := unknownMutationWorkspaceBatch(workspaceID, "another-session", "another-run", "another-batch", "another-action", "another-call")
	dispatch, err = index.BeginDispatchBatch(runCtx, mutationAttempt)
	require.ErrorIs(t, err, session.ErrWorkspaceQuarantined, "starting a fresh physical session must not clear an unresolved workspace mutation marker")
	require.Equal(t, session.WorkspaceDispatchNotCommitted, dispatch)
	decisions, err := index.InspectDispatchMarkers(runCtx, []session.PendingActionRef{quarantineBatch.Actions[0].Action})
	require.NoError(t, err)
	require.Len(t, decisions, 1)
	require.Equal(t, session.DispatchMarkerPresent, decisions[0].MarkerState, "the unresolved marker remains authoritative after /session new")

	requestMu.Lock()
	bodies := append([]string(nil), requestBodies...)
	requestMu.Unlock()
	require.Len(t, bodies, 2)
	assert.Contains(t, bodies[1], "fresh prompt after recovery")
	assert.NotContains(t, bodies[1], "inspect the workspace", "the fresh provider request must use the new session only")
}

func TestUnknownCancellationAllowlistUsesRegisteredHandlers(t *testing.T) {
	buildSessionRegistry := func(t *testing.T, mode sessionCommandMode, recoveryEnabled bool) *SlashCommandRegistry {
		t.Helper()
		registry := buildSlashCommandRegistry()
		registered, ok := registry.Lookup("session")
		require.True(t, ok)
		handler, ok := registered.(*SessionHandler)
		require.True(t, ok)
		handler.mode = mode
		if recoveryEnabled {
			handler.recoveryNewSession = func(context.Context) (string, error) { return "new-session", nil }
		}
		return registry
	}
	registryWithHandler := func(t *testing.T, handler SlashCommandHandler) *SlashCommandRegistry {
		t.Helper()
		registry := NewSlashCommandRegistry()
		require.NoError(t, registry.Register(handler))
		return registry
	}

	tests := []struct {
		name     string
		command  session.SlashCommand
		registry func(*testing.T) *SlashCommandRegistry
		allow    bool
	}{
		{
			name:     "registered help handler",
			command:  session.SlashCommand{Name: "help"},
			registry: func(*testing.T) *SlashCommandRegistry { return buildSlashCommandRegistry() },
			allow:    true,
		},
		{
			name:     "registered help alias",
			command:  session.SlashCommand{Name: "h"},
			registry: func(*testing.T) *SlashCommandRegistry { return buildSlashCommandRegistry() },
			allow:    true,
		},
		{
			name:     "custom handler cannot impersonate help",
			command:  session.SlashCommand{Name: "help"},
			registry: func(t *testing.T) *SlashCommandRegistry { return registryWithHandler(t, &fakeHandler{name: "help"}) },
		},
		{
			name:    "durable session pending",
			command: session.SlashCommand{Name: "session", Args: []string{"pending"}},
			registry: func(t *testing.T) *SlashCommandRegistry {
				return buildSessionRegistry(t, sessionCommandModeDurable, true)
			},
			allow: true,
		},
		{
			name:    "durable session new with recovery handler",
			command: session.SlashCommand{Name: "session", Args: []string{"new"}},
			registry: func(t *testing.T) *SlashCommandRegistry {
				return buildSessionRegistry(t, sessionCommandModeDurable, true)
			},
			allow: true,
		},
		{
			name:    "durable session new without recovery handler",
			command: session.SlashCommand{Name: "session", Args: []string{"new"}},
			registry: func(t *testing.T) *SlashCommandRegistry {
				return buildSessionRegistry(t, sessionCommandModeDurable, false)
			},
		},
		{
			name:    "legacy session pending",
			command: session.SlashCommand{Name: "session", Args: []string{"pending"}},
			registry: func(t *testing.T) *SlashCommandRegistry {
				return buildSessionRegistry(t, sessionCommandModeLegacy, true)
			},
		},
		{
			name:    "unavailable session new",
			command: session.SlashCommand{Name: "session", Args: []string{"new"}},
			registry: func(t *testing.T) *SlashCommandRegistry {
				return buildSessionRegistry(t, sessionCommandModeUnavailable, true)
			},
		},
		{
			name:    "session pending with extra argument",
			command: session.SlashCommand{Name: "session", Args: []string{"pending", "extra"}},
			registry: func(t *testing.T) *SlashCommandRegistry {
				return buildSessionRegistry(t, sessionCommandModeDurable, true)
			},
		},
		{
			name:    "unsupported durable subcommand",
			command: session.SlashCommand{Name: "session", Args: []string{"reconcile"}},
			registry: func(t *testing.T) *SlashCommandRegistry {
				return buildSessionRegistry(t, sessionCommandModeDurable, true)
			},
		},
		{
			name:     "custom handler cannot impersonate session",
			command:  session.SlashCommand{Name: "session", Args: []string{"pending"}},
			registry: func(t *testing.T) *SlashCommandRegistry { return registryWithHandler(t, &fakeHandler{name: "session"}) },
		},
		{
			name:     "unknown command",
			command:  session.SlashCommand{Name: "custom"},
			registry: func(*testing.T) *SlashCommandRegistry { return buildSlashCommandRegistry() },
		},
		{
			name:     "missing registry",
			command:  session.SlashCommand{Name: "help"},
			registry: func(*testing.T) *SlashCommandRegistry { return nil },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.allow, allowedDuringUnknownCancellation(test.command, test.registry(t)))
		})
	}
}

func TestUnknownCancellationOwnerEvidenceFailsClosed(t *testing.T) {
	valid := unknownCancellationOwnerEvidence{
		explicitUICancel: true,
		turnContextErr:   context.Canceled,
		statusHandle:     true,
		resultRunID:      "run-unknown",
		initialSessionID: "physical-1",
		joinedStatus: core.RunStatus{
			RunID: "run-unknown", Outcome: core.RunOutcomeUnknown, ExecutionState: core.RunExecutionQuiescent,
		},
		publication: core.DurablePublication{
			State: core.DurablePublicationSynced, RunID: "run-unknown", SessionID: "physical-1", OwnerGeneration: "owner-1",
		},
	}

	tests := []struct {
		name   string
		mutate func(*unknownCancellationOwnerEvidence)
	}{
		{name: "valid explicit cancellation", mutate: func(*unknownCancellationOwnerEvidence) {}},
		{name: "no explicit UI cancellation", mutate: func(e *unknownCancellationOwnerEvidence) { e.explicitUICancel = false }},
		{name: "parent context ended", mutate: func(e *unknownCancellationOwnerEvidence) { e.parentContextErr = context.Canceled }},
		{name: "turn was not canceled", mutate: func(e *unknownCancellationOwnerEvidence) { e.turnContextErr = nil }},
		{name: "owner join failed or remains unjoined", mutate: func(e *unknownCancellationOwnerEvidence) { e.joinErr = context.DeadlineExceeded }},
		{name: "status handle missing", mutate: func(e *unknownCancellationOwnerEvidence) { e.statusHandle = false }},
		{name: "result run missing", mutate: func(e *unknownCancellationOwnerEvidence) { e.resultRunID = "" }},
		{name: "owner run mismatch", mutate: func(e *unknownCancellationOwnerEvidence) { e.joinedStatus.RunID = "other-run" }},
		{name: "owner not quiescent", mutate: func(e *unknownCancellationOwnerEvidence) {
			e.joinedStatus.ExecutionState = core.RunExecutionCleanupPending
		}},
		{name: "owner outcome known", mutate: func(e *unknownCancellationOwnerEvidence) { e.joinedStatus.Outcome = core.RunOutcomeCancelled }},
		{name: "publication pending", mutate: func(e *unknownCancellationOwnerEvidence) { e.publication.State = core.DurablePublicationPending }},
		{name: "publication run mismatch", mutate: func(e *unknownCancellationOwnerEvidence) { e.publication.RunID = "other-run" }},
		{name: "publication session mismatch", mutate: func(e *unknownCancellationOwnerEvidence) { e.publication.SessionID = "other-session" }},
		{name: "publication generation missing", mutate: func(e *unknownCancellationOwnerEvidence) { e.publication.OwnerGeneration = "" }},
		{name: "initial session missing", mutate: func(e *unknownCancellationOwnerEvidence) { e.initialSessionID = "" }},
		{name: "run slot active", mutate: func(e *unknownCancellationOwnerEvidence) { e.runSlotActive = true }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := valid
			test.mutate(&evidence)
			if test.name == "valid explicit cancellation" {
				require.True(t, validUnknownCancellationOwnerEvidence(evidence))
				return
			}
			require.False(t, validUnknownCancellationOwnerEvidence(evidence), "recovery must require every owner/publication/cancellation fact")
		})
	}
}

func TestUnknownCancellationSelectionRevalidatesSavedIdentity(t *testing.T) {
	snapshot := projectionSnapshot([]session.SessionEntry{
		{ID: "unknown-user", Type: session.EntryTypeUser, Content: "original request"},
		{ID: "unknown-terminal", ParentID: "unknown-user", Type: session.EntryTypeCheckpoint, RunID: "run-unknown",
			CheckpointKind: session.CheckpointRunTerminal, RunOutcome: core.RunOutcomeUnknown},
	}, []core.AgentMessage{{Role: "user", Content: "original request"}}, []string{"unknown-user"}, "unknown-terminal")
	status := core.RunStatus{RunID: "run-unknown", Outcome: core.RunOutcomeUnknown, ExecutionState: core.RunExecutionQuiescent}
	publication := core.DurablePublication{
		State: core.DurablePublicationSynced, RunID: "run-unknown", SessionID: snapshot.SessionID, OwnerGeneration: "owner-1",
	}
	handle := projectionStatusHandle{status: status}
	observation, err := ProjectRunObservation(context.Background(), snapshot, handle, "run-unknown")
	require.NoError(t, err)
	recovery := &durableUnknownCancelRecovery{
		sessionID: snapshot.SessionID, activeBranchID: snapshot.ActiveBranchID,
		runID: "run-unknown", ownerGeneration: "owner-1", terminalEntryID: "unknown-terminal",
	}

	require.NoError(t, validateUnknownCancellationRecoverySelection(recovery, "run-unknown", status, publication, snapshot, observation))

	tests := []struct {
		name   string
		mutate func(*durableUnknownCancelRecovery, *string, *core.RunStatus, *core.DurablePublication, *session.PreparedSessionSnapshot, *tui.RunObservation)
	}{
		{name: "result run changed", mutate: func(_ *durableUnknownCancelRecovery, resultRunID *string, _ *core.RunStatus, _ *core.DurablePublication, _ *session.PreparedSessionSnapshot, _ *tui.RunObservation) {
			*resultRunID = "other-run"
		}},
		{name: "owner run changed", mutate: func(_ *durableUnknownCancelRecovery, _ *string, status *core.RunStatus, _ *core.DurablePublication, _ *session.PreparedSessionSnapshot, _ *tui.RunObservation) {
			status.RunID = "other-run"
		}},
		{name: "publication run changed", mutate: func(_ *durableUnknownCancelRecovery, _ *string, _ *core.RunStatus, publication *core.DurablePublication, _ *session.PreparedSessionSnapshot, _ *tui.RunObservation) {
			publication.RunID = "other-run"
		}},
		{name: "owner generation changed", mutate: func(_ *durableUnknownCancelRecovery, _ *string, _ *core.RunStatus, publication *core.DurablePublication, _ *session.PreparedSessionSnapshot, _ *tui.RunObservation) {
			publication.OwnerGeneration = "stale-owner"
		}},
		{name: "physical session changed", mutate: func(_ *durableUnknownCancelRecovery, _ *string, _ *core.RunStatus, _ *core.DurablePublication, snapshot *session.PreparedSessionSnapshot, _ *tui.RunObservation) {
			snapshot.SessionID = "other-session"
		}},
		{name: "branch changed", mutate: func(_ *durableUnknownCancelRecovery, _ *string, _ *core.RunStatus, _ *core.DurablePublication, snapshot *session.PreparedSessionSnapshot, _ *tui.RunObservation) {
			snapshot.ActiveBranchID = "other-branch"
		}},
		{name: "terminal identity changed", mutate: func(recovery *durableUnknownCancelRecovery, _ *string, _ *core.RunStatus, _ *core.DurablePublication, _ *session.PreparedSessionSnapshot, _ *tui.RunObservation) {
			recovery.terminalEntryID = "stale-terminal"
		}},
		{name: "terminal missing", mutate: func(_ *durableUnknownCancelRecovery, _ *string, _ *core.RunStatus, _ *core.DurablePublication, _ *session.PreparedSessionSnapshot, observation *tui.RunObservation) {
			observation.TerminalEntryID = ""
		}},
		{name: "terminal outcome changed", mutate: func(_ *durableUnknownCancelRecovery, _ *string, _ *core.RunStatus, _ *core.DurablePublication, _ *session.PreparedSessionSnapshot, observation *tui.RunObservation) {
			observation.TerminalOutcome = core.RunOutcomeSuccess
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changedRecovery := *recovery
			resultRunID := "run-unknown"
			changedStatus := status
			changedPublication := publication
			changedSnapshot := snapshot.Clone()
			changedObservation := observation
			test.mutate(&changedRecovery, &resultRunID, &changedStatus, &changedPublication, &changedSnapshot, &changedObservation)
			require.Error(t, validateUnknownCancellationRecoverySelection(&changedRecovery, resultRunID, changedStatus, changedPublication, changedSnapshot, changedObservation))
		})
	}

	missingTerminal := snapshot.Clone()
	missingTerminal.DurableEntries[1].RunID = "other-run"
	missingObservation, err := ProjectRunObservation(context.Background(), missingTerminal, handle, "run-unknown")
	require.NoError(t, err)
	require.Error(t, validateUnknownCancellationRecoverySelection(recovery, "run-unknown", status, publication, missingTerminal, missingObservation))

	ambiguousTerminal := snapshot.Clone()
	duplicate := ambiguousTerminal.DurableEntries[1]
	duplicate.ID = "unknown-terminal-duplicate"
	duplicate.ParentID = "unknown-terminal"
	duplicate.Seq = 3
	ambiguousTerminal.DurableEntries = append(ambiguousTerminal.DurableEntries, duplicate)
	ambiguousTerminal.MaxSeq = 3
	ambiguousTerminal.ActiveLeafEntryID = duplicate.ID
	_, err = ProjectRunObservation(context.Background(), ambiguousTerminal, handle, "run-unknown")
	require.Error(t, err, "multiple terminal checkpoints must fail closed")
}

func TestUnknownCancellationNewSessionFailureKeepsFence(t *testing.T) {
	recovery := &durableUnknownCancelRecovery{
		sessionID: "physical-old", activeBranchID: "main", runID: "run-unknown",
		ownerGeneration: "owner-1", terminalEntryID: "terminal-1",
	}
	newREPL := func(guard core.RunSlotGuard, manager *durableSessionManager) *REPLSession {
		return &REPLSession{
			unknownCancelRecovery: recovery,
			assembly: &AgentAssembly{
				durableSession: &durableCLISession{manager: manager}, runSlotGuard: guard,
			},
		}
	}

	repl := newREPL(core.NewDefaultRunSlotGuard(), nil)
	_, err := repl.startNewSessionAfterUnknownCancellation(context.Background())
	require.Error(t, err, "an unavailable manager must fail closed")
	require.Same(t, recovery, repl.unknownCancelRecovery, "a failed recovery attempt keeps the ordinary-prompt fence")
	var output bytes.Buffer
	repl.out = &output
	repl.spanCtx = context.Background()
	repl.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	repl.lineEditor = &scriptedLineEditor{results: []lineReadResult{{line: "write a file"}, {line: exitCommand}}}
	require.NoError(t, repl.readInput())
	require.Contains(t, output.String(), "no new model request was sent")

	activeGuard := activeRunSlotTestGuard{}
	repl = newREPL(activeGuard, &durableSessionManager{})
	_, err = repl.startNewSessionAfterUnknownCancellation(context.Background())
	require.ErrorContains(t, err, "run owner is active")
	require.Same(t, recovery, repl.unknownCancelRecovery, "an active slot must leave recovery fenced")
}

func waitForTestFile(ctx context.Context, path string) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func durableTerminalOutcome(snapshot session.PreparedSessionSnapshot, runID string) core.RunOutcome {
	var outcome core.RunOutcome
	for _, entry := range snapshot.DurableEntries {
		if entry.Type == session.EntryTypeCheckpoint && entry.CheckpointKind == session.CheckpointRunTerminal && entry.RunID == runID {
			if outcome != "" {
				return ""
			}
			outcome = entry.RunOutcome
		}
	}
	return outcome
}

func selectedRunID(snapshot session.PreparedSessionSnapshot) string {
	for index := len(snapshot.DurableEntries) - 1; index >= 0; index-- {
		entry := snapshot.DurableEntries[index]
		if entry.Type == session.EntryTypeCheckpoint && entry.CheckpointKind == session.CheckpointRunTerminal {
			return entry.RunID
		}
	}
	return ""
}

func messageContents(snapshot session.PreparedSessionSnapshot) []string {
	contents := make([]string, 0, len(snapshot.PublishedView.Messages))
	for _, message := range snapshot.PublishedView.Messages {
		contents = append(contents, message.Content)
	}
	return contents
}

func newPhysicalSessionIDFromOutput(t *testing.T, output string) string {
	t.Helper()
	const prefix = "Started fresh durable session "
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			id := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			id = strings.TrimSuffix(id, ". The prior run and any unresolved actions remain unchanged.")
			require.NotEmpty(t, id)
			return id
		}
	}
	t.Fatalf("output does not expose the newly selected physical session ID: %q", output)
	return ""
}

type recoveryGatedLineEditor struct {
	results []lineReadResult
	blockAt int
	readAt  int
	blocked chan struct{}
	proceed chan struct{}
}

func (editor *recoveryGatedLineEditor) ReadLine(ctx context.Context, _ string) (string, error) {
	if editor.readAt == editor.blockAt {
		close(editor.blocked)
		select {
		case <-editor.proceed:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if editor.readAt >= len(editor.results) {
		return "", io.EOF
	}
	result := editor.results[editor.readAt]
	editor.readAt++
	return result.line, result.err
}

func (*recoveryGatedLineEditor) SetHistory([]string)    {}
func (*recoveryGatedLineEditor) History() []string      { return nil }
func (*recoveryGatedLineEditor) SetCompleter(Completer) {}

func unknownMutationWorkspaceBatch(workspaceID, sessionID, runID, batchID, actionID, callID string) session.WorkspaceDispatchBatch {
	return session.WorkspaceDispatchBatch{
		WorkspaceID: workspaceID, SessionID: sessionID, RunID: runID, ActionBatchID: batchID,
		Actions: []session.WorkspaceDispatchAction{{
			Action: session.PendingActionRef{
				SessionID: sessionID, WorkspaceID: workspaceID, RunID: runID,
				ActionBatchID: batchID, ActionID: actionID, ToolCallID: callID,
			},
			EffectSet: []tools.TrustedToolEffect{tools.EffectWorkspaceWrite},
		}},
	}
}

type activeRunSlotTestGuard struct {
	core.RunSlotGuard
}

func (activeRunSlotTestGuard) RegisterRunWrapper(core.RunClaim, core.RunStatusHandle) (func(), error) {
	return func() {}, nil
}

func (activeRunSlotTestGuard) SetRunWrapperStatus(core.RunClaim, core.RunStatusHandle) error {
	return nil
}

func (activeRunSlotTestGuard) ActiveRunStatus() (core.RunStatus, bool) {
	return core.RunStatus{RunID: "active-run", ExecutionState: core.RunExecutionRunning}, true
}
