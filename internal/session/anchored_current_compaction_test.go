package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestAnchoredCurrentRunSessionCommitReloadAndIdempotency(t *testing.T) {
	ctx := context.Background()
	preparer := NewDurableJSONLSessionPreparer(t.TempDir(), nil)
	prepared, err := preparer.PrepareNew(ctx, "anchored synthetic session")
	require.NoError(t, err)
	workspaceID := "workspace-v1-adapter"
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	preparer.authority = index
	sink, err := NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)
	binding := core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID, OwnerGeneration: "fixture-owner"}
	first := preparedSessionRunCommitIntentFor(t, "anchor-call-1", "anchor-action-1", "anchor-batch-1")
	second := preparedSessionRunCommitIntentFor(t, "anchor-call-2", "anchor-action-2", "anchor-batch-2")
	user := core.AgentMessage{Role: "user", Content: "original input", InputKind: core.InputKindUser}
	var firstProjection, secondProjection core.CompactionProjection
	var beforeResolutionErr, wrongDigestErr, conflictingErr error
	var runID string
	err = runSessionCommitAgent(t, sink, binding, func(ctx context.Context, actual core.RunCommitSink, runBinding core.RunCommitBinding, ownedRunID string) error {
		runID = ownedRunID
		if err := actual.CommitInput(ctx, core.RunCommitInput{RunID: runID, TranscriptIndex: 0, Message: user, InputKind: user.InputKind}); err != nil {
			return err
		}
		digest, err := core.RetainedInputDigest(runID, user)
		if err != nil {
			return err
		}
		firstProjection = core.CompactionProjection{ExpectedHistoryRevision: sink.baseView.Revision, Through: core.CompactionCursor{TranscriptCount: 3}, Summary: compaction.DurableTruncationMarker, RetainCurrentInput: true, RetainedInputDigest: digest}
		secondProjection = firstProjection
		secondProjection.Through.TranscriptCount = 5
		if err := anchoredSessionFixtureGroup(ctx, actual, runBinding, runID, &first, 0, 2, false); err != nil {
			return err
		}
		beforeResolutionErr = actual.CommitCompaction(ctx, firstProjection)
		if beforeResolutionErr == nil {
			return errors.New("unresolved action was accepted")
		}
		if err := actual.ResolveExecution(ctx, core.ExecutionResolution{RunID: runID, ActionID: first.ActionID, Evidence: core.ResolutionLocalResourcesStopped}); err != nil {
			return err
		}
		bad := firstProjection
		bad.RetainedInputDigest = strings.Repeat("f", 64)
		wrongDigestErr = actual.CommitCompaction(ctx, bad)
		if wrongDigestErr == nil {
			return errors.New("wrong input digest was accepted")
		}
		if err := actual.CommitCompaction(ctx, firstProjection); err != nil {
			return err
		}
		if err := actual.CommitCompaction(ctx, firstProjection); err != nil {
			return fmt.Errorf("identical retry: %w", err)
		}
		conflicting := firstProjection
		conflicting.RetainCurrentInput, conflicting.RetainedInputDigest = false, ""
		conflictingErr = actual.CommitCompaction(ctx, conflicting)
		if conflictingErr == nil {
			return errors.New("conflicting same-boundary projection was accepted")
		}
		if err := anchoredSessionFixtureGroup(ctx, actual, runBinding, runID, &second, 1, 4, true); err != nil {
			return err
		}
		if err := actual.CommitCompaction(ctx, secondProjection); err != nil {
			return err
		}
		assistant := func(intent core.ActionIntent) core.AgentMessage {
			return core.AgentMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: intent.ToolCallID, Name: "read", Args: map[string]any{"path": "README.md"}}}}
		}
		receipt := func(intent core.ActionIntent) core.AgentMessage {
			return core.AgentMessage{Role: "tool", ToolCallID: intent.ToolCallID, ToolName: "read", Content: "ok", ReceiptOutcome: core.ReceiptOutcomeSuccess}
		}
		terminal := core.RunTerminal{RunID: runID, Transcript: []core.TranscriptEntry{
			{Message: user}, {Message: assistant(first)}, {Message: receipt(first)}, {Message: assistant(second)}, {Message: receipt(second)}, {Message: core.AgentMessage{Role: "assistant", Content: "done"}},
		}, Projections: []core.CompactionProjection{firstProjection, secondProjection}, Outcome: core.RunOutcomeSuccess, TerminalCheckpointID: core.TerminalCheckpointID("anchor-terminal-" + runID)}
		if err := actual.CommitTerminal(ctx, terminal); err != nil {
			return err
		}
		return actual.CommitTerminal(ctx, terminal)
	})
	require.NoError(t, err)
	require.Error(t, beforeResolutionErr)
	require.Error(t, wrongDigestErr)
	require.Error(t, conflictingErr)
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	var input SessionEntry
	var markers []SessionEntry
	for _, row := range snapshot.DurableEntries {
		if row.RunID == runID && row.Type == EntryTypeUser && row.TranscriptIndex != nil && *row.TranscriptIndex == 0 {
			input = row
		}
		if row.RunID == runID && row.Type == EntryTypeCompaction {
			markers = append(markers, row)
		}
	}
	require.NotEmpty(t, input.ID)
	require.Len(t, markers, 2)
	for _, marker := range markers {
		require.Equal(t, input.ID, marker.RetainedInputEntryID)
		require.Equal(t, input.PayloadDigest, marker.RetainedInputPayloadDigest)
		require.Equal(t, firstProjection.RetainedInputDigest, marker.RetainedInputDigest)
	}
	require.NotEqual(t, markers[0].CompactedThroughEntryID, markers[1].CompactedThroughEntryID)
	for _, tc := range []struct {
		name   string
		mutate func([]SessionEntry)
	}{
		{name: "wrong_anchor", mutate: func(rows []SessionEntry) {
			for i := range rows {
				if rows[i].ID == markers[0].ID {
					rows[i].RetainedInputEntryID = markers[0].CompactedThroughEntryID
				}
			}
		}},
		{name: "wrong_digest", mutate: func(rows []SessionEntry) {
			for i := range rows {
				if rows[i].ID == markers[0].ID {
					rows[i].RetainedInputDigest = strings.Repeat("a", 64)
				}
			}
		}},
		{name: "changed_input", mutate: func(rows []SessionEntry) {
			for i := range rows {
				if rows[i].ID == input.ID {
					rows[i].Content += " changed"
				}
			}
		}},
		{name: "missing_resolution", mutate: func(rows []SessionEntry) {
			for i := range rows {
				if rows[i].ActionID == first.ActionID && rows[i].CheckpointKind == CheckpointExecutionResolution {
					rows[i].CheckpointKind = "fixture-missing"
				}
			}
		}},
		{name: "unknown_receipt", mutate: func(rows []SessionEntry) {
			for i := range rows {
				if rows[i].ActionID == first.ActionID && rows[i].Type == EntryTypeTool && rows[i].ActionState == "" {
					rows[i].ReceiptOutcome = core.ReceiptOutcomeUnknown
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := cloneSessionEntries(snapshot.DurableEntries)
			tc.mutate(rows)
			_, err := SessionViewFromEntries(binding.SessionID, "main", "fixture-revision", rows)
			require.Error(t, err)
		})
	}
	require.NoError(t, prepared.Close())
	resumed, err := preparer.PrepareResume(ctx, binding.SessionID)
	require.NoError(t, err)
	defer resumed.Close()
	view := resumed.View()
	require.Len(t, view.Messages, 3)
	require.Equal(t, user.Content, view.Messages[0].Content)
	require.Equal(t, input.ID, view.SourceEntryIDs[0])
	require.True(t, view.Messages[1].Runtime.CompactionMarker)
	require.Equal(t, compaction.UntrustedSummaryContext(compaction.DurableTruncationMarker), view.Messages[1].Content)
	require.Equal(t, "done", view.Messages[2].Content)
}

func anchoredSessionFixtureGroup(ctx context.Context, sink core.RunCommitSink, binding core.RunCommitBinding, runID string, intent *core.ActionIntent, ordinal, receiptIndex uint64, resolve bool) error {
	intent.RunID, intent.SessionID, intent.OwnerGeneration = runID, binding.SessionID, binding.OwnerGeneration
	intent.AssistantTranscriptOrdinal = ordinal
	call := llm.ToolCall{ID: intent.ToolCallID, Name: "read", Args: map[string]any{"path": "README.md"}}
	if err := sink.BeginIntentBatch(ctx, core.RunIntentBatch{RunID: runID, ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: ordinal, AssistantMessage: core.AgentMessage{Role: "assistant", ToolCalls: []llm.ToolCall{call}}, Intents: []core.ActionIntent{*intent}}); err != nil {
		return err
	}
	if err := sink.RecordExecution(ctx, core.ActionExecution{RunID: runID, AssistantTranscriptOrdinal: ordinal, ToolCallID: intent.ToolCallID, ActionID: intent.ActionID, OwnerGeneration: intent.OwnerGeneration, DispatchState: core.DispatchStateDispatched, ResourceState: core.RunExecutionQuiescent, CompletionProof: tools.CompletionInlinePipelineJoined}); err != nil {
		return err
	}
	message := core.AgentMessage{Role: "tool", ToolCallID: intent.ToolCallID, ToolName: "read", Content: "ok", ReceiptOutcome: core.ReceiptOutcomeSuccess}
	if err := sink.RecordReceipt(ctx, core.RunToolReceipt{RunID: runID, AssistantTranscriptOrdinal: ordinal, ToolCallID: intent.ToolCallID, ActionID: intent.ActionID, TranscriptIndex: receiptIndex, DispatchState: core.DispatchStateDispatched, ReceiptOutcome: core.ReceiptOutcomeSuccess, Message: message}); err != nil {
		return err
	}
	if resolve {
		return sink.ResolveExecution(ctx, core.ExecutionResolution{RunID: runID, ActionID: intent.ActionID, Evidence: core.ResolutionLocalResourcesStopped})
	}
	return nil
}

func TestAnchoredCurrentRunSessionSyncFailurePoisonsWriter(t *testing.T) {
	ctx := context.Background()
	var failSync atomic.Bool
	preparer := newDurableJSONLSessionPreparer(t.TempDir(), nil, durableJSONLIO{sync: func(file *os.File) error {
		if failSync.Load() {
			return errors.New("injected projection sync failure")
		}
		return file.Sync()
	}})
	prepared, err := preparer.PrepareNew(ctx, "anchored sync failure")
	require.NoError(t, err)
	defer prepared.Close()
	workspaceID := "workspace-v1-adapter"
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	sink, err := NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)
	binding := core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID, OwnerGeneration: "fixture-owner"}
	intent := preparedSessionRunCommitIntent(t)
	user := core.AgentMessage{Role: "user", Content: "original input", InputKind: core.InputKindUser}
	err = runSessionCommitAgent(t, sink, binding, func(ctx context.Context, actual core.RunCommitSink, runBinding core.RunCommitBinding, runID string) error {
		if err := actual.CommitInput(ctx, core.RunCommitInput{RunID: runID, TranscriptIndex: 0, Message: user, InputKind: user.InputKind}); err != nil {
			return err
		}
		if err := anchoredSessionFixtureGroup(ctx, actual, runBinding, runID, &intent, 0, 2, true); err != nil {
			return err
		}
		digest, err := core.RetainedInputDigest(runID, user)
		if err != nil {
			return err
		}
		failSync.Store(true)
		return actual.CommitCompaction(ctx, core.CompactionProjection{ExpectedHistoryRevision: sink.baseView.Revision, Through: core.CompactionCursor{TranscriptCount: 3}, Summary: compaction.DurableTruncationMarker, RetainCurrentInput: true, RetainedInputDigest: digest})
	})
	require.Error(t, err)
	_, err = prepared.Snapshot()
	require.ErrorIs(t, err, ErrDurableWriterPoisoned)
}

func TestAnchoredCurrentRunPreservesLegacyIDAndRejectsOldDecoder(t *testing.T) {
	sink := &SessionRunCommitSink{workspaceID: "workspace", baseView: SessionView{Revision: "rev", BranchID: "main", Messages: []core.AgentMessage{{Role: "user"}}, SourceEntryIDs: []string{"entry"}}, branchRows: []SessionEntry{{ID: "entry"}}}
	row, err := sink.compactionEntryLocked("run", core.CompactionProjection{ExpectedHistoryRevision: "rev", Through: core.CompactionCursor{PriorHistoryCount: 1}, Summary: "summary"})
	require.NoError(t, err)
	require.Equal(t, "runcommit_683e3c112d63640175492040c659a3d1544b4d4351f6dde41e5b8cb670d3744a", row.ID)
	legacyJSON, err := json.Marshal(row)
	require.NoError(t, err)
	require.NotContains(t, string(legacyJSON), "retained_input_")

	row.RetainedInputEntryID = "input"
	row.RetainedInputPayloadDigest = "sha256:" + strings.Repeat("a", 64)
	row.RetainedInputDigest = strings.Repeat("b", 64)
	_, newDigest, err := canonicalEntryPayload(row)
	require.NoError(t, err)
	row.RetainedInputEntryID, row.RetainedInputPayloadDigest, row.RetainedInputDigest = "", "", ""
	_, oldDecoderDigest, err := canonicalEntryPayload(row)
	require.NoError(t, err)
	require.NotEqual(t, newDigest, oldDecoderDigest, "an old decoder that omits anchor fields must fail its payload check")
}
