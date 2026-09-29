package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

var errTestRunCommitContextMissing = errors.New("test: run-commit context missing")

type preparedSessionWithoutWriter struct{ PreparedSession }

func (preparedSessionWithoutWriter) DurableWriter() DurableSessionWriter { return nil }

type sessionCommitTestAgent struct {
	sink    core.RunCommitSink
	binding core.RunCommitBinding
	commit  func(context.Context, core.RunCommitSink, core.RunCommitBinding, string) error
}

func (*sessionCommitTestAgent) Name() string { return "session-commit-test-agent" }

func (a *sessionCommitTestAgent) Run(ctx context.Context, _ core.Submission, _ ...core.EventStream) (core.Result, error) {
	// These adapter tests call the sink's low-level methods directly. Bind it
	// inside the callback so Harness does not treat the fixture as a durable
	// production run that must acknowledge a terminal checkpoint.
	ctx = core.WithRunCommit(ctx, a.sink, a.binding)
	sink, binding, ok := core.RunCommitFromContext(ctx)
	if !ok {
		return core.Result{}, errTestRunCommitContextMissing
	}
	runID := core.RunIDFromContext(ctx)
	if err := a.commit(ctx, sink, binding, runID); err != nil {
		return core.Result{}, err
	}
	return core.Result{Message: runID}, nil
}

func runSessionCommitAgent(t *testing.T, sink core.RunCommitSink, binding core.RunCommitBinding, commit func(context.Context, core.RunCommitSink, core.RunCommitBinding, string) error) error {
	return runSessionCommitAgentWithContext(context.Background(), t, sink, binding, commit)
}

func runSessionCommitAgentWithContext(parent context.Context, t *testing.T, sink core.RunCommitSink, binding core.RunCommitBinding, commit func(context.Context, core.RunCommitSink, core.RunCommitBinding, string) error) error {
	t.Helper()
	if lease, ok := sink.(core.RunCommitRunLease); ok {
		defer lease.CloseRunCommit()
	}
	harness := core.NewHarnessImpl(&sessionCommitTestAgent{sink: sink, binding: binding, commit: commit})
	stream, err := harness.Submit(parent, "test input")
	require.NoError(t, err)
	for range stream.Events() {
	}
	_, err = stream.Result()
	return err
}

func TestDurableRowMatchesMessageIgnoresRuntimeOnlyMetadata(t *testing.T) {
	row := SessionEntry{
		RecordVersion: 1, Type: EntryTypeTool, Content: "result", ToolCallID: "call-1",
		ToolName: "read", ReceiptOutcome: core.ReceiptOutcomeUnknown,
	}
	expected := core.AgentMessage{
		Role: "tool", Content: "result", ToolCallID: "call-1",
		ToolName: "read", ReceiptOutcome: core.ReceiptOutcomeUnknown,
		Runtime: llm.RuntimeMetadata{
			Source:         llm.SourceRef{Kind: llm.SourceRunTranscript, RunID: "run-1", TranscriptIndex: 2},
			ReceiptOutcome: core.ReceiptOutcomeUnknown,
		},
	}
	require.True(t, durableRowMatchesMessage(row, expected), "json:- source metadata is not part of the persisted row comparison")
}

func TestSessionRunCommitSinkCommitInputPersistsBeforeReturning(t *testing.T) {
	ctx := context.Background()
	prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(ctx, "commit input")
	require.NoError(t, err)
	defer prepared.Close()

	workspaceID := "workspace-v1-adapter"
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	var released int
	sink, err := NewSessionRunCommitSink(prepared, index, workspaceID, func() { released++ })
	require.NoError(t, err)
	binding := core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID, OwnerGeneration: "parent-generation"}
	var closedWriteErr error

	err = runSessionCommitAgent(t, sink, binding, func(ctx context.Context, actual core.RunCommitSink, _ core.RunCommitBinding, runID string) error {
		if actual != sink {
			return errors.New("callback received a different run-commit sink")
		}
		if err := actual.CommitInput(ctx, core.RunCommitInput{
			RunID: runID, TranscriptIndex: 0,
			Message:   core.AgentMessage{Role: "user", Content: "same text is still a distinct input"},
			InputKind: core.InputKindUser,
		}); err != nil {
			return err
		}
		sink.CloseRunCommit()
		closedWriteErr = actual.CommitInput(ctx, core.RunCommitInput{
			RunID: runID, TranscriptIndex: 1,
			Message:   core.AgentMessage{Role: "user", Content: "must not write after lease release"},
			InputKind: core.InputKindFollowUp,
		})
		return nil
	})
	require.NoError(t, err)
	require.Error(t, closedWriteErr)

	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	require.Len(t, snapshot.DurableEntries, 1)
	row := snapshot.DurableEntries[0]
	require.Equal(t, EntryTypeUser, row.Type)
	require.Equal(t, uint64(0), *row.TranscriptIndex)
	require.Equal(t, core.InputKindUser, row.InputKind)
	require.Equal(t, "same text is still a distinct input", row.Content)
	require.Equal(t, uint64(1), row.Seq)
	require.Len(t, snapshot.PublishedView.Messages, 1)
	require.Equal(t, row.ID, snapshot.PublishedView.SourceEntryIDs[0])

	sink.CloseRunCommit()
	sink.CloseRunCommit()
	require.Equal(t, 1, released, "run lease release is idempotent")
	_, err = prepared.Snapshot()
	require.NoError(t, err, "releasing a run lease must not close the shared prepared handle")
}

func TestSessionRunCommitSinkRejectsMismatchedBindingWithoutWriting(t *testing.T) {
	ctx := context.Background()
	prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(ctx, "binding mismatch")
	require.NoError(t, err)
	defer prepared.Close()

	workspaceID := "workspace-v1-adapter"
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	sink, err := NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)
	binding := core.RunCommitBinding{SessionID: "other-session", WorkspaceID: workspaceID, OwnerGeneration: "parent-generation"}

	err = runSessionCommitAgent(t, sink, binding, func(ctx context.Context, actual core.RunCommitSink, _ core.RunCommitBinding, runID string) error {
		return actual.CommitInput(ctx, core.RunCommitInput{
			RunID: runID, TranscriptIndex: 0,
			Message:   core.AgentMessage{Role: "user", Content: "must not persist"},
			InputKind: core.InputKindUser,
		})
	})
	require.Error(t, err)
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	require.Empty(t, snapshot.DurableEntries, "binding mismatch must fail before a session write")
}

func TestSessionRunCommitSinkRejectsPreparedSessionWithoutWriter(t *testing.T) {
	ctx := context.Background()
	prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(ctx, "missing writer")
	require.NoError(t, err)
	defer prepared.Close()
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	require.NoError(t, index.EnsureWorkspace(ctx, "workspace-v1-adapter"))
	_, err = NewSessionRunCommitSink(preparedSessionWithoutWriter{PreparedSession: prepared}, index, "workspace-v1-adapter", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no durable writer")
}

func TestSessionRunCommitSinkRejectsTranscriptGapWithoutWriting(t *testing.T) {
	ctx := context.Background()
	prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(ctx, "transcript gap")
	require.NoError(t, err)
	defer prepared.Close()

	workspaceID := "workspace-v1-adapter"
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	sink, err := NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)
	binding := core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID, OwnerGeneration: "parent-generation"}

	err = runSessionCommitAgent(t, sink, binding, func(ctx context.Context, actual core.RunCommitSink, _ core.RunCommitBinding, runID string) error {
		return actual.CommitInput(ctx, core.RunCommitInput{
			RunID: runID, TranscriptIndex: 1,
			Message:   core.AgentMessage{Role: "user", Content: "gap"},
			InputKind: core.InputKindUser,
		})
	})
	require.Error(t, err)
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	require.Empty(t, snapshot.DurableEntries, "a transcript gap must fail before writing")
}

type sessionRunCommitTestTool struct{}

func (*sessionRunCommitTestTool) Name() string        { return "read" }
func (*sessionRunCommitTestTool) Description() string { return "test read tool" }
func (*sessionRunCommitTestTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
		},
	}
}
func (*sessionRunCommitTestTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	return &tools.ToolResult{}, nil
}

func preparedSessionRunCommitIntent(t *testing.T) core.ActionIntent {
	return preparedSessionRunCommitIntentFor(t, "call-1", "action-1", "batch-1")
}

func preparedSessionRunCommitIntentFor(t *testing.T, toolCallID, actionID, batchID string) core.ActionIntent {
	t.Helper()
	ctx := context.Background()
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry)
	require.NoError(t, base.Register(ctx, &sessionRunCommitTestTool{}))
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	invocation, err := registry.PrepareToolCall(ctx, tools.ToolCall{ID: toolCallID, Name: "read", Args: map[string]any{"path": "README.md"}})
	require.NoError(t, err)
	return core.ActionIntent{
		WorkspaceID: "workspace-v1-adapter", ActionBatchID: batchID,
		AssistantTranscriptOrdinal: 0, ToolCallID: toolCallID, ActionID: actionID,
		ToolIdentity: invocation.ToolIdentity(), SchemaDigest: invocation.SchemaDigest(),
		ArgumentDigest: invocation.ArgumentDigest(), EffectSet: invocation.EffectSet(),
	}
}

func TestSessionRunCommitSinkPersistsIntentBeforeGlobalMarker(t *testing.T) {
	ctx := context.Background()
	prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(ctx, "intent batch")
	require.NoError(t, err)
	defer prepared.Close()
	workspaceID := "workspace-v1-adapter"
	var checkingMarker atomic.Bool
	var markerObservedDurableIntent atomic.Bool
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{write: func(file *os.File, payload []byte) (int, error) {
		if checkingMarker.Load() {
			snapshot, snapshotErr := prepared.Snapshot()
			if snapshotErr == nil {
				for _, row := range snapshot.DurableEntries {
					if row.ActionState == ActionStateIntent && row.ActionID == "action-1" {
						markerObservedDurableIntent.Store(true)
					}
				}
			}
		}
		return file.Write(payload)
	}})
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	sink, err := NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)
	binding := core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID, OwnerGeneration: "parent-generation"}
	intent := preparedSessionRunCommitIntent(t)
	var secondErr error
	checkingMarker.Store(true)
	err = runSessionCommitAgent(t, sink, binding, func(ctx context.Context, actual core.RunCommitSink, runBinding core.RunCommitBinding, runID string) error {
		intent.RunID = runID
		intent.SessionID = runBinding.SessionID
		intent.OwnerGeneration = runBinding.OwnerGeneration
		batch := core.RunIntentBatch{
			RunID: runID, ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: intent.AssistantTranscriptOrdinal,
			AssistantMessage: core.AgentMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: intent.ToolCallID, Name: "read", Args: map[string]any{"path": "README.md"}}}},
			Intents:          []core.ActionIntent{intent},
		}
		if err := actual.BeginIntentBatch(ctx, batch); err != nil {
			return err
		}
		secondErr = actual.BeginIntentBatch(ctx, batch)
		return nil
	})
	require.NoError(t, err)
	require.Error(t, secondErr)
	require.True(t, markerObservedDurableIntent.Load(), "workspace marker write must observe already-synced intent rows")
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	require.Len(t, snapshot.DurableEntries, 2)
	require.Equal(t, EntryTypeAssistant, snapshot.DurableEntries[0].Type)
	require.Equal(t, ActionStateIntent, snapshot.DurableEntries[1].ActionState)
	require.Equal(t, intent.ActionID, snapshot.DurableEntries[1].ActionID)
	decisions, err := index.InspectDispatchMarkers(ctx, []PendingActionRef{{
		SessionID: snapshot.SessionID, WorkspaceID: workspaceID, RunID: intent.RunID,
		ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: intent.AssistantTranscriptOrdinal,
		ToolCallID: intent.ToolCallID, ActionID: intent.ActionID,
	}})
	require.NoError(t, err)
	require.Len(t, decisions, 1)
	require.Equal(t, DispatchMarkerPresent, decisions[0].MarkerState)
}

func TestSessionRunCommitSinkRecordsReceiptBeforeResolvingExactProof(t *testing.T) {
	ctx := context.Background()
	prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(ctx, "receipt and resolution")
	require.NoError(t, err)
	defer prepared.Close()
	workspaceID := "workspace-v1-adapter"
	var failIndexSync atomic.Bool
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{sync: func(file *os.File) error {
		if failIndexSync.Load() {
			return errors.New("injected workspace resolution sync failure")
		}
		return file.Sync()
	}})
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	sink, err := NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)
	binding := core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID, OwnerGeneration: "parent-generation"}
	intent := preparedSessionRunCommitIntent(t)
	var runID string
	err = runSessionCommitAgent(t, sink, binding, func(ctx context.Context, actual core.RunCommitSink, runBinding core.RunCommitBinding, ownedRunID string) error {
		runID = ownedRunID
		intent.RunID = ownedRunID
		intent.SessionID = runBinding.SessionID
		intent.OwnerGeneration = runBinding.OwnerGeneration
		if err := actual.CommitInput(ctx, core.RunCommitInput{RunID: ownedRunID, TranscriptIndex: 0, Message: core.AgentMessage{Role: "user", Content: "read"}, InputKind: core.InputKindUser}); err != nil {
			return err
		}
		batch := core.RunIntentBatch{
			RunID: ownedRunID, ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: 0,
			AssistantMessage: core.AgentMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: intent.ToolCallID, Name: "read", Args: map[string]any{"path": "README.md"}}}},
			Intents:          []core.ActionIntent{intent},
		}
		if err := actual.BeginIntentBatch(ctx, batch); err != nil {
			return err
		}
		execution := core.ActionExecution{
			RunID: ownedRunID, AssistantTranscriptOrdinal: 0, ToolCallID: intent.ToolCallID,
			ActionID: intent.ActionID, OwnerGeneration: intent.OwnerGeneration,
			DispatchState: core.DispatchStateDispatched, ResourceState: core.RunExecutionQuiescent,
			CompletionProof: tools.CompletionInlinePipelineJoined,
		}
		invalidExecution := execution
		invalidExecution.ResourceState = core.RunExecutionState("unknown-state")
		if err := actual.RecordExecution(ctx, invalidExecution); err == nil {
			return errors.New("unsupported resource state was accepted")
		}
		if err := actual.RecordExecution(ctx, execution); err != nil {
			return err
		}
		if err := actual.RecordExecution(ctx, execution); err != nil {
			return err
		}
		receipt := core.RunToolReceipt{
			RunID: ownedRunID, AssistantTranscriptOrdinal: 0, ToolCallID: intent.ToolCallID,
			ActionID: intent.ActionID, TranscriptIndex: 2, DispatchState: core.DispatchStateDispatched,
			ReceiptOutcome: core.ReceiptOutcomeSuccess,
			Message:        core.AgentMessage{Role: "tool", ToolCallID: intent.ToolCallID, ToolName: "read", Content: "ok", ReceiptOutcome: core.ReceiptOutcomeSuccess},
		}
		if err := actual.RecordReceipt(ctx, receipt); err != nil {
			return err
		}
		if err := actual.RecordReceipt(ctx, receipt); err != nil {
			return err
		}
		if err := actual.ResolveExecution(ctx, core.ExecutionResolution{
			RunID: ownedRunID, ActionID: intent.ActionID, Evidence: core.ResolutionRemoteTerminal,
		}); err == nil {
			return errors.New("generic receipt unexpectedly authorized remote terminal resolution")
		}
		decisions, err := index.InspectDispatchMarkers(ctx, []PendingActionRef{{
			SessionID: runBinding.SessionID, WorkspaceID: workspaceID, RunID: ownedRunID,
			ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: 0,
			ToolCallID: intent.ToolCallID, ActionID: intent.ActionID,
		}})
		if err != nil || len(decisions) != 1 || decisions[0].MarkerState != DispatchMarkerPresent {
			return errors.New("remote terminal rejection changed marker state")
		}
		resolution := core.ExecutionResolution{
			RunID: ownedRunID, ActionID: intent.ActionID, Evidence: core.ResolutionLocalResourcesStopped,
		}
		failIndexSync.Store(true)
		resolutionApplyErr := actual.ResolveExecution(ctx, resolution)
		if resolutionApplyErr == nil {
			return errors.New("workspace resolution sync failure was not returned")
		}
		failIndexSync.Store(false)
		if err := actual.ResolveExecution(ctx, resolution); err != nil {
			return err
		}
		return actual.ResolveExecution(ctx, resolution)
	})
	require.NoError(t, err)

	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	var proofRow, receiptRow, resolutionRow *SessionEntry
	for index := range snapshot.DurableEntries {
		row := &snapshot.DurableEntries[index]
		switch {
		case row.ActionState == ActionStateExecution && row.CompletionProof != "":
			proofRow = row
		case row.Type == EntryTypeTool && row.ActionState == "":
			receiptRow = row
		case row.CheckpointKind == CheckpointExecutionResolution:
			resolutionRow = row
		}
	}
	require.NotNil(t, proofRow)
	require.NotNil(t, receiptRow)
	require.NotNil(t, resolutionRow)
	require.Greater(t, proofRow.Seq, uint64(0))
	require.Equal(t, core.RunExecutionQuiescent, proofRow.ResourceState)
	require.Equal(t, proofRow.ID, resolutionRow.ExecutionResolution.EvidenceRef)
	require.Greater(t, receiptRow.Seq, proofRow.Seq)
	require.Len(t, snapshot.DurableEntries, 6, "identical calls must not duplicate synced execution, receipt, or resolution rows")
	decisions, err := index.InspectDispatchMarkers(ctx, []PendingActionRef{{
		SessionID: snapshot.SessionID, WorkspaceID: workspaceID, RunID: runID,
		ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: 0,
		ToolCallID: intent.ToolCallID, ActionID: intent.ActionID,
	}})
	require.NoError(t, err)
	require.Len(t, decisions, 1)
	require.Equal(t, DispatchMarkerResolved, decisions[0].MarkerState)
}

func TestSessionRunCommitSinkBuffersCanonicalReceiptOrderAcrossKnownDenial(t *testing.T) {
	ctx := context.Background()
	prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(ctx, "canonical receipt order")
	require.NoError(t, err)
	defer prepared.Close()
	workspaceID := "workspace-v1-adapter"
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	sink, err := NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)
	binding := core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID, OwnerGeneration: "parent-generation"}
	intent := preparedSessionRunCommitIntentFor(t, "call-1", "action-1", "batch-1")
	var ownedRunID string
	err = runSessionCommitAgent(t, sink, binding, func(ctx context.Context, actual core.RunCommitSink, runBinding core.RunCommitBinding, runID string) error {
		ownedRunID = runID
		intent.RunID, intent.SessionID, intent.OwnerGeneration = runID, runBinding.SessionID, runBinding.OwnerGeneration
		assistant := core.AgentMessage{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "read", Args: map[string]any{"path": "README.md"}},
			{ID: "call-2", Name: "read", Args: map[string]any{"path": "two.txt"}},
		}}
		if err := actual.CommitInput(ctx, core.RunCommitInput{RunID: runID, TranscriptIndex: 0, Message: core.AgentMessage{Role: "user", Content: "read both"}, InputKind: core.InputKindUser}); err != nil {
			return err
		}
		if err := actual.BeginIntentBatch(ctx, core.RunIntentBatch{
			RunID: runID, ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: 0,
			AssistantMessage: assistant, Intents: []core.ActionIntent{intent},
			KnownReceipts: []core.RunToolReceipt{{
				RunID: runID, AssistantTranscriptOrdinal: 0, ToolCallID: "call-2",
				TranscriptIndex: 3, DispatchState: core.DispatchStateNotDispatched,
				ReceiptOutcome: core.ReceiptOutcomeError,
				Message:        core.AgentMessage{Role: "tool", ToolCallID: "call-2", ToolName: "read", Content: "denied", ReceiptOutcome: core.ReceiptOutcomeError},
			}},
		}); err != nil {
			return err
		}
		require.Error(t, actual.RecordReceipt(ctx, core.RunToolReceipt{
			RunID: runID, AssistantTranscriptOrdinal: 0, ToolCallID: "call-2", TranscriptIndex: 3,
			DispatchState: core.DispatchStateNotDispatched, ReceiptOutcome: core.ReceiptOutcomeError,
			Message: core.AgentMessage{Role: "tool", ToolCallID: "call-2", ToolName: "read", Content: "denied", ReceiptOutcome: core.ReceiptOutcomeError},
		}), "later-index denial must not be persisted ahead of an earlier dispatched receipt")
		snapshot, err := prepared.Snapshot()
		require.NoError(t, err)
		require.Len(t, snapshot.DurableEntries, 3, "failed out-of-order receipt must write no bytes")
		if err := actual.RecordReceipt(ctx, core.RunToolReceipt{
			RunID: runID, AssistantTranscriptOrdinal: 0, ToolCallID: "call-1", ActionID: intent.ActionID,
			TranscriptIndex: 2, DispatchState: core.DispatchStateDispatched, ReceiptOutcome: core.ReceiptOutcomeSuccess,
			Message: core.AgentMessage{Role: "tool", ToolCallID: "call-1", ToolName: "read", Content: "ok", ReceiptOutcome: core.ReceiptOutcomeSuccess},
		}); err != nil {
			return err
		}
		denial := core.RunToolReceipt{
			RunID: runID, AssistantTranscriptOrdinal: 0, ToolCallID: "call-2", TranscriptIndex: 3,
			DispatchState: core.DispatchStateNotDispatched, ReceiptOutcome: core.ReceiptOutcomeError,
			Message: core.AgentMessage{Role: "tool", ToolCallID: "call-2", ToolName: "read", Content: "denied", ReceiptOutcome: core.ReceiptOutcomeError},
		}
		if err := actual.RecordReceipt(ctx, denial); err != nil {
			return err
		}
		return actual.RecordReceipt(ctx, denial)
	})
	require.NoError(t, err)
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	var receipts []SessionEntry
	for _, row := range snapshot.DurableEntries {
		if row.Type == EntryTypeTool && row.ActionState == "" {
			receipts = append(receipts, row)
		}
	}
	require.Len(t, receipts, 2)
	require.Equal(t, uint64(2), *receipts[0].TranscriptIndex)
	require.Equal(t, "call-1", receipts[0].ToolCallID)
	require.Equal(t, uint64(3), *receipts[1].TranscriptIndex)
	require.Equal(t, "call-2", receipts[1].ToolCallID)
	require.Empty(t, receipts[1].ActionID, "unreserved denials must not gain a synthetic action identity")
	require.Equal(t, DispatchMarkerPresent, indexMarkerStateForTest(t, index, PendingActionRef{
		SessionID: snapshot.SessionID, WorkspaceID: workspaceID, RunID: ownedRunID,
		ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: 0,
		ToolCallID: intent.ToolCallID, ActionID: intent.ActionID,
	}), "the receipt itself must not resolve a dispatch marker")
}

func TestSessionRunCommitSinkAdmitsOnlyPreparedReadBesideKnownErrorDenial(t *testing.T) {
	ctx := context.Background()
	root := canonicalWorkspaceIndexTestRoot(t, t.TempDir())
	readPath := filepath.Join(root, "safe.txt")
	require.NoError(t, os.WriteFile(readPath, []byte("safe\n"), 0o600))
	workspaceID, err := WorkspaceIDForCanonicalRoot(root)
	require.NoError(t, err)
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	oldMutation := workspaceTestBatch(workspaceID, "session-old", "run-old", "batch-old", "action-old", 0, "write-old")
	oldMutation.Actions[0].EffectSet = []tools.TrustedToolEffect{tools.EffectWorkspaceWrite}
	firstMarker, err := index.BeginDispatchBatch(ctx, oldMutation)
	require.NoError(t, err)
	require.Equal(t, WorkspaceDispatchCommittedNow, firstMarker)

	prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), index).PrepareNew(ctx, "safe read with known denial")
	require.NoError(t, err)
	defer prepared.Close()
	sink, err := NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)
	invocation, proof := preparedWorkspaceIndexRead(t, root, readPath, "safe-read-call")
	call := invocation.NormalizedCall()
	var readAction PendingActionRef
	var executorCalls int
	err = runSessionCommitAgent(t, sink, core.RunCommitBinding{
		SessionID: prepared.View().SessionID, WorkspaceID: workspaceID,
		CanonicalWorkspaceRoot: root, OwnerGeneration: "parent-generation",
	}, func(ctx context.Context, actual core.RunCommitSink, binding core.RunCommitBinding, runID string) error {
		if err := actual.CommitInput(ctx, core.RunCommitInput{
			RunID: runID, TranscriptIndex: 0,
			Message: core.AgentMessage{Role: "user", Content: "read the safe file"}, InputKind: core.InputKindUser,
		}); err != nil {
			return err
		}
		intent := core.ActionIntent{
			SessionID: binding.SessionID, WorkspaceID: binding.WorkspaceID, RunID: runID,
			ActionBatchID: "safe-read-batch", OwnerGeneration: binding.OwnerGeneration,
			AssistantTranscriptOrdinal: 0, ToolCallID: call.ID, ActionID: "safe-read-action",
			ToolIdentity: invocation.ToolIdentity(), SchemaDigest: invocation.SchemaDigest(),
			ArgumentDigest: invocation.ArgumentDigest(), EffectSet: invocation.EffectSet(), ScopedReadProof: proof,
		}
		assistant := core.AgentMessage{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: call.ID, Name: call.Name, Args: call.Args},
			{ID: "denied-write-call", Name: "write", Args: map[string]any{"path": "outside.txt", "content": "blocked"}},
		}}
		denial := core.RunToolReceipt{
			RunID: runID, AssistantTranscriptOrdinal: 0, ToolCallID: "denied-write-call",
			TranscriptIndex: 3, DispatchState: core.DispatchStateNotDispatched,
			ReceiptOutcome: core.ReceiptOutcomeError,
			Message:        core.AgentMessage{Role: "tool", ToolCallID: "denied-write-call", ToolName: "write", Content: "denied", ReceiptOutcome: core.ReceiptOutcomeError},
		}
		if err := actual.BeginIntentBatch(ctx, core.RunIntentBatch{
			RunID: runID, ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: 0,
			AssistantMessage: assistant, Intents: []core.ActionIntent{intent}, KnownReceipts: []core.RunToolReceipt{denial},
			CanonicalWorkspaceRoot: root,
		}); err != nil {
			return err
		}
		readAction = PendingActionRef{
			SessionID: intent.SessionID, WorkspaceID: intent.WorkspaceID, RunID: runID,
			ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: 0,
			ToolCallID: intent.ToolCallID, ActionID: intent.ActionID,
		}
		executorCalls++
		readResult, dispatchErr := invocation.Dispatch(ctx)
		if dispatchErr != nil {
			return dispatchErr
		}
		if readResult == nil || readResult.Output != "safe\n" {
			return fmt.Errorf("actual read result = %#v, want safe file contents", readResult)
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, executorCalls)
	decisions, err := index.InspectDispatchMarkers(ctx, []PendingActionRef{oldMutation.Actions[0].Action, readAction})
	require.NoError(t, err)
	require.Len(t, decisions, 2)
	require.Equal(t, DispatchMarkerPresent, decisions[0].MarkerState, "the earlier mutation remains unresolved")
	require.Equal(t, DispatchMarkerPresent, decisions[1].MarkerState, "the read receives its own marker")

	rows, err := readWorkspaceIndexFile(index.indexPath(workspaceID), filepath.Dir(index.indexPath(workspaceID)), workspaceID)
	require.NoError(t, err)
	var sessionDispatchRows int
	for _, row := range rows {
		if row.Kind == workspaceIndexDispatchReady && row.SessionID == prepared.View().SessionID {
			require.Len(t, row.Actions, 1)
			require.Equal(t, "safe-read-call", row.Actions[0].Action.ToolCallID)
			sessionDispatchRows++
		}
	}
	require.Equal(t, 1, sessionDispatchRows, "only the fresh read intent reaches the global dispatch index")
}

func TestSessionRunCommitSinkRejectsUnknownReceiptBeforeScopedReadAdmission(t *testing.T) {
	ctx := context.Background()
	root := canonicalWorkspaceIndexTestRoot(t, t.TempDir())
	readPath := filepath.Join(root, "safe.txt")
	require.NoError(t, os.WriteFile(readPath, []byte("safe\n"), 0o600))
	workspaceID, err := WorkspaceIDForCanonicalRoot(root)
	require.NoError(t, err)
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	oldMutation := workspaceTestBatch(workspaceID, "session-old", "run-old", "batch-old", "action-old", 0, "write-old")
	_, err = index.BeginDispatchBatch(ctx, oldMutation)
	require.NoError(t, err)

	prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), index).PrepareNew(ctx, "reject unknown receipt")
	require.NoError(t, err)
	defer prepared.Close()
	sink, err := NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)
	invocation, proof := preparedWorkspaceIndexRead(t, root, readPath, "read-before-unknown")
	call := invocation.NormalizedCall()
	var attemptedRef PendingActionRef
	var executorCalls atomic.Int32
	err = runSessionCommitAgent(t, sink, core.RunCommitBinding{
		SessionID: prepared.View().SessionID, WorkspaceID: workspaceID,
		CanonicalWorkspaceRoot: root, OwnerGeneration: "parent-generation",
	}, func(ctx context.Context, actual core.RunCommitSink, binding core.RunCommitBinding, runID string) error {
		if err := actual.CommitInput(ctx, core.RunCommitInput{
			RunID: runID, TranscriptIndex: 0,
			Message: core.AgentMessage{Role: "user", Content: "read with an invalid denial"}, InputKind: core.InputKindUser,
		}); err != nil {
			return err
		}
		intent := core.ActionIntent{
			SessionID: binding.SessionID, WorkspaceID: binding.WorkspaceID, RunID: runID,
			ActionBatchID: "unknown-receipt-batch", OwnerGeneration: binding.OwnerGeneration,
			AssistantTranscriptOrdinal: 0, ToolCallID: call.ID, ActionID: "unknown-receipt-action",
			ToolIdentity: invocation.ToolIdentity(), SchemaDigest: invocation.SchemaDigest(),
			ArgumentDigest: invocation.ArgumentDigest(), EffectSet: invocation.EffectSet(), ScopedReadProof: proof,
		}
		attemptedRef = PendingActionRef{
			SessionID: intent.SessionID, WorkspaceID: intent.WorkspaceID, RunID: runID,
			ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: 0,
			ToolCallID: intent.ToolCallID, ActionID: intent.ActionID,
		}
		unknown := core.RunToolReceipt{
			RunID: runID, AssistantTranscriptOrdinal: 0, ToolCallID: "unknown-write-call",
			TranscriptIndex: 3, DispatchState: core.DispatchStateNotDispatched,
			ReceiptOutcome: core.ReceiptOutcomeUnknown,
			Message:        core.AgentMessage{Role: "tool", ToolCallID: "unknown-write-call", ToolName: "write", Content: "outcome unknown", ReceiptOutcome: core.ReceiptOutcomeUnknown},
		}
		beginErr := actual.BeginIntentBatch(ctx, core.RunIntentBatch{
			RunID: runID, ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: 0,
			AssistantMessage: core.AgentMessage{Role: "assistant", ToolCalls: []llm.ToolCall{
				{ID: call.ID, Name: call.Name, Args: call.Args},
				{ID: "unknown-write-call", Name: "write", Args: map[string]any{"path": "outside.txt", "content": "blocked"}},
			}},
			Intents: []core.ActionIntent{intent}, KnownReceipts: []core.RunToolReceipt{unknown},
			CanonicalWorkspaceRoot: root,
		})
		if beginErr == nil {
			executorCalls.Add(1)
			_, beginErr = invocation.Dispatch(ctx)
		}
		return beginErr
	})
	require.Error(t, err)
	require.Zero(t, executorCalls.Load(), "unknown no-intent denial must fail before read dispatch")
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	require.Len(t, snapshot.DurableEntries, 1, "unknown receipt must be rejected before assistant/intent persistence")
	decisions, err := index.InspectDispatchMarkers(ctx, []PendingActionRef{attemptedRef})
	require.NoError(t, err)
	require.Len(t, decisions, 1)
	require.Equal(t, DispatchMarkerAbsent, decisions[0].MarkerState)
}

func TestSessionRunCommitSinkCommitTerminalAppendsOnlySuffixAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(ctx, "terminal suffix")
	require.NoError(t, err)
	defer prepared.Close()
	workspaceID := "workspace-v1-adapter"
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	sink, err := NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)
	binding := core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID, OwnerGeneration: "parent-generation"}
	user := core.AgentMessage{Role: "user", Content: "finish", InputKind: core.InputKindUser}
	assistant := core.AgentMessage{Role: "assistant", Content: "done"}
	terminal := core.RunTerminal{
		Transcript: []core.TranscriptEntry{{Message: user}, {Message: assistant}},
		Outcome:    core.RunOutcomeSuccess, TerminalCheckpointID: "terminal-run-1",
	}
	var terminalRetryErr, writeAfterTerminalErr error
	err = runSessionCommitAgent(t, sink, binding, func(ctx context.Context, actual core.RunCommitSink, _ core.RunCommitBinding, runID string) error {
		terminal.RunID = runID
		if err := actual.CommitInput(ctx, core.RunCommitInput{RunID: runID, TranscriptIndex: 0, Message: user, InputKind: core.InputKindUser}); err != nil {
			return err
		}
		if err := actual.CommitTerminal(ctx, terminal); err != nil {
			return err
		}
		terminalRetryErr = actual.CommitTerminal(ctx, terminal)
		writeAfterTerminalErr = actual.CommitInput(ctx, core.RunCommitInput{RunID: runID, TranscriptIndex: 2, Message: user, InputKind: core.InputKindFollowUp})
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, terminalRetryErr, "same terminal ID and payload is an idempotent retry")
	require.Error(t, writeAfterTerminalErr, "a run cannot append transcript after its terminal checkpoint")
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	require.Len(t, snapshot.DurableEntries, 3)
	require.Equal(t, EntryTypeUser, snapshot.DurableEntries[0].Type)
	require.Equal(t, EntryTypeAssistant, snapshot.DurableEntries[1].Type)
	require.Equal(t, uint64(1), *snapshot.DurableEntries[1].TranscriptIndex)
	checkpoint := snapshot.DurableEntries[2]
	require.Equal(t, EntryTypeCheckpoint, checkpoint.Type)
	require.Equal(t, CheckpointRunTerminal, checkpoint.CheckpointKind)
	require.Equal(t, "terminal-run-1", checkpoint.ID)
	require.Equal(t, core.RunOutcomeSuccess, checkpoint.RunOutcome)
	require.Len(t, snapshot.PublishedView.Messages, 2)
	require.Equal(t, "done", snapshot.PublishedView.Messages[1].Content)
}

func TestSessionRunCommitSinkAllowsBoundedLateProofAfterTerminalWithoutChangingResult(t *testing.T) {
	parent := context.Background()
	prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(parent, "late completion proof")
	require.NoError(t, err)
	defer prepared.Close()
	workspaceID := "workspace-v1-adapter"
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	require.NoError(t, index.EnsureWorkspace(parent, workspaceID))
	sink, err := NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)
	binding := core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID, OwnerGeneration: "parent-generation"}

	var runID string
	var intent core.ActionIntent
	var proof core.ActionExecution
	var terminal core.RunTerminal
	var wrongExecutionErr, wrongResolutionErr error
	var staleExecutionErr, staleResolutionErr error
	var forbiddenInputErr, forbiddenIntentErr, forbiddenReceiptErr, forbiddenCompactionErr, changedTerminalErr error
	var lateExecutionErr, repeatedExecutionErr, lateResolutionErr, repeatedResolutionErr error
	var closedExecutionErr, closedResolutionErr error
	err = runSessionCommitAgentWithContext(parent, t, sink, binding, func(ctx context.Context, actual core.RunCommitSink, runBinding core.RunCommitBinding, ownedRunID string) error {
		runID = ownedRunID
		var ok bool
		actualSink, actualBinding, ok := core.RunCommitFromContext(ctx)
		if !ok || actualSink != actual || actualBinding.SessionID != runBinding.SessionID || core.RunIDFromContext(ctx) != ownedRunID {
			return errors.New("test: callback did not retain the bound sink, identity, and owned run")
		}
		user := core.AgentMessage{Role: "user", Content: "run the tool", InputKind: core.InputKindUser}
		assistant := core.AgentMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read", Args: map[string]any{"path": "README.md"}}}}
		intent = preparedSessionRunCommitIntent(t)
		intent.RunID, intent.SessionID, intent.OwnerGeneration = ownedRunID, runBinding.SessionID, runBinding.OwnerGeneration
		if err := actual.CommitInput(ctx, core.RunCommitInput{RunID: ownedRunID, TranscriptIndex: 0, Message: user, InputKind: core.InputKindUser}); err != nil {
			return err
		}
		if err := actual.BeginIntentBatch(ctx, core.RunIntentBatch{
			RunID: ownedRunID, ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: intent.AssistantTranscriptOrdinal,
			AssistantMessage: assistant, Intents: []core.ActionIntent{intent},
		}); err != nil {
			return err
		}
		if err := actual.RecordExecution(ctx, core.ActionExecution{
			RunID: ownedRunID, AssistantTranscriptOrdinal: intent.AssistantTranscriptOrdinal, ToolCallID: intent.ToolCallID,
			ActionID: intent.ActionID, OwnerGeneration: intent.OwnerGeneration,
			DispatchState: core.DispatchStateDispatched, ResourceState: core.RunExecutionCleanupPending,
		}); err != nil {
			return err
		}
		receiptMessage := core.AgentMessage{Role: "tool", ToolCallID: intent.ToolCallID, ToolName: "read", Content: "ok", ReceiptOutcome: core.ReceiptOutcomeSuccess}
		if err := actual.RecordReceipt(ctx, core.RunToolReceipt{
			RunID: ownedRunID, AssistantTranscriptOrdinal: intent.AssistantTranscriptOrdinal, ToolCallID: intent.ToolCallID,
			ActionID: intent.ActionID, TranscriptIndex: 2, DispatchState: core.DispatchStateDispatched,
			ReceiptOutcome: core.ReceiptOutcomeSuccess, Message: receiptMessage,
		}); err != nil {
			return err
		}
		terminal = core.RunTerminal{
			RunID:      ownedRunID,
			Transcript: []core.TranscriptEntry{{Message: user}, {Message: assistant}, {Message: receiptMessage}},
			Outcome:    core.RunOutcomeSuccess, TerminalCheckpointID: core.TerminalCheckpointID("terminal-late-proof-" + ownedRunID),
		}
		if err := actual.CommitTerminal(ctx, terminal); err != nil {
			return err
		}
		proof = core.ActionExecution{
			RunID: ownedRunID, AssistantTranscriptOrdinal: intent.AssistantTranscriptOrdinal, ToolCallID: intent.ToolCallID,
			ActionID: intent.ActionID, OwnerGeneration: intent.OwnerGeneration,
			DispatchState: core.DispatchStateDispatched, ResourceState: core.RunExecutionQuiescent,
			CompletionProof: tools.CompletionInlinePipelineJoined,
		}
		wrongExecution := proof
		wrongExecution.ActionID = "other-action"
		wrongExecutionErr = actual.RecordExecution(ctx, wrongExecution)
		wrongResolutionErr = actual.ResolveExecution(ctx, core.ExecutionResolution{
			RunID: ownedRunID, ActionID: "other-action", Evidence: core.ResolutionLocalResourcesStopped,
		})
		staleCtx, staleCancel := context.WithCancel(ctx)
		staleCancel()
		staleExecutionErr = actual.RecordExecution(staleCtx, proof)
		staleResolutionErr = actual.ResolveExecution(staleCtx, core.ExecutionResolution{
			RunID: ownedRunID, ActionID: intent.ActionID, Evidence: core.ResolutionLocalResourcesStopped,
		})
		forbiddenInputErr = actual.CommitInput(ctx, core.RunCommitInput{
			RunID: ownedRunID, TranscriptIndex: 3, Message: core.AgentMessage{Role: "user", Content: "must stay terminal"}, InputKind: core.InputKindFollowUp,
		})
		forbiddenIntentErr = actual.BeginIntentBatch(ctx, core.RunIntentBatch{RunID: ownedRunID})
		forbiddenReceiptErr = actual.RecordReceipt(ctx, core.RunToolReceipt{RunID: ownedRunID})
		forbiddenCompactionErr = actual.CommitCompaction(ctx, core.CompactionProjection{Summary: "must stay terminal"})
		changedTerminal := terminal
		changedTerminal.Outcome = core.RunOutcomeError
		changedTerminalErr = actual.CommitTerminal(ctx, changedTerminal)
		if wrongExecutionErr == nil || wrongResolutionErr == nil || staleExecutionErr == nil || staleResolutionErr == nil || forbiddenInputErr == nil || forbiddenIntentErr == nil || forbiddenReceiptErr == nil || forbiddenCompactionErr == nil || changedTerminalErr == nil {
			return errors.New("test: post-terminal negative operation unexpectedly succeeded")
		}
		if indexMarkerStateForTest(t, index, PendingActionRef{
			SessionID: runBinding.SessionID, WorkspaceID: workspaceID, RunID: ownedRunID,
			ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: intent.AssistantTranscriptOrdinal,
			ToolCallID: intent.ToolCallID, ActionID: intent.ActionID,
		}) != DispatchMarkerPresent {
			return errors.New("test: rejected late proof or resolution changed the workspace marker")
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(ctx, 250*time.Millisecond)
		defer cleanupCancel()
		cleanupDeadline, hasCleanupDeadline := cleanupCtx.Deadline()
		if !hasCleanupDeadline {
			return errors.New("test: late cleanup context has no 250ms deadline")
		}
		lateExecutionErr = actual.RecordExecution(cleanupCtx, proof)
		if lateExecutionErr != nil {
			return lateExecutionErr
		}
		repeatedExecutionErr = actual.RecordExecution(cleanupCtx, proof)
		if repeatedExecutionErr != nil {
			return repeatedExecutionErr
		}
		resolution := core.ExecutionResolution{RunID: ownedRunID, ActionID: intent.ActionID, Evidence: core.ResolutionLocalResourcesStopped}
		lateResolutionErr = actual.ResolveExecution(cleanupCtx, resolution)
		if lateResolutionErr != nil {
			return lateResolutionErr
		}
		repeatedResolutionErr = actual.ResolveExecution(cleanupCtx, resolution)
		if repeatedResolutionErr != nil {
			return repeatedResolutionErr
		}
		if deadlineAfter, _ := cleanupCtx.Deadline(); !cleanupDeadline.Equal(deadlineAfter) {
			return errors.New("test: late cleanup reused a different deadline")
		}
		if err := cleanupCtx.Err(); err != nil {
			return fmt.Errorf("test: shared cleanup deadline expired before resolution sync: %w", err)
		}
		sink.CloseRunCommit()
		closedExecutionErr = actual.RecordExecution(cleanupCtx, proof)
		closedResolutionErr = actual.ResolveExecution(cleanupCtx, resolution)
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, lateExecutionErr, "the original 250ms cleanup context permits the same owner's late proof")
	require.NoError(t, repeatedExecutionErr, "an identical late proof is idempotent")
	require.NoError(t, lateResolutionErr, "the proof's typed resolution clears only its exact marker")
	require.NoError(t, repeatedResolutionErr, "an identical late resolution is idempotent")
	require.Error(t, wrongExecutionErr)
	require.Error(t, wrongResolutionErr)
	require.Error(t, staleExecutionErr)
	require.Error(t, staleResolutionErr)
	require.Error(t, forbiddenInputErr)
	require.Error(t, forbiddenIntentErr)
	require.Error(t, forbiddenReceiptErr)
	require.Error(t, forbiddenCompactionErr)
	require.Error(t, changedTerminalErr)
	require.Error(t, closedExecutionErr, "a returned run lease cannot be reused for late completion writes")
	require.Error(t, closedResolutionErr, "a returned run lease cannot be reused for late resolution writes")
	require.Equal(t, DispatchMarkerResolved, indexMarkerStateForTest(t, index, PendingActionRef{
		SessionID: prepared.View().SessionID, WorkspaceID: workspaceID, RunID: runID,
		ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: intent.AssistantTranscriptOrdinal,
		ToolCallID: intent.ToolCallID, ActionID: intent.ActionID,
	}))
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	require.Len(t, snapshot.DurableEntries, 8, "late proof and resolution append once without altering the terminal rows")
	terminalCount := 0
	var runTranscript []SessionEntry
	for _, row := range snapshot.DurableEntries {
		if row.ID == string(terminal.TerminalCheckpointID) {
			terminalCount++
			require.Equal(t, CheckpointRunTerminal, row.CheckpointKind)
			require.Equal(t, core.RunOutcomeSuccess, row.RunOutcome)
		}
		if row.RunID == runID && row.TranscriptIndex != nil {
			runTranscript = append(runTranscript, row)
		}
	}
	require.Equal(t, 1, terminalCount, "late proof metadata must leave the saved terminal checkpoint unchanged")
	require.Len(t, runTranscript, len(terminal.Transcript), "late metadata must not append transcript rows")
	for index, row := range runTranscript {
		require.Equal(t, uint64(index), *row.TranscriptIndex)
		require.True(t, durableRowMatchesMessage(row, terminal.Transcript[index].Message), "terminal transcript row %d changed after late proof handling", index)
	}
}

func TestSessionRunCommitSinkTerminalRequiresCompleteOrderedDurableProjections(t *testing.T) {
	ctx := context.Background()
	prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(ctx, "terminal projection set")
	require.NoError(t, err)
	defer prepared.Close()
	writer := prepared.DurableWriter()
	seedUser := sessionEntryFromRunCommitMessage(core.AgentMessage{Role: "user", Content: "prior user", InputKind: core.InputKindUser}, EntryTypeUser)
	seedUser.ID = "terminal-projection-prior-user"
	priorUserIndex := uint64(0)
	seedUser.RunID = "terminal-projection-prior-run"
	seedUser.TranscriptIndex = &priorUserIndex
	seedAssistant := sessionEntryFromRunCommitMessage(core.AgentMessage{Role: "assistant", Content: "prior assistant"}, EntryTypeAssistant)
	seedAssistant.ID = "terminal-projection-prior-assistant"
	seedAssistant.ParentID = seedUser.ID
	priorAssistantIndex := uint64(1)
	seedAssistant.RunID = "terminal-projection-prior-run"
	seedAssistant.TranscriptIndex = &priorAssistantIndex
	_, err = writer.AppendAndSync(ctx, seedUser, seedAssistant)
	require.NoError(t, err)
	workspaceID := "workspace-v1-adapter"
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	sink, err := NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)
	binding := core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID, OwnerGeneration: "parent-generation"}
	var missingProjectionErr, exactProjectionErr, omittedRetryErr, reorderedRetryErr, changedRetryErr error
	var rowsBeforeMissingProjection, rowsAfterMissingProjection int
	var terminalID string
	err = runSessionCommitAgent(t, sink, binding, func(ctx context.Context, actual core.RunCommitSink, _ core.RunCommitBinding, runID string) error {
		user := core.AgentMessage{Role: "user", Content: "finish", InputKind: core.InputKindUser}
		assistant := core.AgentMessage{Role: "assistant", Content: "done"}
		if err := actual.CommitInput(ctx, core.RunCommitInput{RunID: runID, TranscriptIndex: 0, Message: user, InputKind: core.InputKindUser}); err != nil {
			return err
		}
		projectionOne := core.CompactionProjection{
			ExpectedHistoryRevision: sink.baseView.Revision,
			Through:                 core.CompactionCursor{PriorHistoryCount: 1}, Summary: "first projection",
		}
		projectionTwo := core.CompactionProjection{
			ExpectedHistoryRevision: sink.baseView.Revision,
			Through:                 core.CompactionCursor{PriorHistoryCount: 2}, Summary: "second projection",
		}
		if err := actual.CommitCompaction(ctx, projectionOne); err != nil {
			return err
		}
		if err := actual.CommitCompaction(ctx, projectionTwo); err != nil {
			return err
		}
		terminal := core.RunTerminal{
			RunID: runID, Transcript: []core.TranscriptEntry{{Message: user}, {Message: assistant}},
			Outcome: core.RunOutcomeSuccess, TerminalCheckpointID: core.TerminalCheckpointID("terminal-projection-set-" + runID),
		}
		terminalID = string(terminal.TerminalCheckpointID)
		before, err := prepared.Snapshot()
		if err != nil {
			return err
		}
		rowsBeforeMissingProjection = len(before.DurableEntries)
		missingProjectionErr = actual.CommitTerminal(ctx, terminal)
		afterMissing, err := prepared.Snapshot()
		if err != nil {
			return err
		}
		rowsAfterMissingProjection = len(afterMissing.DurableEntries)
		terminal.Projections = []core.CompactionProjection{projectionOne, projectionTwo}
		exactProjectionErr = actual.CommitTerminal(ctx, terminal)
		if exactProjectionErr != nil {
			return exactProjectionErr
		}
		omittedRetry := terminal
		omittedRetry.Projections = nil
		omittedRetryErr = actual.CommitTerminal(ctx, omittedRetry)
		reorderedRetry := terminal
		reorderedRetry.Projections = []core.CompactionProjection{projectionTwo, projectionOne}
		reorderedRetryErr = actual.CommitTerminal(ctx, reorderedRetry)
		changedRetry := terminal
		changedRetry.Projections = []core.CompactionProjection{projectionOne, {ExpectedHistoryRevision: projectionTwo.ExpectedHistoryRevision, Through: projectionTwo.Through, Summary: "changed payload"}}
		changedRetryErr = actual.CommitTerminal(ctx, changedRetry)
		return nil
	})
	require.NoError(t, err)
	require.Error(t, missingProjectionErr, "the first terminal must include the complete durable projection set")
	require.Equal(t, rowsBeforeMissingProjection, rowsAfterMissingProjection, "an incomplete first terminal must write no transcript or terminal bytes")
	require.NoError(t, exactProjectionErr)
	require.Error(t, omittedRetryErr, "an idempotent retry cannot omit a durable projection")
	require.Error(t, reorderedRetryErr, "an idempotent retry must preserve projection order")
	require.Error(t, changedRetryErr, "an idempotent retry cannot change a projection payload")
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	var compactions []SessionEntry
	var terminalRows []SessionEntry
	for _, row := range snapshot.DurableEntries {
		if row.Type == EntryTypeCompaction && row.RunID != "" {
			compactions = append(compactions, row)
		}
		if row.Type == EntryTypeCheckpoint && row.CheckpointKind == CheckpointRunTerminal {
			terminalRows = append(terminalRows, row)
		}
	}
	require.Len(t, compactions, 2)
	require.Len(t, terminalRows, 1)
	require.Equal(t, terminalID, terminalRows[0].ID)
	require.Equal(t, "first projection", compactions[0].Summary)
	require.Equal(t, "second projection", compactions[1].Summary)
	require.Equal(t, core.RunOutcomeSuccess, terminalRows[0].RunOutcome, "rejected retries must leave the terminal payload unchanged")
	require.Len(t, snapshot.DurableEntries, 7)
}

func TestSessionRunCommitSinkRejectsConflictingTerminalTranscriptWithoutWrite(t *testing.T) {
	ctx := context.Background()
	prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(ctx, "terminal conflict")
	require.NoError(t, err)
	defer prepared.Close()
	workspaceID := "workspace-v1-adapter"
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	sink, err := NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)
	binding := core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID, OwnerGeneration: "parent-generation"}
	terminal := core.RunTerminal{
		Transcript: []core.TranscriptEntry{
			{Message: core.AgentMessage{Role: "user", Content: "different", InputKind: core.InputKindUser}},
			{Message: core.AgentMessage{Role: "assistant", Content: "done"}},
		},
		Outcome: core.RunOutcomeSuccess, TerminalCheckpointID: "terminal-conflict",
	}
	err = runSessionCommitAgent(t, sink, binding, func(ctx context.Context, actual core.RunCommitSink, _ core.RunCommitBinding, runID string) error {
		terminal.RunID = runID
		if err := actual.CommitInput(ctx, core.RunCommitInput{RunID: runID, TranscriptIndex: 0, Message: core.AgentMessage{Role: "user", Content: "actual", InputKind: core.InputKindUser}, InputKind: core.InputKindUser}); err != nil {
			return err
		}
		return actual.CommitTerminal(ctx, terminal)
	})
	require.Error(t, err)
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	require.Len(t, snapshot.DurableEntries, 1, "terminal conflict must fail before suffix or checkpoint writes")
	require.Equal(t, EntryTypeUser, snapshot.DurableEntries[0].Type)
}

func TestSessionRunCommitSinkTerminalSyncFailureWithholdsAcknowledgment(t *testing.T) {
	ctx := context.Background()
	var failSync atomic.Bool
	preparer := newDurableJSONLSessionPreparer(t.TempDir(), nil, durableJSONLIO{sync: func(file *os.File) error {
		if failSync.Load() {
			return errors.New("injected terminal sync failure")
		}
		return file.Sync()
	}})
	prepared, err := preparer.PrepareNew(ctx, "terminal sync failure")
	require.NoError(t, err)
	defer prepared.Close()
	workspaceID := "workspace-v1-adapter"
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	sink, err := NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)
	binding := core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID, OwnerGeneration: "parent-generation"}
	terminal := core.RunTerminal{
		Transcript: []core.TranscriptEntry{
			{Message: core.AgentMessage{Role: "user", Content: "finish", InputKind: core.InputKindUser}},
			{Message: core.AgentMessage{Role: "assistant", Content: "done"}},
		},
		Outcome: core.RunOutcomeSuccess, TerminalCheckpointID: "terminal-sync-failure",
	}
	failSync.Store(false)
	err = runSessionCommitAgent(t, sink, binding, func(ctx context.Context, actual core.RunCommitSink, _ core.RunCommitBinding, runID string) error {
		terminal.RunID = runID
		if err := actual.CommitInput(ctx, core.RunCommitInput{RunID: runID, TranscriptIndex: 0, Message: terminal.Transcript[0].Message, InputKind: core.InputKindUser}); err != nil {
			return err
		}
		failSync.Store(true)
		return actual.CommitTerminal(ctx, terminal)
	})
	require.Error(t, err, "terminal append/sync failure must not acknowledge the run")
	_, snapshotErr := prepared.Snapshot()
	require.ErrorIs(t, snapshotErr, ErrDurableWriterPoisoned, "a failed terminal sync poisons the handle until reopen")
}

func TestSessionRunCommitSinkCompactionUsesRunStartRevisionAndCanonicalSource(t *testing.T) {
	ctx := context.Background()
	prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(ctx, "compaction source")
	require.NoError(t, err)
	defer prepared.Close()
	workspaceID := "workspace-v1-adapter"
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	sink, err := NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)
	binding := core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID, OwnerGeneration: "parent-generation"}
	projection := core.CompactionProjection{ExpectedHistoryRevision: sink.baseView.Revision, Through: core.CompactionCursor{TranscriptCount: 1}, Summary: "summary of request"}
	var staleErr error
	err = runSessionCommitAgent(t, sink, binding, func(ctx context.Context, actual core.RunCommitSink, _ core.RunCommitBinding, runID string) error {
		if err := actual.CommitInput(ctx, core.RunCommitInput{RunID: runID, TranscriptIndex: 0, Message: core.AgentMessage{Role: "user", Content: "summarize", InputKind: core.InputKindUser}, InputKind: core.InputKindUser}); err != nil {
			return err
		}
		stale := projection
		stale.ExpectedHistoryRevision = "stale-revision"
		staleErr = actual.CommitCompaction(ctx, stale)
		if staleErr == nil {
			return errors.New("stale compaction revision was accepted")
		}
		if err := actual.CommitCompaction(ctx, projection); err != nil {
			return err
		}
		return actual.CommitCompaction(ctx, projection)
	})
	require.NoError(t, err)
	require.Error(t, staleErr)
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	require.Len(t, snapshot.DurableEntries, 2, "stale and identical retry paths must not duplicate compaction rows")
	require.Equal(t, EntryTypeCompaction, snapshot.DurableEntries[1].Type)
	require.Equal(t, snapshot.DurableEntries[0].ID, snapshot.DurableEntries[1].CompactedThroughEntryID)
	require.Equal(t, "summary of request", snapshot.DurableEntries[1].Summary)
	require.Len(t, snapshot.PublishedView.Messages, 1)
	require.Contains(t, snapshot.PublishedView.Messages[0].Content, "summary of request")
}

func TestSessionRunCommitSinkCompactionCannotCutAnOpenToolCallGroup(t *testing.T) {
	ctx := context.Background()
	prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(ctx, "open compaction group")
	require.NoError(t, err)
	defer prepared.Close()
	workspaceID := "workspace-v1-adapter"
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	sink, err := NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)
	binding := core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID, OwnerGeneration: "parent-generation"}
	intent := preparedSessionRunCommitIntent(t)
	var compactionErr error
	err = runSessionCommitAgent(t, sink, binding, func(ctx context.Context, actual core.RunCommitSink, runBinding core.RunCommitBinding, runID string) error {
		intent.RunID, intent.SessionID, intent.OwnerGeneration = runID, runBinding.SessionID, runBinding.OwnerGeneration
		if err := actual.CommitInput(ctx, core.RunCommitInput{RunID: runID, TranscriptIndex: 0, Message: core.AgentMessage{Role: "user", Content: "read", InputKind: core.InputKindUser}, InputKind: core.InputKindUser}); err != nil {
			return err
		}
		if err := actual.BeginIntentBatch(ctx, core.RunIntentBatch{
			RunID: runID, ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: 0,
			AssistantMessage: core.AgentMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: intent.ToolCallID, Name: "read", Args: map[string]any{"path": "README.md"}}}},
			Intents:          []core.ActionIntent{intent},
		}); err != nil {
			return err
		}
		compactionErr = actual.CommitCompaction(ctx, core.CompactionProjection{
			ExpectedHistoryRevision: sink.baseView.Revision,
			Through:                 core.CompactionCursor{TranscriptCount: 2},
			Summary:                 "must wait for receipt closure",
		})
		return nil
	})
	require.NoError(t, err)
	require.Error(t, compactionErr)
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	require.Len(t, snapshot.DurableEntries, 3, "open-group compaction must fail before writing a row")
	for _, row := range snapshot.DurableEntries {
		require.NotEqual(t, EntryTypeCompaction, row.Type)
	}
}

func indexMarkerStateForTest(t *testing.T, index *WorkspaceQuarantineIndex, ref PendingActionRef) DispatchMarkerState {
	t.Helper()
	decisions, err := index.InspectDispatchMarkers(context.Background(), []PendingActionRef{ref})
	require.NoError(t, err)
	require.Len(t, decisions, 1)
	return decisions[0].MarkerState
}
