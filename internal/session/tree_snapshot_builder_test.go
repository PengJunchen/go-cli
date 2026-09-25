package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestTreeBuilderBuildFromSnapshotUsesPreparedCursorAfterTerminal(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(ctx, "terminal snapshot")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Close()) })

	zero := uint64(0)
	user, err := prepared.DurableWriter().AppendAndSync(ctx, SessionEntry{
		ID: "terminal-user", Type: EntryTypeUser, RunID: "terminal-run", TranscriptIndex: &zero, Content: "hello",
	})
	require.NoError(t, err)
	_, err = prepared.DurableWriter().AppendAndSync(ctx, SessionEntry{
		ID: "terminal-checkpoint", ParentID: user[0].ID, Type: EntryTypeCheckpoint,
		RunID: "terminal-run", RunOutcome: core.RunOutcomeSuccess, CheckpointKind: CheckpointRunTerminal,
	})
	require.NoError(t, err)

	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	require.Equal(t, []string{user[0].ID}, snapshot.PublishedView.SourceEntryIDs)

	tree, err := NewDefaultSessionTreeBuilder().BuildFromSnapshot(ctx, snapshot)
	require.NoError(t, err)
	assert.Equal(t, snapshot.ActiveLeafEntryID, tree.CurrentLeaf())
	assert.Equal(t, "terminal-checkpoint", tree.CurrentLeaf())
	branch, err := tree.GetBranch(ctx, tree.CurrentLeaf())
	require.NoError(t, err)
	assert.Equal(t, []string{"terminal-user", "terminal-checkpoint"}, entryIDs(branch))
}

func TestTreeBuilderBuildFromSnapshotUsesExactLegacyLeaf(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	late := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	early := late.Add(-time.Hour)
	snapshot := PreparedSessionSnapshot{
		SessionID:         "legacy-physical",
		ActiveBranchID:    "main",
		ActiveLeafEntryID: "left",
		DurableEntries: []SessionEntry{
			{ID: "root", Type: EntryTypeUser, Timestamp: late},
			{ID: "left", ParentID: "root", Type: EntryTypeAssistant, Timestamp: early},
			{ID: "right", ParentID: "root", Type: EntryTypeUser, Timestamp: late.Add(time.Hour)},
		},
		PublishedView: SessionView{
			SessionID: "legacy-physical", BranchID: "main", Revision: "legacy-revision",
			Messages: []core.AgentMessage{{Role: "user", Content: "root"}}, SourceEntryIDs: []string{"root"},
		},
	}

	tree, err := NewDefaultSessionTreeBuilder().BuildFromSnapshot(ctx, snapshot)
	require.NoError(t, err)
	assert.Equal(t, "left", tree.CurrentLeaf(), "snapshot cursor is authoritative even when timestamps disagree")
}

func TestTreeBuilderBuildFromSnapshotRestoresVersionOneBranchAndCheckpointCursor(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	prepared, err := NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(ctx, "branch snapshot")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Close()) })

	zero := uint64(0)
	root, err := prepared.DurableWriter().AppendAndSync(ctx, SessionEntry{
		ID: "branch-root", Type: EntryTypeUser, RunID: "branch-run", TranscriptIndex: &zero,
		Timestamp: time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC), Content: "root",
	})
	require.NoError(t, err)
	branchCheckpoint, err := prepared.DurableWriter().AppendAndSync(ctx, SessionEntry{
		ID: "feature-root", ParentID: root[0].ID, Type: EntryTypeCheckpoint,
		BranchID: "feature", ParentBranchID: "main", BaseLeafEntryID: root[0].ID,
		ActiveLeafEntryID: "feature-root", CheckpointKind: CheckpointBranch,
	})
	require.NoError(t, err)
	one := uint64(1)
	_, err = prepared.DurableWriter().AppendAndSync(ctx, SessionEntry{
		ID: "feature-tip", ParentID: branchCheckpoint[0].ID, Type: EntryTypeUser,
		RunID: "branch-run", TranscriptIndex: &one, Timestamp: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Content: "tip",
	})
	require.NoError(t, err)
	_, err = prepared.DurableWriter().AppendAndSync(ctx, SessionEntry{
		ID: "feature-active-pointer", ParentID: "feature-tip", Type: EntryTypeCheckpoint,
		CheckpointKind: CheckpointActiveLeaf, ActiveLeafEntryID: "feature-root",
	})
	require.NoError(t, err)

	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	require.Equal(t, "feature", snapshot.ActiveBranchID)
	require.Equal(t, "feature-root", snapshot.ActiveLeafEntryID)

	tree, err := NewDefaultSessionTreeBuilder().BuildFromSnapshot(ctx, snapshot)
	require.NoError(t, err)
	assert.Equal(t, "feature-root", tree.CurrentLeaf())
	branch, err := tree.GetBranch(ctx, tree.CurrentLeaf())
	require.NoError(t, err)
	assert.Equal(t, []string{"branch-root", "feature-root"}, entryIDs(branch))

	concrete, ok := tree.(*DefaultSessionTree)
	require.True(t, ok)
	meta, exists := concrete.BranchMetaFor("feature")
	require.True(t, exists)
	assert.Equal(t, "branch-root", meta.BaseLeafID)

	wrongBranch := snapshot.Clone()
	wrongBranch.ActiveBranchID = "main"
	wrongBranch.PublishedView.BranchID = "main"
	_, err = NewDefaultSessionTreeBuilder().BuildFromSnapshot(ctx, wrongBranch)
	require.ErrorContains(t, err, "outside active branch")
}

func TestTreeBuilderBuildFromSnapshotKeepsPublishedViewSeparateFromOpenJournal(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	_, prepared, _ := prepareTwoCallCompletionFixture(t, false)
	t.Cleanup(func() { require.NoError(t, prepared.Close()) })

	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	require.Equal(t, []string{"root-user"}, snapshot.PublishedView.SourceEntryIDs)
	require.Len(t, snapshot.PublishedView.Messages, 1)

	tree, err := NewDefaultSessionTreeBuilder().BuildFromSnapshot(context.Background(), snapshot)
	require.NoError(t, err, "an incomplete tool-call journal tail is not a model-history rebuild request")
	assert.Equal(t, snapshot.ActiveLeafEntryID, tree.CurrentLeaf())
	assert.Equal(t, "resolution-1", tree.CurrentLeaf())
	assert.Equal(t, []string{"root-user"}, snapshot.PublishedView.SourceEntryIDs)
}

func TestTreeBuilderBuildFromSnapshotRejectsMismatchedMetadata(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	_, prepared, _ := prepareTwoCallCompletionFixture(t, false)
	t.Cleanup(func() { require.NoError(t, prepared.Close()) })
	base, err := prepared.Snapshot()
	require.NoError(t, err)

	tests := []struct {
		name   string
		mutate func(*PreparedSessionSnapshot)
	}{
		{name: "missing physical ID", mutate: func(snapshot *PreparedSessionSnapshot) { snapshot.SessionID = "" }},
		{name: "missing active branch", mutate: func(snapshot *PreparedSessionSnapshot) { snapshot.ActiveBranchID = "" }},
		{name: "view physical ID mismatch", mutate: func(snapshot *PreparedSessionSnapshot) { snapshot.PublishedView.SessionID = "other-session" }},
		{name: "view branch mismatch", mutate: func(snapshot *PreparedSessionSnapshot) { snapshot.PublishedView.BranchID = "other-branch" }},
		{name: "unknown active branch", mutate: func(snapshot *PreparedSessionSnapshot) { snapshot.ActiveBranchID = "unknown-branch" }},
		{name: "missing active leaf", mutate: func(snapshot *PreparedSessionSnapshot) { snapshot.ActiveLeafEntryID = "missing-leaf" }},
		{name: "missing published source", mutate: func(snapshot *PreparedSessionSnapshot) { snapshot.PublishedView.SourceEntryIDs[0] = "missing-source" }},
		{name: "journal row as published source", mutate: func(snapshot *PreparedSessionSnapshot) { snapshot.PublishedView.SourceEntryIDs[0] = "execution-1" }},
		{name: "max sequence mismatch", mutate: func(snapshot *PreparedSessionSnapshot) { snapshot.MaxSeq++ }},
		{name: "entry physical ID mismatch", mutate: func(snapshot *PreparedSessionSnapshot) { snapshot.DurableEntries[0].SessionID = "other-session" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer verify.AssertNoGoroutineLeak(t)()
			snapshot := base.Clone()
			test.mutate(&snapshot)
			_, err := NewDefaultSessionTreeBuilder().BuildFromSnapshot(context.Background(), snapshot)
			require.Error(t, err)
		})
	}
}

func TestTreeBuilderBuildFromSnapshotDoesNotAliasEntryPayloads(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	args := map[string]any{"nested": []any{map[string]any{"value": "original"}}}
	snapshot := PreparedSessionSnapshot{
		SessionID: "clone-session", ActiveBranchID: "main", ActiveLeafEntryID: "assistant",
		DurableEntries: []SessionEntry{{
			ID: "assistant", Type: EntryTypeAssistant, Content: "hello",
			ToolCalls: []llm.ToolCall{{ID: "call", Name: "read", Args: args}},
		}},
		PublishedView: SessionView{
			SessionID: "clone-session", BranchID: "main", Revision: "revision",
			Messages: []core.AgentMessage{{Role: "assistant", Content: "hello"}}, SourceEntryIDs: []string{"assistant"},
		},
	}

	tree, err := NewDefaultSessionTreeBuilder().BuildFromSnapshot(ctx, snapshot)
	require.NoError(t, err)
	args["nested"].([]any)[0].(map[string]any)["value"] = "changed"

	branch, err := tree.GetBranch(ctx, "assistant")
	require.NoError(t, err)
	got := branch[0].ToolCalls[0].Args.(map[string]any)["nested"].([]any)[0].(map[string]any)["value"]
	assert.Equal(t, "original", got)
}

func entryIDs(entries []*SessionEntry) []string {
	ids := make([]string, len(entries))
	for index, entry := range entries {
		ids[index] = entry.ID
	}
	return ids
}
