package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestBuildContextStopsAtCompaction verifies that BuildContext only includes
// entries from the last compaction point onwards, excluding earlier history.
func TestBuildContextStopsAtCompaction(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("b", "a", EntryTypeAssistant)))
	require.NoError(t, tree.Append(context.Background(), newCompactionEntry("comp", "b", "summarized history")))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("c", "comp", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("d", "c", EntryTypeAssistant)))

	mg := NewDefaultContextManager(tree)
	sc, err := mg.BuildContext(context.Background(), "d")
	require.NoError(t, err)
	require.NotNil(t, sc)

	// Only entries from the compaction point onwards: comp, c, d.
	require.Len(t, sc.Messages, 3)
	assert.Equal(t, EntryTypeCompaction, sc.Messages[0].Type)
	assert.Equal(t, "summarized history", sc.Messages[0].Content)
	assert.Equal(t, "c", sc.Messages[1].ID)
	assert.Equal(t, "d", sc.Messages[2].ID)

	// RootID is the compaction entry, not the original root.
	assert.Equal(t, "comp", sc.RootID)

	// Traversed still contains all raw entries (leaf to root).
	require.Len(t, sc.Traversed, 5)
	assert.Equal(t, "d", sc.Traversed[0].ID)
	assert.Equal(t, "a", sc.Traversed[4].ID)
}

// TestBuildContextIncludesCompactionSummary verifies that the compaction entry
// is included as a summary message with its Summary as Content.
func TestBuildContextIncludesCompactionSummary(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("root", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newCompactionEntry("comp", "root", "the summary text")))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("after", "comp", EntryTypeUser)))

	mg := NewDefaultContextManager(tree)
	sc, err := mg.BuildContext(context.Background(), "after")
	require.NoError(t, err)
	require.NotNil(t, sc)

	// The compaction entry is folded into a summary message.
	require.Len(t, sc.Messages, 2)
	assert.Equal(t, "comp", sc.Messages[0].ID)
	assert.Equal(t, EntryTypeCompaction, sc.Messages[0].Type)
	assert.Equal(t, "the summary text", sc.Messages[0].Content)
	assert.Equal(t, "after", sc.Messages[1].ID)
}

// TestBuildContextNoCompactionFullHistory verifies that without a compaction
// entry, the full branch history is included.
func TestBuildContextNoCompactionFullHistory(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("b", "a", EntryTypeAssistant)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("c", "b", EntryTypeUser)))

	mg := NewDefaultContextManager(tree)
	sc, err := mg.BuildContext(context.Background(), "c")
	require.NoError(t, err)
	require.NotNil(t, sc)

	// No compaction means the full history is included.
	require.Len(t, sc.Messages, 3)
	assert.Equal(t, "a", sc.Messages[0].ID)
	assert.Equal(t, "b", sc.Messages[1].ID)
	assert.Equal(t, "c", sc.Messages[2].ID)

	// RootID is the original root.
	assert.Equal(t, "a", sc.RootID)
}

// TestBuildContextMultipleCompactionsUseLatest verifies that when multiple
// compaction entries exist, only the latest one is used as the starting point.
func TestBuildContextMultipleCompactionsUseLatest(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newCompactionEntry("comp1", "a", "first summary")))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("b", "comp1", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newCompactionEntry("comp2", "b", "second summary")))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("c", "comp2", EntryTypeUser)))

	mg := NewDefaultContextManager(tree)
	sc, err := mg.BuildContext(context.Background(), "c")
	require.NoError(t, err)
	require.NotNil(t, sc)

	// Only entries from the latest compaction (comp2) onwards: comp2, c.
	require.Len(t, sc.Messages, 2)
	assert.Equal(t, "comp2", sc.Messages[0].ID)
	assert.Equal(t, "second summary", sc.Messages[0].Content)
	assert.Equal(t, "c", sc.Messages[1].ID)

	// RootID is the latest compaction entry.
	assert.Equal(t, "comp2", sc.RootID)
}
