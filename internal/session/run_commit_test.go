package session

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/stretchr/testify/require"
)

var errTestRunCommitContextMissing = errors.New("test: run-commit context missing")

type sessionCommitTestAgent struct {
	commit func(context.Context, core.RunCommitSink, core.RunCommitBinding, string) error
}

func (*sessionCommitTestAgent) Name() string { return "session-commit-test-agent" }

func (a *sessionCommitTestAgent) Run(ctx context.Context, _ core.Submission, _ ...core.EventStream) (core.Result, error) {
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
	t.Helper()
	harness := core.NewHarnessImpl(&sessionCommitTestAgent{commit: commit})
	ctx := core.WithRunCommit(context.Background(), sink, binding)
	stream, err := harness.Submit(ctx, "test input")
	require.NoError(t, err)
	for range stream.Events() {
	}
	_, err = stream.Result()
	return err
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

	err = runSessionCommitAgent(t, sink, binding, func(ctx context.Context, actual core.RunCommitSink, _ core.RunCommitBinding, runID string) error {
		if actual != sink {
			return errors.New("callback received a different run-commit sink")
		}
		return actual.CommitInput(ctx, core.RunCommitInput{
			RunID: runID, TranscriptIndex: 0,
			Message:   core.AgentMessage{Role: "user", Content: "same text is still a distinct input"},
			InputKind: core.InputKindUser,
		})
	})
	require.NoError(t, err)

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
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
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
		resolution := core.ExecutionResolution{
			RunID: ownedRunID, ActionID: intent.ActionID, Evidence: core.ResolutionLocalResourcesStopped,
		}
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

func indexMarkerStateForTest(t *testing.T, index *WorkspaceQuarantineIndex, ref PendingActionRef) DispatchMarkerState {
	t.Helper()
	decisions, err := index.InspectDispatchMarkers(context.Background(), []PendingActionRef{ref})
	require.NoError(t, err)
	require.Len(t, decisions, 1)
	return decisions[0].MarkerState
}
