package session

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
)

type rejectedBatchWriter struct {
	base       DurableSessionWriter
	fail       string
	mirrorFail *atomic.Bool
}

func (w rejectedBatchWriter) AppendAndSync(ctx context.Context, rows ...SessionEntry) ([]SessionEntry, error) {
	for _, row := range rows {
		if (w.fail == "receipt" && row.Type == EntryTypeTool && row.ActionState == "") || (w.fail == "resolution" && row.CheckpointKind == CheckpointExecutionResolution) {
			return nil, errors.New("injected denied batch sync failure")
		}
	}
	canonical, err := w.base.AppendAndSync(ctx, rows...)
	if err == nil && w.mirrorFail != nil {
		for _, row := range rows {
			if row.CheckpointKind == CheckpointExecutionResolution {
				w.mirrorFail.Store(true)
			}
		}
	}
	return canonical, err
}

type rejectedBatchPrepared struct {
	PreparedSession
	writer DurableSessionWriter
}

func (p rejectedBatchPrepared) DurableWriter() DurableSessionWriter { return p.writer }

func TestRejectedBatchCacheBindsOwnerFullBatchAndOneShot(t *testing.T) {
	for _, failure := range []string{"", "receipt", "resolution", "mirror"} {
		t.Run("failure="+failure, func(t *testing.T) {
			workspace := "workspace-v1-adapter"
			index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
			require.NoError(t, index.EnsureWorkspace(t.Context(), workspace))
			old := workspaceTestBatch(workspace, "old-session", "old-run", "old-batch", "old-action", 0, "old-call")
			_, err := index.BeginDispatchBatch(t.Context(), old)
			require.NoError(t, err)
			prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), index).PrepareNew(t.Context(), "denial cache")
			require.NoError(t, err)
			defer func() { require.NoError(t, prepared.Close()) }()
			var bound PreparedSession = prepared
			var mirrorFail atomic.Bool
			var mirrorHit atomic.Bool
			if failure == "mirror" {
				index.io.dirSync = func(path string) error {
					if mirrorFail.Load() {
						mirrorHit.Store(true)
						return errors.New("injected denied resolution mirror failure")
					}
					return syncDirectory(path)
				}
			}
			if failure != "" {
				bound = rejectedBatchPrepared{PreparedSession: prepared, writer: rejectedBatchWriter{base: prepared.DurableWriter(), fail: failure, mirrorFail: func() *atomic.Bool {
					if failure == "mirror" {
						return &mirrorFail
					}
					return nil
				}()}}
			}
			sink, err := NewSessionRunCommitSink(bound, index, workspace, nil)
			require.NoError(t, err)
			binding := core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspace, OwnerGeneration: "cache-owner"}
			intent := preparedSessionRunCommitIntent(t)
			err = runSessionCommitAgent(t, sink, binding, func(ctx context.Context, actual core.RunCommitSink, binding core.RunCommitBinding, runID string) error {
				assert.NoError(t, actual.CommitInput(ctx, core.RunCommitInput{RunID: runID, TranscriptIndex: 0, InputKind: core.InputKindUser, Message: core.AgentMessage{Role: "user", Content: "attempt"}}))
				intent.RunID, intent.SessionID, intent.OwnerGeneration = runID, binding.SessionID, binding.OwnerGeneration
				batch := core.RunIntentBatch{RunID: runID, ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: 0, AssistantMessage: core.AgentMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: intent.ToolCallID, Name: "read", Args: map[string]any{"path": "README.md"}}}}, Intents: []core.ActionIntent{intent}}
				assert.ErrorIs(t, actual.BeginIntentBatch(ctx, batch), ErrWorkspaceQuarantined)
				if failure != "" {
					if failure == "mirror" {
						assert.True(t, mirrorHit.Load(), "mirror failure injection must actually execute")
					}
					_, closed, err := sink.TakeRejectedIntentBatch(ctx, batch)
					assert.NoError(t, err)
					assert.False(t, closed, "partial receipt/resolution sync cannot publish closed facts")
					return nil
				}
				for _, change := range []func(*core.RunIntentBatch){func(b *core.RunIntentBatch) { b.ActionBatchID = "borrowed-batch" }, func(b *core.RunIntentBatch) { b.CanonicalWorkspaceRoot = "different-root" }, func(b *core.RunIntentBatch) { b.Intents[0].OwnerGeneration = "foreign-owner" }, func(b *core.RunIntentBatch) { setRejectedBatchPath(t, b, "changed") }} {
					bad := cloneRejectedBatch(batch)
					change(&bad)
					_, closed, err := sink.TakeRejectedIntentBatch(ctx, bad)
					assert.Error(t, err)
					assert.False(t, closed)
				}
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				_, closed, err := sink.TakeRejectedIntentBatch(canceled, batch)
				assert.ErrorIs(t, err, context.Canceled)
				assert.False(t, closed)
				foreign, err := NewSessionRunCommitSink(prepared, index, workspace, nil)
				assert.NoError(t, err)
				_, closed, err = sink.TakeRejectedIntentBatch(core.WithRunCommit(ctx, foreign, binding), batch)
				assert.Error(t, err)
				assert.False(t, closed)
				assert.Error(t, sink.BeginIntentBatch(ctx, batch), "repeated Begin must not mint or overwrite the cached fact")
				setRejectedBatchPath(t, &batch, "mutated after capture")
				_, closed, err = sink.TakeRejectedIntentBatch(ctx, batch)
				assert.Error(t, err)
				assert.False(t, closed)
				setRejectedBatchPath(t, &batch, "README.md")
				facts, closed, err := sink.TakeRejectedIntentBatch(ctx, batch)
				assert.NoError(t, err)
				assert.True(t, closed)
				assert.Len(t, facts, 1)
				if len(facts) != 1 {
					return errors.New("denied cache did not return exactly one receipt")
				}
				assert.Equal(t, intent.ActionID, facts[0].ActionID)
				facts[0].Message.Content = "caller mutation"
				assert.Equal(t, core.RejectedBatchReservedOutput, sink.rejectedBatch.receipts[0].Message.Content)
				_, closed, err = sink.TakeRejectedIntentBatch(ctx, batch)
				assert.NoError(t, err)
				assert.False(t, closed)
				assert.Error(t, sink.BeginIntentBatch(ctx, batch))
				return nil
			})
			require.NoError(t, err)
		})
	}
}

func setRejectedBatchPath(t *testing.T, batch *core.RunIntentBatch, path string) {
	t.Helper()
	args, ok := batch.AssistantMessage.ToolCalls[0].Args.(map[string]any)
	if !ok {
		t.Errorf("rejected test batch args has type %T", batch.AssistantMessage.ToolCalls[0].Args)
		return
	}
	args["path"] = path
}
