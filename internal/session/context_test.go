package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func newCompactionEntry(id, parentID, summary string) *SessionEntry {
	return &SessionEntry{
		ID:        id,
		ParentID:  parentID,
		Type:      EntryTypeCompaction,
		Summary:   summary,
		Timestamp: time.Date(2024, 5, 1, 12, 0, int(id[0]), 0, time.UTC),
	}
}

func TestContextManager_BuildContextOrderAndFold(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newCompactionEntry("comp", "a", "earlier conversation summary")))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("b", "comp", EntryTypeAssistant)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("c", "b", EntryTypeUser)))

	mg := NewDefaultContextManager(tree)
	sc, err := mg.BuildContext(context.Background(), "c")
	require.NoError(t, err)
	require.NotNil(t, sc)

	// Messages are ordered root to leaf; the compaction is folded to a summary.
	require.Len(t, sc.Messages, 4)
	assert.Equal(t, "a", sc.Messages[0].ID)
	assert.Equal(t, EntryTypeCompaction, sc.Messages[1].Type)
	assert.Equal(t, "earlier conversation summary", sc.Messages[1].Content)
	assert.Equal(t, "b", sc.Messages[2].ID)
	assert.Equal(t, "c", sc.Messages[3].ID)

	// RootID, LeafID, EntryCount.
	assert.Equal(t, "c", sc.LeafID)
	assert.Equal(t, "a", sc.RootID)
	assert.Equal(t, 4, sc.EntryCount)

	// Traversed are the raw entries visited in walk (leaf to root) order,
	// following the parent chain: c -> b -> comp -> a.
	require.Len(t, sc.Traversed, 4)
	assert.Equal(t, "c", sc.Traversed[0].ID)
	assert.Equal(t, "b", sc.Traversed[1].ID)
	assert.Equal(t, "comp", sc.Traversed[2].ID)
	assert.Equal(t, "a", sc.Traversed[3].ID)

	// Token estimate is positive.
	assert.Greater(t, sc.EstimatedTokens, 0)
	assert.False(t, sc.LastUpdate.IsZero())
}

func TestContextManager_NoCompaction(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	for _, id := range []string{"a", "b", "c"} {
		parent := ""
		if id != "a" {
			parent = string(rune(id[0] - 1))
		}
		require.NoError(t, tree.Append(context.Background(), newTestEntry(id, parent, EntryTypeUser)))
	}

	mg := NewDefaultContextManager(tree)
	sc, err := mg.BuildContext(context.Background(), "c")
	require.NoError(t, err)
	require.Len(t, sc.Messages, 3)
	// No compaction means every message retains its original content/type.
	assert.Equal(t, EntryTypeUser, sc.Messages[0].Type)
	assert.Equal(t, "content-a", sc.Messages[0].Content)
}

func TestContextManager_MissingLeaf(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))

	mg := NewDefaultContextManager(tree)
	sc, err := mg.BuildContext(context.Background(), "nope")
	require.ErrorIs(t, err, ErrLeafNotFound)
	assert.Nil(t, sc)
}

func TestContextManager_ReconstructFromBranch(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("root", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("pivot", "root", EntryTypeAssistant)))
	// Two arbitrary child leaves off pivot.
	require.NoError(t, tree.Append(context.Background(), newTestEntry("c1", "pivot", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("c2", "pivot", EntryTypeUser)))

	mg := NewDefaultContextManager(tree)
	sc1, err := mg.BuildContext(context.Background(), "c1")
	require.NoError(t, err)
	require.Len(t, sc1.Messages, 3)
	assert.Equal(t, "c1", sc1.Messages[2].ID)

	sc2, err := mg.BuildContext(context.Background(), "c2")
	require.NoError(t, err)
	require.Len(t, sc2.Messages, 3)
	assert.Equal(t, "c2", sc2.Messages[2].ID)
}

func TestContextManager_RebuildSpan(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, exp := tracedCtx(t)
	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(ctx, newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(ctx, newTestEntry("b", "a", EntryTypeUser)))

	mg := NewDefaultContextManager(tree)
	_, err := mg.BuildContext(ctx, "b")
	require.NoError(t, err)

	validateSpan(t, exp, "context.rebuild")
	time.Sleep(50 * time.Millisecond)
	exp.AssertSpanExists(t, "context.rebuild")
}
