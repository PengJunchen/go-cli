package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestTreeBuilder_EmptyStore(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	store := NewJSONLSessionStore(path)
	require.NoError(t, store.Open(context.Background()))
	defer func() { _ = store.Close() }() //nolint:errcheck

	builder := NewDefaultSessionTreeBuilder()
	tree, err := builder.BuildFromStore(context.Background(), store)
	require.NoError(t, err)
	assert.Equal(t, "", tree.CurrentLeaf())
}

func TestTreeBuilder_MissingFile(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	path := filepath.Join(t.TempDir(), "does-not-exist.jsonl")
	store := NewJSONLSessionStore(path)
	defer func() { _ = store.Close() }() //nolint:errcheck

	builder := NewDefaultSessionTreeBuilder()
	tree, err := builder.BuildFromStore(context.Background(), store)
	require.NoError(t, err)
	assert.Equal(t, "", tree.CurrentLeaf())
}

func TestTreeBuilder_NilStore(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	builder := NewDefaultSessionTreeBuilder()
	tree, err := builder.BuildFromStore(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "", tree.CurrentLeaf())
}

func TestTreeBuilder_WithEntries(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	store := NewJSONLSessionStore(path)
	require.NoError(t, store.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, store.Append(context.Background(), newTestEntry("b", "a", EntryTypeAssistant)))
	require.NoError(t, store.Append(context.Background(), newTestEntry("c1", "b", EntryTypeUser)))
	require.NoError(t, store.Append(context.Background(), newTestEntry("c2", "b", EntryTypeUser)))
	require.NoError(t, store.Close())

	// Re-open from disk to simulate a restart.
	reopened := NewJSONLSessionStore(path)
	require.NoError(t, reopened.Open(context.Background()))
	defer func() { _ = reopened.Close() }() //nolint:errcheck

	builder := NewDefaultSessionTreeBuilder()
	tree, err := builder.BuildFromStore(context.Background(), reopened)
	require.NoError(t, err)

	// Verify parent-child links via GetBranch.
	branch1, err := tree.GetBranch(context.Background(), "c1")
	require.NoError(t, err)
	require.Len(t, branch1, 3)
	assert.Equal(t, "a", branch1[0].ID)
	assert.Equal(t, "b", branch1[1].ID)
	assert.Equal(t, "c1", branch1[2].ID)

	branch2, err := tree.GetBranch(context.Background(), "c2")
	require.NoError(t, err)
	require.Len(t, branch2, 3)
	assert.Equal(t, "a", branch2[0].ID)
	assert.Equal(t, "b", branch2[1].ID)
	assert.Equal(t, "c2", branch2[2].ID)
}

func TestTreeBuilder_CorruptedEntryFailsClosed(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	store := NewJSONLSessionStore(path)
	require.NoError(t, store.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, store.Append(context.Background(), newTestEntry("b", "a", EntryTypeAssistant)))
	require.NoError(t, store.Close())

	// Append a corrupted JSONL line to the file.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString("{this is not valid json}\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	// Re-open from disk; a corrupt complete row must not produce partial tree
	// state by silently skipping part of the transcript.
	reopened := NewJSONLSessionStore(path)
	err = reopened.Open(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode JSONL row")
	defer func() { _ = reopened.Close() }() //nolint:errcheck

	builder := NewDefaultSessionTreeBuilder()
	_, err = builder.BuildFromStore(context.Background(), reopened)
	require.Error(t, err)
}

func TestTreeBuilder_OrphanedEntry(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	store := NewJSONLSessionStore(path)
	require.NoError(t, store.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, store.Append(context.Background(), newTestEntry("b", "a", EntryTypeAssistant)))
	// "d" references a parent that will never exist in the store.
	require.NoError(t, store.Append(context.Background(), newTestEntry("d", "missing-parent", EntryTypeUser)))
	require.NoError(t, store.Close())

	reopened := NewJSONLSessionStore(path)
	require.NoError(t, reopened.Open(context.Background()))
	defer func() { _ = reopened.Close() }() //nolint:errcheck

	builder := NewDefaultSessionTreeBuilder()
	tree, err := builder.BuildFromStore(context.Background(), reopened)
	require.NoError(t, err)

	// "d" is skipped because its parent is missing; "a" and "b" are present.
	branch, err := tree.GetBranch(context.Background(), "b")
	require.NoError(t, err)
	require.Len(t, branch, 2)
	assert.Equal(t, "a", branch[0].ID)
	assert.Equal(t, "b", branch[1].ID)

	// "d" was not appended to the tree.
	_, err = tree.GetBranch(context.Background(), "d")
	assert.ErrorIs(t, err, ErrLeafNotFound)
}

func TestTreeBuilder_CurrentLeaf(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	store := NewJSONLSessionStore(path)
	require.NoError(t, store.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, store.Append(context.Background(), newTestEntry("b", "a", EntryTypeAssistant)))
	require.NoError(t, store.Append(context.Background(), newTestEntry("c", "b", EntryTypeUser)))
	require.NoError(t, store.Close())

	reopened := NewJSONLSessionStore(path)
	require.NoError(t, reopened.Open(context.Background()))
	defer func() { _ = reopened.Close() }() //nolint:errcheck

	builder := NewDefaultSessionTreeBuilder()
	tree, err := builder.BuildFromStore(context.Background(), reopened)
	require.NoError(t, err)

	// "c" has the latest timestamp among the entries, so it is the current leaf.
	assert.Equal(t, "c", tree.CurrentLeaf())

	// BuildContext should replay the full chain root-to-leaf.
	sc, err := tree.BuildContext(context.Background(), "c")
	require.NoError(t, err)
	assert.Equal(t, "c", sc.LeafID)
	assert.Equal(t, 3, sc.EntryCount)
}

func TestTreeBuilder_CurrentLeafWithBranch(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	store := NewJSONLSessionStore(path)
	require.NoError(t, store.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, store.Append(context.Background(), newTestEntry("b", "a", EntryTypeAssistant)))
	// "d" has a later timestamp than "c1"/"c2" (ASCII 'd'=100 > 'c'=99).
	require.NoError(t, store.Append(context.Background(), newTestEntry("c1", "b", EntryTypeUser)))
	require.NoError(t, store.Append(context.Background(), newTestEntry("d", "c1", EntryTypeUser)))
	require.NoError(t, store.Close())

	reopened := NewJSONLSessionStore(path)
	require.NoError(t, reopened.Open(context.Background()))
	defer func() { _ = reopened.Close() }() //nolint:errcheck

	builder := NewDefaultSessionTreeBuilder()
	tree, err := builder.BuildFromStore(context.Background(), reopened)
	require.NoError(t, err)

	// "d" has the latest timestamp and should be the current leaf.
	assert.Equal(t, "d", tree.CurrentLeaf())

	branch, err := tree.GetBranch(context.Background(), "d")
	require.NoError(t, err)
	require.Len(t, branch, 4)
	assert.Equal(t, "a", branch[0].ID)
	assert.Equal(t, "b", branch[1].ID)
	assert.Equal(t, "c1", branch[2].ID)
	assert.Equal(t, "d", branch[3].ID)
}

func TestTreeBuilder_MemoryStore(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	store := NewMemoryStore()
	require.NoError(t, store.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, store.Append(context.Background(), newTestEntry("b", "a", EntryTypeAssistant)))

	builder := NewDefaultSessionTreeBuilder()
	tree, err := builder.BuildFromStore(context.Background(), store)
	require.NoError(t, err)

	branch, err := tree.GetBranch(context.Background(), "b")
	require.NoError(t, err)
	require.Len(t, branch, 2)
	assert.Equal(t, "a", branch[0].ID)
	assert.Equal(t, "b", branch[1].ID)

	// "b" has a later timestamp than "a".
	assert.Equal(t, "b", tree.CurrentLeaf())
}

func TestTreeBuilderSelectsVersionOneLeafBySequenceNotTimestamp(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	root := newR2UserEntry("root", "", 1, 0)
	root.Timestamp = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	first := newR2UserEntry("first", "root", 2, 1)
	first.Timestamp = time.Date(2030, 1, 1, 0, 0, 1, 0, time.UTC)
	latest := newR2UserEntry("latest", "first", 3, 2)
	latest.Timestamp = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, entry := range []*SessionEntry{root, first, latest} {
		require.NoError(t, store.Append(ctx, entry))
	}

	tree, err := NewDefaultSessionTreeBuilder().BuildFromStore(ctx, store)
	require.NoError(t, err)
	assert.Equal(t, "latest", tree.CurrentLeaf())
}

func TestTreeBuilderRestoresActiveLeafCheckpointPointer(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	root := newR2UserEntry("root", "", 1, 0)
	left := newR2UserEntry("left", "root", 2, 1)
	right := newR2UserEntry("right", "root", 3, 2)
	checkpoint := &SessionEntry{
		ID:                "active-pointer",
		ParentID:          "right",
		Type:              EntryTypeCheckpoint,
		RecordVersion:     1,
		SessionID:         "physical-session",
		BranchID:          "main",
		CheckpointKind:    CheckpointActiveLeaf,
		ActiveLeafEntryID: "left",
		Seq:               4,
	}
	for _, entry := range []*SessionEntry{root, left, right, checkpoint} {
		require.NoError(t, store.Append(ctx, entry))
	}

	tree, err := NewDefaultSessionTreeBuilder().BuildFromStore(ctx, store)
	require.NoError(t, err)
	assert.Equal(t, "left", tree.CurrentLeaf())
	branch, err := tree.GetBranch(ctx, tree.CurrentLeaf())
	require.NoError(t, err)
	require.Len(t, branch, 2)
	assert.Equal(t, "root", branch[0].ID)
	assert.Equal(t, "left", branch[1].ID)
}

func TestTreeBuilderRejectsCorruptVersionOneTree(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	root := newR2UserEntry("root", "", 1, 0)
	orphan := newR2UserEntry("orphan", "missing", 2, 1)
	require.NoError(t, store.Append(ctx, root))
	require.NoError(t, store.Append(ctx, orphan))

	_, err := NewDefaultSessionTreeBuilder().BuildFromStore(ctx, store)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parent")
}

func TestTreeBuilderRejectsChildSequenceBeforeParent(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	root := newR2UserEntry("root", "", 3, 0)
	child := newR2UserEntry("child", "root", 2, 1)
	require.NoError(t, store.Append(ctx, root))
	require.NoError(t, store.Append(ctx, child))

	_, err := NewDefaultSessionTreeBuilder().BuildFromStore(ctx, store)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parent")
}

func TestTreeBuilderRejectsBranchCheckpointOutsideParentBranch(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	entries := versionOneBranchBuilderFixture()
	entries = append(entries, &SessionEntry{
		ID:                "unrelated-fork",
		ParentID:          "feature-tip",
		Type:              EntryTypeCheckpoint,
		RecordVersion:     1,
		SessionID:         "physical-session",
		BranchID:          "unrelated",
		ParentBranchID:    "main",
		BaseLeafEntryID:   "feature-tip",
		ActiveLeafEntryID: "unrelated-fork",
		CheckpointKind:    CheckpointBranch,
		Seq:               4,
	})
	for _, entry := range entries {
		require.NoError(t, store.Append(ctx, entry))
	}

	_, err := NewDefaultSessionTreeBuilder().BuildFromStore(ctx, store)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "base leaf is outside parent branch")
}

func TestTreeBuilderRejectsActiveLeafFromUnrelatedBranch(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	entries := versionOneBranchBuilderFixture()
	entries = append(entries, &SessionEntry{
		ID:                "active-mismatch",
		ParentID:          "root",
		Type:              EntryTypeCheckpoint,
		RecordVersion:     1,
		SessionID:         "physical-session",
		BranchID:          "main",
		ActiveLeafEntryID: "feature-tip",
		CheckpointKind:    CheckpointActiveLeaf,
		Seq:               4,
	})
	for _, entry := range entries {
		require.NoError(t, store.Append(ctx, entry))
	}

	_, err := NewDefaultSessionTreeBuilder().BuildFromStore(ctx, store)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "active-leaf checkpoint")
	assert.Contains(t, err.Error(), "outside known branch")
}

func versionOneBranchBuilderFixture() []*SessionEntry {
	root := newR2UserEntry("root", "", 1, 0)
	root.BranchID = "main"
	branchCheckpoint := &SessionEntry{
		ID:                "feature-root",
		ParentID:          "root",
		Type:              EntryTypeCheckpoint,
		RecordVersion:     1,
		SessionID:         "physical-session",
		BranchID:          "feature",
		ParentBranchID:    "main",
		BaseLeafEntryID:   "root",
		ActiveLeafEntryID: "feature-root",
		CheckpointKind:    CheckpointBranch,
		Seq:               2,
	}
	featureTip := newR2UserEntry("feature-tip", "feature-root", 3, 1)
	featureTip.BranchID = "feature"
	return []*SessionEntry{root, branchCheckpoint, featureTip}
}

func TestTreeBuilderRestoresBranchMetadataFromDurableCheckpoint(t *testing.T) {
	ctx := context.Background()
	rootDir := t.TempDir()
	preparer := NewDurableJSONLSessionPreparer(rootDir, nil)
	prepared, err := preparer.PrepareNew(ctx, "branch metadata round trip")
	if err != nil {
		if errors.Is(err, ErrUnsupportedSessionLock) {
			t.Skip("OS-backed flock is unavailable on this platform")
		}
		require.NoError(t, err)
	}
	sessionID := prepared.View().SessionID
	t.Cleanup(func() { _ = prepared.Close() })

	rootIndex := uint64(0)
	sourceIndex := uint64(1)
	root := SessionEntry{ID: "root", Type: EntryTypeUser, Content: "root", RunID: "run-1", TranscriptIndex: &rootIndex}
	source := SessionEntry{ID: "source", ParentID: "root", Type: EntryTypeUser, Content: "source", RunID: "run-1", TranscriptIndex: &sourceIndex}
	_, err = prepared.DurableWriter().AppendAndSync(ctx, root, source)
	require.NoError(t, err)

	checkpoint := SessionEntry{
		ID:                "feature-source",
		ParentID:          "source",
		Type:              EntryTypeCheckpoint,
		Timestamp:         time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC),
		BranchID:          "feature",
		ParentBranchID:    "main",
		BaseLeafEntryID:   "source",
		ActiveLeafEntryID: "feature-source",
		CheckpointKind:    CheckpointBranch,
	}
	_, err = prepared.DurableWriter().AppendAndSync(ctx, checkpoint)
	require.NoError(t, err)
	require.NoError(t, prepared.Close())

	filePath := filepath.Join(rootDir, "sessions", sessionID+".jsonl")
	reopened := NewJSONLSessionStore(filePath)
	require.NoError(t, reopened.Open(ctx))
	t.Cleanup(func() { _ = reopened.Close() })

	tree, err := NewDefaultSessionTreeBuilder().BuildFromStore(ctx, reopened)
	require.NoError(t, err)
	assert.Equal(t, "feature-source", tree.CurrentLeaf())
	branch, err := tree.GetBranch(ctx, tree.CurrentLeaf())
	require.NoError(t, err)
	require.Len(t, branch, 3)
	assert.Equal(t, "root", branch[0].ID)
	assert.Equal(t, "source", branch[1].ID)
	assert.Equal(t, "feature-source", branch[2].ID)

	concrete, ok := tree.(*DefaultSessionTree)
	require.True(t, ok)
	meta, found := concrete.BranchMetaFor("feature")
	require.True(t, found, "branch metadata must be reconstructed from its checkpoint")
	assert.Equal(t, "source", meta.ParentID)
	assert.Equal(t, "source", meta.BaseLeafID)
	assert.Equal(t, checkpoint.Timestamp, meta.CreatedAt)
	assert.Equal(t, []BranchMeta{meta}, concrete.ListBranches())
}
