package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type reconciliationSeedTool struct{}

func (*reconciliationSeedTool) Name() string        { return "reconcile_seed" }
func (*reconciliationSeedTool) Description() string { return "test-only durable unknown action" }
func (*reconciliationSeedTool) Parameters() any {
	return map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}}
}
func (*reconciliationSeedTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	return &tools.ToolResult{Output: "must not execute during reconciliation"}, nil
}

type durableUnknownActionSeeder struct {
	sink    core.RunCommitSink
	binding core.RunCommitBinding
	intent  core.ActionIntent
}

func (*durableUnknownActionSeeder) Name() string { return "durable-unknown-action-seeder" }

func (agent *durableUnknownActionSeeder) Run(ctx context.Context, _ core.Submission, _ ...core.EventStream) (core.Result, error) {
	ctx = core.WithRunCommit(ctx, agent.sink, agent.binding)
	runID := core.RunIDFromContext(ctx)
	intent := agent.intent
	intent.RunID = runID
	intent.SessionID = agent.binding.SessionID
	intent.WorkspaceID = agent.binding.WorkspaceID
	intent.OwnerGeneration = agent.binding.OwnerGeneration
	user := core.AgentMessage{Role: "user", Content: "make a write", InputKind: core.InputKindUser}
	assistant := core.AgentMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: intent.ToolCallID, Name: "reconcile_seed", Args: map[string]any{"value": "private-argument"}}}}
	unknown := core.AgentMessage{Role: "tool", ToolCallID: intent.ToolCallID, ToolName: "reconcile_seed", Content: "outcome unknown", ReceiptOutcome: core.ReceiptOutcomeUnknown}
	if err := agent.sink.CommitInput(ctx, core.RunCommitInput{RunID: runID, TranscriptIndex: 0, Message: user, InputKind: core.InputKindUser}); err != nil {
		return core.Result{}, err
	}
	if err := agent.sink.BeginIntentBatch(ctx, core.RunIntentBatch{
		RunID: runID, ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: 0,
		AssistantMessage: assistant, Intents: []core.ActionIntent{intent},
		CanonicalWorkspaceRoot: agent.binding.CanonicalWorkspaceRoot,
	}); err != nil {
		return core.Result{}, err
	}
	if err := agent.sink.RecordReceipt(ctx, core.RunToolReceipt{
		RunID: runID, AssistantTranscriptOrdinal: 0, ToolCallID: intent.ToolCallID, ActionID: intent.ActionID,
		TranscriptIndex: 2, DispatchState: core.DispatchStateDispatched, ReceiptOutcome: core.ReceiptOutcomeUnknown,
		Message: unknown,
	}); err != nil {
		return core.Result{}, err
	}
	if err := agent.sink.CommitTerminal(ctx, core.RunTerminal{
		RunID:      runID,
		Transcript: []core.TranscriptEntry{{Message: user}, {Message: assistant}, {Message: unknown}},
		Outcome:    core.RunOutcomeUnknown, TerminalCheckpointID: "terminal-cli-unknown-action",
	}); err != nil {
		return core.Result{}, err
	}
	return core.Result{Outcome: core.RunOutcomeUnknown}, nil
}

func seedDurableUnknownAction(t *testing.T, cliSession *durableCLISession) string {
	t.Helper()
	lease, err := cliSession.manager.acquireRunLease()
	require.NoError(t, err)
	sink, err := session.NewSessionRunCommitSink(lease.PreparedSession(), cliSession.index, lease.WorkspaceID(), lease.CloseRunCommit)
	require.NoError(t, err)
	defer sink.CloseRunCommit()

	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, base.Register(t.Context(), &reconciliationSeedTool{}))
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	invocation, err := registry.PrepareToolCall(t.Context(), tools.ToolCall{ID: "call-unknown", Name: "reconcile_seed", Args: map[string]any{"value": "private-argument"}})
	require.NoError(t, err)
	intent := core.ActionIntent{
		WorkspaceID: lease.WorkspaceID(), ActionBatchID: "batch-unknown", AssistantTranscriptOrdinal: 0,
		ToolCallID: "call-unknown", ActionID: "action-unknown", ToolIdentity: invocation.ToolIdentity(),
		SchemaDigest: invocation.SchemaDigest(), ArgumentDigest: invocation.ArgumentDigest(), EffectSet: invocation.EffectSet(),
	}
	seed := &durableUnknownActionSeeder{
		sink: sink,
		binding: core.RunCommitBinding{
			SessionID: lease.SessionID(), WorkspaceID: lease.WorkspaceID(),
			CanonicalWorkspaceRoot: lease.CanonicalWorkspaceRoot(), OwnerGeneration: "cli-test-owner-generation",
		},
		intent: intent,
	}
	harness := core.NewHarnessImpl(seed)
	stream, err := harness.Submit(t.Context(), "seed one unknown action")
	require.NoError(t, err)
	for range stream.Events() {
	}
	_, err = stream.Result()
	require.NoError(t, err)
	return intent.ActionID
}

func TestInteractiveDurableSessionReconcilesUnknownActionThroughSlashRoute(t *testing.T) {
	configureDurableCLISessionTest(t)
	server, provider := newDurablePromptServer(t, `{"choices":[{"message":{"role":"assistant","content":"unexpected provider call"}}]}`)
	defer server.Close()
	storeDirectory := filepath.Join(t.TempDir(), "sessions")
	seedSession, err := newDurableCLISession(t.Context(), storeDirectory, "reconcile", "", "")
	require.NoError(t, err)
	physicalID, err := seedSession.manager.current.prepared.Snapshot()
	require.NoError(t, err)
	actionID := seedDurableUnknownAction(t, seedSession)
	seedSnapshot, err := seedSession.snapshotSelected()
	require.NoError(t, err)
	guard := core.NewDefaultRunSlotGuard()
	claim, err := guard.ClaimRun(t.Context())
	require.NoError(t, err)
	handler := &SessionHandler{mode: sessionCommandModeDurable, durableSession: seedSession, runSlotGuard: guard}
	activeOwnerArgs := []string{actionID,
		"--revision=" + seedSnapshot.PublishedView.Revision,
		"--accept-unknown-risk",
		"--reason=confirmed-out-of-band",
	}
	require.ErrorContains(t, handler.reconcilePendingAction(t.Context(), activeOwnerArgs), "run owner is active")
	guard.Release(claim)
	malformedOptionArgs := append(append([]string(nil), activeOwnerArgs...), "reference=incident-42")
	require.Error(t, handler.reconcilePendingAction(t.Context(), malformedOptionArgs), "options without the required -- prefix must be rejected")
	stillPending, err := seedSession.snapshotSelected()
	require.NoError(t, err)
	stillPendingActions, err := session.InspectPendingActions(stillPending)
	require.NoError(t, err)
	require.Len(t, stillPendingActions, 1)
	require.NoError(t, seedSession.requestClose(t.Context()))

	lines := &scriptedLineEditor{results: []lineReadResult{
		{line: "/session pending"},
		{line: "/session reconcile " + actionID + " --revision=" + seedSnapshot.PublishedView.Revision + " --accept-unknown-risk --reason=confirmed-out-of-band --reference=incident-42"},
		{line: "exit"},
	}}
	var output bytes.Buffer
	cmd := newInteractiveCmd(nil, &output)
	cmd.lineEditor = lines
	repl := &REPLSession{cmd: cmd, out: &output, lineEditor: lines}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	require.NoError(t, repl.Run(ctx, durablePromptConfig(t, server.URL, storeDirectory), []string{"--resume-session", physicalID.SessionID, "--no-onboarding"}))
	require.Empty(t, provider.requestBodies(), "slash recovery commands must not issue a model request")
	require.Contains(t, output.String(), "Unresolved durable actions (tool arguments are omitted):")
	require.Contains(t, output.String(), "action=action-unknown tool=reconcile_seed dispatch=dispatched receipt=unknown reconcile=available")
	require.Contains(t, output.String(), "Recorded explicit reconciliation")
	require.NotContains(t, output.String(), "action is not an unresolved", "an identical retry should repair the already-recorded index resolution")
	require.NotContains(t, output.String(), "private-argument", "pending output must not expose tool arguments")

	readback, err := newDurableCLISession(t.Context(), storeDirectory, "readback", physicalID.SessionID, "")
	require.NoError(t, err)
	defer readback.requestClose(t.Context())
	after, err := readback.snapshotSelected()
	require.NoError(t, err)
	var receipt, reconciliation, resolution *session.SessionEntry
	for index := range after.DurableEntries {
		row := &after.DurableEntries[index]
		switch {
		case row.Type == session.EntryTypeTool && row.ActionID == actionID && row.ActionState == "":
			receipt = row
		case row.CheckpointKind == session.CheckpointReconciliation:
			reconciliation = row
		case row.CheckpointKind == session.CheckpointExecutionResolution:
			resolution = row
		}
	}
	require.NotNil(t, receipt)
	require.Equal(t, core.ReceiptOutcomeUnknown, receipt.ReceiptOutcome)
	require.NotNil(t, reconciliation)
	require.NotNil(t, resolution)
	require.Equal(t, "confirmed-out-of-band", reconciliation.OperatorReconciliation.Reason)
	require.Equal(t, "incident-42", reconciliation.OperatorReconciliation.Reference)
	require.Equal(t, reconciliation.ID, resolution.ExecutionResolution.EvidenceRef)
	pending, err := session.InspectPendingActions(after)
	require.NoError(t, err)
	require.Empty(t, pending)
	resolvedAction, found, err := session.FindActionByID(after, actionID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, resolvedAction.Resolved)
	require.Equal(t, core.ResolutionOperatorReconciled, resolvedAction.Resolution)

	readbackHandler := &SessionHandler{mode: sessionCommandModeDurable, durableSession: readback}
	require.NoError(t, readbackHandler.reconcilePendingAction(t.Context(), []string{
		actionID,
		"--revision=" + after.PublishedView.Revision,
		"--accept-unknown-risk",
		"--reason=confirmed-out-of-band",
		"--reference=incident-42",
	}), "an identical retry should idempotently resync the stored resolution")
}
