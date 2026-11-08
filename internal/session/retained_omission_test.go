package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/png"
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

func sessionOmissionGroup(ctx context.Context, t *testing.T, sink core.RunCommitSink, binding core.RunCommitBinding, runID string, ordinal, receiptIndex uint64, imageURL string, priorSourceCount ...uint64) ([]core.TranscriptEntry, llm.RetainedSourceRange, error) {
	t.Helper()
	callID := fmt.Sprintf("call-%d", ordinal)
	intent := preparedSessionRunCommitIntentFor(t, callID, fmt.Sprintf("%s-action-%d", runID, ordinal), fmt.Sprintf("%s-batch-%d", runID, ordinal))
	intent.RunID, intent.SessionID, intent.OwnerGeneration, intent.AssistantTranscriptOrdinal = runID, binding.SessionID, binding.OwnerGeneration, ordinal
	call := llm.ToolCall{ID: callID, Name: "read", Args: map[string]any{"path": "README.md"}}
	assistant := core.AgentMessage{Role: "assistant", ToolCalls: []llm.ToolCall{call}}
	receipt := core.AgentMessage{Role: "tool", ToolCallID: callID, ToolName: "read", Content: strings.Repeat("old text ", 500), ReceiptOutcome: core.ReceiptOutcomeSuccess}
	if imageURL != "" {
		receipt.Content = "image receipt"
		receipt.ContentBlocks = []llm.ContentBlock{{Type: "image_url", ImageURL: &llm.ImageURL{URL: imageURL, Detail: "high"}}}
	}
	if err := sink.BeginIntentBatch(ctx, core.RunIntentBatch{RunID: runID, ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: ordinal, AssistantMessage: assistant, Intents: []core.ActionIntent{intent}}); err != nil {
		return nil, llm.RetainedSourceRange{}, err
	}
	if err := sink.RecordExecution(ctx, core.ActionExecution{RunID: runID, AssistantTranscriptOrdinal: ordinal, ToolCallID: callID, ActionID: intent.ActionID, OwnerGeneration: intent.OwnerGeneration, DispatchState: core.DispatchStateDispatched, ResourceState: core.RunExecutionQuiescent, CompletionProof: tools.CompletionInlinePipelineJoined}); err != nil {
		return nil, llm.RetainedSourceRange{}, err
	}
	if err := sink.RecordReceipt(ctx, core.RunToolReceipt{RunID: runID, AssistantTranscriptOrdinal: ordinal, ToolCallID: callID, ActionID: intent.ActionID, TranscriptIndex: receiptIndex, DispatchState: core.DispatchStateDispatched, ReceiptOutcome: core.ReceiptOutcomeSuccess, Message: receipt}); err != nil {
		return nil, llm.RetainedSourceRange{}, err
	}
	if err := sink.ResolveExecution(ctx, core.ExecutionResolution{RunID: runID, ActionID: intent.ActionID, Evidence: core.ResolutionLocalResourcesStopped}); err != nil {
		return nil, llm.RetainedSourceRange{}, err
	}
	var priorCount uint64
	if len(priorSourceCount) > 0 {
		priorCount = priorSourceCount[0]
	}
	start := llm.SourceRef{Kind: llm.SourceRunTranscript, RunID: runID, PriorSourceCount: priorCount, TranscriptIndex: receiptIndex - 1}
	end := llm.SourceRef{Kind: llm.SourceRunTranscript, RunID: runID, PriorSourceCount: priorCount, TranscriptIndex: receiptIndex}
	items := []compaction.TurnItem{
		{Role: "assistant", ToolCalls: []llm.ToolCall{call}, Runtime: llm.RuntimeMetadata{Source: start}},
		{Role: "tool", ToolResult: receipt.Content, ToolCallID: callID, ToolName: "read", ContentBlocks: receipt.ContentBlocks, ReceiptOutcome: "success", Runtime: llm.RuntimeMetadata{Source: end, ReceiptOutcome: "success"}},
	}
	digest, err := compaction.RetainedExchangeDigest(items)
	return []core.TranscriptEntry{{Message: assistant}, {Message: receipt}}, llm.RetainedSourceRange{Start: start, End: end, Digest: digest}, err
}

func TestRetainedImageOmissionCommitResumeAndSecondCompaction(t *testing.T) {
	ctx := context.Background()
	preparer := NewDurableJSONLSessionPreparer(t.TempDir(), nil)
	prepared, err := preparer.PrepareNew(ctx, "synthetic images")
	require.NoError(t, err)
	workspaceID := "workspace-v1-adapter"
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	preparer.authority = index
	sink, err := NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)
	binding := core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID, OwnerGeneration: "fixture-owner"}
	var pngBytes bytes.Buffer
	require.NoError(t, png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	imageURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes.Bytes())
	user := core.AgentMessage{Role: "user", Content: "original input", InputKind: core.InputKindUser}
	var runID string
	err = runSessionCommitAgent(t, sink, binding, func(ctx context.Context, actual core.RunCommitSink, runBinding core.RunCommitBinding, ownedRunID string) error {
		runID = ownedRunID
		if err := actual.CommitInput(ctx, core.RunCommitInput{RunID: runID, TranscriptIndex: 0, Message: user, InputKind: user.InputKind}); err != nil {
			return err
		}
		transcript := []core.TranscriptEntry{{Message: user}}
		var retained []llm.RetainedSourceRange
		for ordinal := uint64(0); ordinal < 4; ordinal++ {
			url := ""
			if ordinal == 0 || ordinal == 3 {
				url = imageURL
			}
			group, r, err := sessionOmissionGroup(ctx, t, actual, runBinding, runID, ordinal, 2+ordinal*2, url)
			if err != nil {
				return err
			}
			transcript = append(transcript, group...)
			if url != "" {
				retained = append(retained, r)
			}
		}
		digest, err := core.RetainedInputDigest(runID, user)
		if err != nil {
			return err
		}
		first := core.CompactionProjection{ExpectedHistoryRevision: sink.baseView.Revision, Through: core.CompactionCursor{TranscriptCount: 9}, Summary: compaction.DurableTruncationMarker, RetainCurrentInput: true, RetainedInputDigest: digest, RetainedSourceRanges: append([]llm.RetainedSourceRange(nil), retained...)}
		for _, mutate := range []func(*core.CompactionProjection){
			func(p *core.CompactionProjection) { p.RetainedSourceRanges[0].Digest = strings.Repeat("f", 64) },
			func(p *core.CompactionProjection) {
				p.RetainedSourceRanges = append(p.RetainedSourceRanges, p.RetainedSourceRanges[0])
			},
			func(p *core.CompactionProjection) {
				p.RetainedSourceRanges[0], p.RetainedSourceRanges[1] = p.RetainedSourceRanges[1], p.RetainedSourceRanges[0]
			},
			func(p *core.CompactionProjection) { p.RetainedSourceRanges[0].End = p.RetainedSourceRanges[0].Start },
			func(p *core.CompactionProjection) { p.RetainedSourceRanges[0].Start.RunID = "foreign" },
			func(p *core.CompactionProjection) { p.RetainedSourceRanges = p.RetainedSourceRanges[1:] },
		} {
			bad := first
			bad.RetainedSourceRanges = append([]llm.RetainedSourceRange(nil), first.RetainedSourceRanges...)
			mutate(&bad)
			if err := actual.CommitCompaction(ctx, bad); err == nil {
				return fmt.Errorf("invalid retained projection was accepted")
			}
		}
		if err := actual.CommitCompaction(ctx, first); err != nil {
			return fmt.Errorf("first retained projection: %w", err)
		}
		if err := actual.CommitCompaction(ctx, first); err != nil {
			return fmt.Errorf("identical retry: %w", err)
		}
		for ordinal := uint64(4); ordinal < 6; ordinal++ {
			url := ""
			if ordinal == 5 {
				url = imageURL
			}
			group, r, err := sessionOmissionGroup(ctx, t, actual, runBinding, runID, ordinal, 2+ordinal*2, url)
			if err != nil {
				return err
			}
			transcript = append(transcript, group...)
			if url != "" {
				retained = append(retained, r)
			}
		}
		second := first
		second.Through.TranscriptCount = 13
		second.RetainedSourceRanges = append([]llm.RetainedSourceRange(nil), retained...)
		if err := actual.CommitCompaction(ctx, second); err != nil {
			return fmt.Errorf("second retained projection: %w", err)
		}
		transcript = append(transcript, core.TranscriptEntry{Message: core.AgentMessage{Role: "assistant", Content: "done"}})
		return actual.CommitTerminal(ctx, core.RunTerminal{RunID: runID, Transcript: transcript, Projections: []core.CompactionProjection{first, second}, Outcome: core.RunOutcomeSuccess, TerminalCheckpointID: core.TerminalCheckpointID("synthetic-terminal-" + runID)})
	})
	require.NoError(t, err)
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	var markers []SessionEntry
	for _, row := range snapshot.DurableEntries {
		if row.RunID == runID && row.Type == EntryTypeCompaction {
			markers = append(markers, row)
		}
	}
	require.Len(t, markers, 2)
	require.Len(t, markers[1].RetainedExchanges, 3)
	require.NoError(t, prepared.Close())
	resumed, err := preparer.PrepareResume(ctx, binding.SessionID)
	require.NoError(t, err)
	defer resumed.Close()
	view := resumed.View()
	require.Len(t, view.Messages, 9)
	require.Equal(t, "original input", view.Messages[0].Content)
	require.True(t, view.Messages[1].Runtime.CompactionMarker)
	for _, i := range []int{3, 5, 7} {
		require.Equal(t, imageURL, view.Messages[i].ContentBlocks[0].ImageURL.URL)
	}
	require.Equal(t, "done", view.Messages[8].Content)
	resumedSnapshot, err := resumed.Snapshot()
	require.NoError(t, err)
	leaf := resumedSnapshot.ActiveLeafEntryID
	_, err = resumed.DurableWriter().AppendAndSync(ctx, SessionEntry{
		ID: "synthetic-image-fork", ParentID: leaf, Type: EntryTypeCheckpoint,
		BranchID: "image-fork", ParentBranchID: "main", BaseLeafEntryID: leaf,
		ActiveLeafEntryID: "synthetic-image-fork", CheckpointKind: CheckpointBranch,
	})
	require.NoError(t, err)
	fork := resumed.View()
	require.Equal(t, "image-fork", fork.BranchID)
	require.Equal(t, view.Messages, fork.Messages)
	require.Equal(t, view.SourceEntryIDs, fork.SourceEntryIDs)

	// A subsequent run sees the earlier image exchanges as captured prior
	// history, including on a fork. Their original journal rows remain the
	// authoritative source for the new projection.
	secondSink, err := NewSessionRunCommitSink(resumed, index, workspaceID, nil)
	require.NoError(t, err)
	var priorRanges []llm.RetainedSourceRange
	for _, startIndex := range []int{2, 4, 6} {
		var members []compaction.TurnItem
		for i := startIndex; i <= startIndex+1; i++ {
			message := fork.Messages[i]
			source := llm.SourceRef{Kind: llm.SourcePriorPublished, HistoryRevision: fork.Revision, BranchID: fork.BranchID, SourceEntryID: fork.SourceEntryIDs[i], PriorOrdinal: uint64(i)}
			item := compaction.TurnItem{Role: message.Role, Content: message.Content, ContentBlocks: message.ContentBlocks, Usage: message.Usage, ToolCalls: message.ToolCalls, ToolCallID: message.ToolCallID, ToolName: message.ToolName, ReceiptOutcome: message.ReceiptOutcome, Runtime: llm.RuntimeMetadata{Source: source, ReceiptOutcome: message.ReceiptOutcome}}
			if item.Role == compaction.RoleTool {
				item.ToolResult, item.Content = item.Content, ""
			}
			members = append(members, item)
		}
		digest, err := compaction.RetainedExchangeDigest(members)
		require.NoError(t, err)
		priorRanges = append(priorRanges, llm.RetainedSourceRange{Start: members[0].Runtime.Source, End: members[1].Runtime.Source, Digest: digest})
	}
	err = runSessionCommitAgent(t, secondSink, binding, func(ctx context.Context, actual core.RunCommitSink, runBinding core.RunCommitBinding, nextRunID string) error {
		if err := actual.CommitInput(ctx, core.RunCommitInput{RunID: nextRunID, TranscriptIndex: 0, Message: user, InputKind: user.InputKind}); err != nil {
			return err
		}
		group, _, err := sessionOmissionGroup(ctx, t, actual, runBinding, nextRunID, 0, 2, "", uint64(len(fork.Messages)))
		if err != nil {
			return err
		}
		imageGroup, currentRange, err := sessionOmissionGroup(ctx, t, actual, runBinding, nextRunID, 1, 4, imageURL, uint64(len(fork.Messages)))
		if err != nil {
			return err
		}
		digest, err := core.RetainedInputDigest(nextRunID, user)
		if err != nil {
			return err
		}
		projection := core.CompactionProjection{ExpectedHistoryRevision: fork.Revision, Through: core.CompactionCursor{PriorHistoryCount: uint64(len(fork.Messages)), TranscriptCount: 5}, Summary: compaction.DurableTruncationMarker, RetainCurrentInput: true, RetainedInputDigest: digest, RetainedSourceRanges: append(append([]llm.RetainedSourceRange(nil), priorRanges...), currentRange)}
		if err := actual.CommitCompaction(ctx, projection); err != nil {
			return err
		}
		transcript := append([]core.TranscriptEntry{{Message: user}}, group...)
		transcript = append(transcript, imageGroup...)
		transcript = append(transcript, core.TranscriptEntry{Message: core.AgentMessage{Role: "assistant", Content: "second run done"}})
		return actual.CommitTerminal(ctx, core.RunTerminal{RunID: nextRunID, Transcript: transcript, Projections: []core.CompactionProjection{projection}, Outcome: core.RunOutcomeSuccess, TerminalCheckpointID: core.TerminalCheckpointID("next-terminal-" + nextRunID)})
	})
	require.NoError(t, err)
	nextView := resumed.View()
	require.Len(t, nextView.Messages, 11)
	for _, i := range []int{3, 5, 7, 9} {
		require.Equal(t, imageURL, nextView.Messages[i].ContentBlocks[0].ImageURL.URL)
	}
	for _, mutation := range []func([]SessionEntry){
		func(rows []SessionEntry) {
			for i := range rows {
				if rows[i].ID == markers[1].ID {
					rows[i].RetainedExchanges[0].Entries[0].PayloadDigest = "sha256:" + strings.Repeat("f", 64)
				}
			}
		},
		func(rows []SessionEntry) {
			for i := range rows {
				if rows[i].ID == markers[1].ID {
					rows[i].RetainedExchanges[0], rows[i].RetainedExchanges[1] = rows[i].RetainedExchanges[1], rows[i].RetainedExchanges[0]
				}
			}
		},
	} {
		rows := cloneSessionEntries(snapshot.DurableEntries)
		mutation(rows)
		_, err := SessionViewFromEntries(binding.SessionID, "main", "revision", rows)
		require.Error(t, err)
	}
}

func TestRetainedImageOmissionSyncFailureDoesNotPublish(t *testing.T) {
	ctx := context.Background()
	var failSync atomic.Bool
	preparer := newDurableJSONLSessionPreparer(t.TempDir(), nil, durableJSONLIO{sync: func(file *os.File) error {
		if failSync.Load() {
			return errors.New("synthetic sync failure")
		}
		return file.Sync()
	}})
	prepared, err := preparer.PrepareNew(ctx, "synthetic sync failure")
	require.NoError(t, err)
	defer prepared.Close()
	workspaceID := "workspace-v1-adapter"
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	sink, err := NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)
	binding := core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID, OwnerGeneration: "fixture-owner"}
	user := core.AgentMessage{Role: "user", Content: "original input", InputKind: core.InputKindUser}
	var pngBytes bytes.Buffer
	require.NoError(t, png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	imageURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes.Bytes())
	err = runSessionCommitAgent(t, sink, binding, func(ctx context.Context, actual core.RunCommitSink, runBinding core.RunCommitBinding, runID string) error {
		if err := actual.CommitInput(ctx, core.RunCommitInput{RunID: runID, TranscriptIndex: 0, Message: user, InputKind: user.InputKind}); err != nil {
			return err
		}
		if _, _, err := sessionOmissionGroup(ctx, t, actual, runBinding, runID, 0, 2, ""); err != nil {
			return err
		}
		_, retained, err := sessionOmissionGroup(ctx, t, actual, runBinding, runID, 1, 4, imageURL)
		if err != nil {
			return err
		}
		digest, err := core.RetainedInputDigest(runID, user)
		if err != nil {
			return err
		}
		failSync.Store(true)
		return actual.CommitCompaction(ctx, core.CompactionProjection{ExpectedHistoryRevision: sink.baseView.Revision, Through: core.CompactionCursor{TranscriptCount: 5}, Summary: compaction.DurableTruncationMarker, RetainCurrentInput: true, RetainedInputDigest: digest, RetainedSourceRanges: []llm.RetainedSourceRange{retained}})
	})
	require.ErrorContains(t, err, "synthetic sync failure")
	_, err = prepared.Snapshot()
	require.ErrorIs(t, err, ErrDurableWriterPoisoned)
}
