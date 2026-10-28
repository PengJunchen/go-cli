package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
)

func seedHistoryAdmin(t *testing.T) (PreparedSession, string) {
	t.Helper()
	dir := t.TempDir()
	prepared, err := NewDurableJSONLSessionPreparer(dir, nil).PrepareNew(t.Context(), "history")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Close()) })
	zero, one := uint64(0), uint64(1)
	_, err = prepared.DurableWriter().AppendAndSync(t.Context(),
		SessionEntry{ID: "root", Type: EntryTypeSystem, Content: "root context"},
		SessionEntry{ID: "user", ParentID: "root", Type: EntryTypeUser, Content: "question", RunID: "run", TranscriptIndex: &zero},
		SessionEntry{ID: "answer", ParentID: "user", Type: EntryTypeAssistant, Content: "answer", RunID: "run", TranscriptIndex: &one},
		SessionEntry{ID: "terminal", ParentID: "answer", Type: EntryTypeCheckpoint, CheckpointKind: CheckpointRunTerminal, RunID: "run", RunOutcome: core.RunOutcomeSuccess})
	require.NoError(t, err)
	return prepared, dir
}

func TestDurableHistoryAdminCASBranchAndRestart(t *testing.T) {
	prepared, dir := seedHistoryAdmin(t)
	before, err := prepared.Snapshot()
	require.NoError(t, err)
	editor, ok := prepared.(HistoryEditor)
	require.True(t, ok)
	fork, err := editor.EditHistory(t.Context(), HistoryEdit{Expected: HistoryCoordinatesFromSnapshot(before), Kind: HistoryFork, TargetEntryID: "answer", NewBranchID: "feature"})
	require.NoError(t, err)
	require.Equal(t, "feature", fork.ActiveBranchID)
	require.Equal(t, []string{"root context", "question", "answer"}, historyContents(fork.PublishedView))
	_, err = editor.EditHistory(t.Context(), HistoryEdit{Expected: HistoryCoordinatesFromSnapshot(before), Kind: HistorySelect, TargetEntryID: "root", TargetBranchID: "main"})
	require.ErrorIs(t, err, ErrHistoryChanged)
	require.NoError(t, prepared.Close())
	resumed, err := NewDurableJSONLSessionPreparer(dir, nil).PrepareResume(t.Context(), before.SessionID)
	require.NoError(t, err)
	defer resumed.Close()
	after, err := resumed.Snapshot()
	require.NoError(t, err)
	require.Equal(t, "feature", after.ActiveBranchID)
	editor = resumed.(HistoryEditor)
	selected, err := editor.EditHistory(t.Context(), HistoryEdit{Expected: HistoryCoordinatesFromSnapshot(after), Kind: HistorySelect, TargetEntryID: "terminal", TargetBranchID: "main"})
	require.NoError(t, err)
	require.Equal(t, "main", selected.ActiveBranchID)
	require.Equal(t, historyContents(before.PublishedView), historyContents(selected.PublishedView))
}

func TestDurableHistoryAdminRejectsIncompleteGroupBeforeWrite(t *testing.T) {
	prepared, dir := seedHistoryAdmin(t)
	before, err := prepared.Snapshot()
	require.NoError(t, err)
	index, ordinal := uint64(0), uint64(0)
	_, err = prepared.DurableWriter().AppendAndSync(t.Context(), SessionEntry{ID: "open", ParentID: before.ActiveLeafEntryID, Type: EntryTypeAssistant, RunID: "open-run", WorkspaceID: "workspace-open", TranscriptIndex: &index, AssistantTranscriptOrdinal: &ordinal, ToolCalls: []llm.ToolCall{{ID: "call", Name: "read"}}})
	require.NoError(t, err)
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	path := filepath.Join(dir, "sessions", snapshot.SessionID+".jsonl")
	bytesBefore, err := os.ReadFile(path)
	require.NoError(t, err)
	_, err = prepared.(HistoryEditor).EditHistory(t.Context(), HistoryEdit{Expected: HistoryCoordinatesFromSnapshot(snapshot), Kind: HistoryFork, TargetEntryID: "answer", NewBranchID: "hide-open"})
	require.Error(t, err)
	bytesAfter, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, bytesBefore, bytesAfter)
}

func TestDurableHistoryAdminCompactionUsesNoRunOwner(t *testing.T) {
	prepared, dir := seedHistoryAdmin(t)
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	compacted, err := prepared.(HistoryEditor).EditHistory(context.Background(), HistoryEdit{Expected: HistoryCoordinatesFromSnapshot(snapshot), Kind: HistoryCompact, TargetEntryID: "answer", Summary: "a concise account"})
	require.NoError(t, err)
	require.Len(t, compacted.PublishedView.Messages, 1)
	require.Contains(t, compacted.PublishedView.Messages[0].Content, "a concise account")
	require.NoError(t, prepared.Close())
	resumed, err := NewDurableJSONLSessionPreparer(dir, nil).PrepareResume(t.Context(), snapshot.SessionID)
	require.NoError(t, err)
	defer resumed.Close()
	require.Equal(t, historyContents(compacted.PublishedView), historyContents(resumed.View()))
}

func historyContents(view SessionView) []string {
	var result []string
	for _, message := range view.Messages {
		result = append(result, message.Content)
	}
	return result
}

func TestDurableHistoryAdminStaleHiddenSequence(t *testing.T) {
	prepared, _ := seedHistoryAdmin(t)
	before, err := prepared.Snapshot()
	require.NoError(t, err)
	_, err = prepared.DurableWriter().AppendAndSync(t.Context(), SessionEntry{ID: "hidden", ParentID: before.ActiveLeafEntryID, Type: EntryTypeSystem, SurfaceOp: SurfaceOpHidden})
	require.NoError(t, err)
	_, err = prepared.(HistoryEditor).EditHistory(t.Context(), HistoryEdit{Expected: HistoryCoordinatesFromSnapshot(before), Kind: HistoryFork, TargetEntryID: "answer", NewBranchID: "stale"})
	require.True(t, errors.Is(err, ErrHistoryChanged))
}

func TestDurableHistoryAdminUncertainSyncCanReopenIntendedBranch(t *testing.T) {
	prepared, dir := seedHistoryAdmin(t)
	before, err := prepared.Snapshot()
	require.NoError(t, err)
	concrete := prepared.(*durablePreparedSession)
	failure := errors.New("administrative sync fault")
	concrete.store.io.sync = func(*os.File) error { return failure }
	_, err = prepared.(HistoryEditor).EditHistory(t.Context(), HistoryEdit{Expected: HistoryCoordinatesFromSnapshot(before), Kind: HistoryFork, TargetEntryID: "answer", NewBranchID: "uncertain"})
	require.ErrorIs(t, err, failure)
	_, err = prepared.Snapshot()
	require.ErrorIs(t, err, ErrDurableWriterPoisoned)
	require.NoError(t, prepared.Close())
	reopened, err := NewDurableJSONLSessionPreparer(dir, nil).PrepareResume(t.Context(), before.SessionID)
	require.NoError(t, err)
	defer reopened.Close()
	snapshot, err := reopened.Snapshot()
	require.NoError(t, err)
	require.Equal(t, "uncertain", snapshot.ActiveBranchID)
}

func TestDurableHistoryAdminRejectsUnknownOwningRun(t *testing.T) {
	prepared, _ := seedHistoryAdmin(t)
	before, err := prepared.Snapshot()
	require.NoError(t, err)
	index := uint64(0)
	_, err = prepared.DurableWriter().AppendAndSync(t.Context(), SessionEntry{ID: "unknown-user", ParentID: before.ActiveLeafEntryID, Type: EntryTypeUser, Content: "unknown", RunID: "unknown", TranscriptIndex: &index}, SessionEntry{ID: "unknown-terminal", ParentID: "unknown-user", Type: EntryTypeCheckpoint, CheckpointKind: CheckpointRunTerminal, RunID: "unknown", RunOutcome: core.RunOutcomeUnknown})
	require.NoError(t, err)
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	_, err = prepared.(HistoryEditor).EditHistory(t.Context(), HistoryEdit{Expected: HistoryCoordinatesFromSnapshot(snapshot), Kind: HistorySelect, TargetEntryID: "answer", TargetBranchID: "main"})
	require.ErrorContains(t, err, "unknown run outcome")
}
