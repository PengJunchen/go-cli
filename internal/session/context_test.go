package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
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
	// With the compaction-point behavior, entries before the last compaction
	// are replaced by the compaction summary, so Messages starts at "comp".
	require.Len(t, sc.Messages, 3)
	assert.Equal(t, EntryTypeCompaction, sc.Messages[0].Type)
	assert.Equal(t, "earlier conversation summary", sc.Messages[0].Content)
	assert.Equal(t, "b", sc.Messages[1].ID)
	assert.Equal(t, "c", sc.Messages[2].ID)

	// RootID, LeafID, EntryCount.
	assert.Equal(t, "c", sc.LeafID)
	assert.Equal(t, "comp", sc.RootID)
	assert.Equal(t, 3, sc.EntryCount)

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

func TestContextManager_BuildContextExcludesHidden(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	// "b" is hidden and should be excluded from the model-visible messages.
	hidden := &SessionEntry{
		ID:        "b",
		ParentID:  "a",
		Type:      EntryTypeUser,
		Content:   "hidden-content",
		Timestamp: time.Date(2024, 5, 1, 12, 0, 'b', 0, time.UTC),
		SurfaceOp: SurfaceOpHidden,
	}
	require.NoError(t, tree.Append(context.Background(), hidden))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("c", "b", EntryTypeAssistant)))

	mg := NewDefaultContextManager(tree)
	sc, err := mg.BuildContext(context.Background(), "c")
	require.NoError(t, err)
	require.NotNil(t, sc)

	// "b" is hidden, so only "a" and "c" appear in Messages.
	require.Len(t, sc.Messages, 2)
	assert.Equal(t, "a", sc.Messages[0].ID)
	assert.Equal(t, "c", sc.Messages[1].ID)

	// Traversed still includes all raw entries (leaf to root): c -> b -> a.
	require.Len(t, sc.Traversed, 3)
	assert.Equal(t, "c", sc.Traversed[0].ID)
	assert.Equal(t, "b", sc.Traversed[1].ID)
	assert.Equal(t, "a", sc.Traversed[2].ID)
}

func TestContextManager_BuildContextConsistentWithDeriveMessages(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newCompactionEntry("comp", "a", "summary of earlier")))
	// Visible entry after compaction.
	require.NoError(t, tree.Append(context.Background(), newTestEntry("b", "comp", EntryTypeAssistant)))
	// Hidden entry after compaction — must be excluded from both paths.
	hidden := &SessionEntry{
		ID:        "c",
		ParentID:  "b",
		Type:      EntryTypeUser,
		Content:   "hidden-content",
		Timestamp: time.Date(2024, 5, 1, 12, 0, 'c', 0, time.UTC),
		SurfaceOp: SurfaceOpHidden,
	}
	require.NoError(t, tree.Append(context.Background(), hidden))
	// Another visible entry.
	require.NoError(t, tree.Append(context.Background(), newTestEntry("d", "c", EntryTypeUser)))

	mg := NewDefaultContextManager(tree)
	sc, err := mg.BuildContext(context.Background(), "d")
	require.NoError(t, err)

	// Retrieve the same branch and convert to []SessionEntry for DeriveMessages.
	branch, err := tree.GetBranch(context.Background(), "d")
	require.NoError(t, err)
	entries := make([]SessionEntry, len(branch))
	for i, e := range branch {
		entries[i] = *e
	}
	derived := DeriveMessages(entries)

	// BuildContext.Messages must match DeriveMessages output exactly, proving
	// the compaction-point logic and SurfaceVisible filtering are unified.
	require.Len(t, sc.Messages, len(derived))
	for i := range sc.Messages {
		assert.Equal(t, derived[i], sc.Messages[i], "entry %d mismatch", i)
	}

	// Sanity: the compaction point is identified — first message is the folded
	// compaction summary, and the hidden entry "c" is absent.
	require.NotEmpty(t, sc.Messages)
	assert.Equal(t, EntryTypeCompaction, sc.Messages[0].Type)
	assert.Equal(t, "summary of earlier", sc.Messages[0].Content)
	for _, m := range sc.Messages {
		assert.NotEqual(t, "c", m.ID, "hidden entry should not appear in messages")
	}
}

func TestTokenEstimateASCII(t *testing.T) {
	// Pure ASCII alphanumerics: ~0.25 tokens per char.
	assert.Equal(t, 2, estimateTokens("abcdefgh")) // 8 * 0.25 = 2
}

func TestTokenEstimateCJK(t *testing.T) {
	// CJK runes count as 2 tokens each, not byte-length/4.
	// "你好世界" is 12 bytes; len()/4 would give 3, but the correct estimate is 8.
	assert.Equal(t, 8, estimateTokens("你好世界")) // 4 * 2 = 8
}

func TestTokenEstimateMixed(t *testing.T) {
	// 3 ASCII letters (0.75) + 2 CJK (4) = 4.75 -> 5
	assert.Equal(t, 5, estimateTokens("abc你好"))
}

func TestTokenEstimateEmpty(t *testing.T) {
	assert.Equal(t, 0, estimateTokens(""))
}

func TestEntriesToAgentMessages(t *testing.T) {
	entries := []SessionEntry{
		{ID: "1", Type: EntryTypeUser, Content: "hello"},
		{ID: "2", Type: EntryTypeAssistant, Content: "hi there", ToolCalls: []llm.ToolCall{{ID: "tc1", Name: "bash"}}, ToolCallID: "tc1", ToolName: "bash"},
		{ID: "3", Type: EntryTypeTool, Content: "tool result"},
		{ID: "4", Type: EntryTypeCompaction, Summary: "compacted"},
		{ID: "5", Type: EntryTypeSystem, Content: "system note"},
	}
	msgs := EntriesToAgentMessages(entries)
	assert.Len(t, msgs, 3)
	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "hello", msgs[0].Content)
	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Equal(t, "hi there", msgs[1].Content)
	assert.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "tc1", msgs[1].ToolCallID)
	assert.Equal(t, "bash", msgs[1].ToolName)
	assert.Equal(t, "system", msgs[2].Role)
	assert.Equal(t, "system note", msgs[2].Content)
}

func TestEntriesToAgentMessages_Empty(t *testing.T) {
	msgs := EntriesToAgentMessages(nil)
	assert.Empty(t, msgs)
}

// TestBuildContext_CacheHitSameLeaf verifies that calling BuildContext twice
// for the same leaf (without branch growth) produces consistent token estimates
// and messages.
func TestBuildContext_CacheHitSameLeaf(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("b", "a", EntryTypeAssistant)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("c", "b", EntryTypeUser)))

	mg := NewDefaultContextManager(tree)

	sc1, err := mg.BuildContext(context.Background(), "c")
	require.NoError(t, err)

	sc2, err := mg.BuildContext(context.Background(), "c")
	require.NoError(t, err)

	assert.Equal(t, sc1.EstimatedTokens, sc2.EstimatedTokens, "token estimates should match on cache hit")
	assert.Equal(t, sc1.EntryCount, sc2.EntryCount)
	assert.Equal(t, sc1.LeafID, sc2.LeafID)
	require.Len(t, sc2.Messages, len(sc1.Messages))
	for i := range sc1.Messages {
		assert.Equal(t, sc1.Messages[i], sc2.Messages[i], "message %d should match", i)
	}
}

// TestBuildContext_CacheMissDifferentLeaf verifies that querying a different
// leaf triggers a full recomputation.
func TestBuildContext_CacheMissDifferentLeaf(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("root", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("pivot", "root", EntryTypeAssistant)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("c1", "pivot", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("c2", "pivot", EntryTypeUser)))

	mg := NewDefaultContextManager(tree)

	sc1, err := mg.BuildContext(context.Background(), "c1")
	require.NoError(t, err)
	assert.Equal(t, "c1", sc1.LeafID)

	sc2, err := mg.BuildContext(context.Background(), "c2")
	require.NoError(t, err)
	assert.Equal(t, "c2", sc2.LeafID)

	// Both should have valid token estimates.
	assert.Greater(t, sc1.EstimatedTokens, 0)
	assert.Greater(t, sc2.EstimatedTokens, 0)
}

// TestBuildContext_CacheIncrementalGrowth verifies that when the branch grows
// after the first BuildContext call, the second call computes the correct total
// token estimate (cached base + delta).
func TestBuildContext_CacheIncrementalGrowth(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("b", "a", EntryTypeAssistant)))

	mg := NewDefaultContextManager(tree)

	// First call with leaf "b".
	sc1, err := mg.BuildContext(context.Background(), "b")
	require.NoError(t, err)
	tokensAfterFirst := sc1.EstimatedTokens

	// Append a new entry — the branch for leaf "b" doesn't change, but
	// we create a new leaf "c" whose branch includes all prior entries.
	require.NoError(t, tree.Append(context.Background(), newTestEntry("c", "b", EntryTypeUser)))

	// Second call with the new leaf "c". The cache is for leaf "b", so
	// this is a cache miss for the leaf ID — full computation is done.
	sc2, err := mg.BuildContext(context.Background(), "c")
	require.NoError(t, err)
	assert.Greater(t, sc2.EstimatedTokens, tokensAfterFirst, "more entries should yield more tokens")

	// Third call with leaf "c" again — cache hit, should match sc2.
	sc3, err := mg.BuildContext(context.Background(), "c")
	require.NoError(t, err)
	assert.Equal(t, sc2.EstimatedTokens, sc3.EstimatedTokens, "cache hit should produce same token estimate")
}

// TestBuildContext_CacheInvalidationNewEntries verifies that appending entries
// and re-querying the same leaf produces correct results. The DefaultSessionTree
// uses the latest entry as the leaf, so after appending, querying the old leaf
// ID returns the same branch, but querying the new leaf returns a longer branch.
func TestBuildContext_CacheInvalidationNewEntries(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))

	mg := NewDefaultContextManager(tree)

	// Build context for "a".
	sc1, err := mg.BuildContext(context.Background(), "a")
	require.NoError(t, err)

	// Append more entries.
	require.NoError(t, tree.Append(context.Background(), newTestEntry("b", "a", EntryTypeAssistant)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("c", "b", EntryTypeUser)))

	// Build context for "c" (longer branch, different leaf — cache miss).
	sc2, err := mg.BuildContext(context.Background(), "c")
	require.NoError(t, err)
	assert.Greater(t, sc2.EstimatedTokens, sc1.EstimatedTokens, "longer branch should have more tokens")
	require.Len(t, sc2.Messages, 3)

	// Build context for "c" again — cache hit.
	sc3, err := mg.BuildContext(context.Background(), "c")
	require.NoError(t, err)
	assert.Equal(t, sc2.EstimatedTokens, sc3.EstimatedTokens)
}

// TestBuildContext_CacheConsistencyWithCompaction verifies that the incremental
// cache produces correct results when compaction entries are present.
func TestBuildContext_CacheConsistencyWithCompaction(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newCompactionEntry("comp", "a", "earlier summary")))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("b", "comp", EntryTypeAssistant)))

	mg := NewDefaultContextManager(tree)

	sc1, err := mg.BuildContext(context.Background(), "b")
	require.NoError(t, err)
	require.Len(t, sc1.Messages, 2) // compaction summary + "b"
	assert.Equal(t, EntryTypeCompaction, sc1.Messages[0].Type)

	// Append a new entry.
	require.NoError(t, tree.Append(context.Background(), newTestEntry("c", "b", EntryTypeUser)))

	// Build for "c" — different leaf, full computation.
	sc2, err := mg.BuildContext(context.Background(), "c")
	require.NoError(t, err)
	require.Len(t, sc2.Messages, 3) // compaction + "b" + "c"
	assert.Equal(t, EntryTypeCompaction, sc2.Messages[0].Type)
	assert.Greater(t, sc2.EstimatedTokens, sc1.EstimatedTokens)

	// Build for "c" again — cache hit.
	sc3, err := mg.BuildContext(context.Background(), "c")
	require.NoError(t, err)
	assert.Equal(t, sc2.EstimatedTokens, sc3.EstimatedTokens, "cache hit should match")
}
