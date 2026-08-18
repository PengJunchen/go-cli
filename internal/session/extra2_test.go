package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTree_MoveToEmptyTree verifies MoveTo on an empty tree reports the missing
// leaf.
func TestTree_MoveToEmptyTree(t *testing.T) {
	tree := newConcreteTree()
	require.ErrorIs(t, tree.MoveTo(context.Background(), "ghost"), ErrLeafNotFound)
}

// TestTree_GetBranchEmptyTree verifies GetBranch on an empty tree fails cleanly.
func TestTree_GetBranchEmptyTree(t *testing.T) {
	tree := newConcreteTree()
	branch, err := tree.GetBranch(context.Background(), "ghost")
	require.ErrorIs(t, err, ErrLeafNotFound)
	assert.Nil(t, branch)
}

// TestTree_BranchNilOption verifies Branch tolerates a nil option.
func TestTree_BranchNilOption(t *testing.T) {
	ctx := context.Background()
	tree := newConcreteTree()
	require.NoError(t, tree.Append(ctx, newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(ctx, newTestEntry("b", "a", EntryTypeUser)))

	require.NoError(t, tree.Branch(ctx, "a", nil, WithBranchID("f")))
	meta, ok := tree.BranchMetaFor("f")
	require.True(t, ok)
	assert.Equal(t, "a", meta.BaseLeafID)
}

// TestTree_BranchMetaForUnknown verifies an unknown branch id yields false.
func TestTree_BranchMetaForUnknown(t *testing.T) {
	tree := newConcreteTree()
	_, ok := tree.BranchMetaFor("nope")
	assert.False(t, ok)
}

// TestTree_BranchReusesFromIDWhenWithoutBranchID verifies keepBranchID falls
// back to fromID and records the branch meta under it.
func TestTree_BranchReusesFromIDWhenWithoutBranchID(t *testing.T) {
	ctx := context.Background()
	tree := newConcreteTree()
	require.NoError(t, tree.Append(ctx, newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(ctx, newTestEntry("b", "a", EntryTypeUser)))

	require.NoError(t, tree.Branch(ctx, "b"))
	meta, ok := tree.BranchMetaFor("b")
	require.True(t, ok)
	assert.Equal(t, "b", meta.BranchID)
	assert.Equal(t, "b", meta.BaseLeafID)
	assert.Equal(t, "b", tree.CurrentLeaf())
}

// TestKeepBranchID verifies the fallback behavior directly.
func TestKeepBranchID(t *testing.T) {
	assert.Equal(t, "explicit", keepBranchID("explicit", "from"))
	assert.Equal(t, "from", keepBranchID("", "from"))
}

// TestTree_AppendDoesNotMoveLeafOnSubsequent verifies only the first appended
// entry becomes the current leaf.
func TestTree_AppendDoesNotMoveLeafOnSubsequent(t *testing.T) {
	ctx := context.Background()
	tree := newConcreteTree()
	require.NoError(t, tree.Append(ctx, newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(ctx, newTestEntry("b", "a", EntryTypeUser)))
	require.NoError(t, tree.Append(ctx, newTestEntry("c", "b", EntryTypeUser)))
	assert.Equal(t, "a", tree.CurrentLeaf(), "append must not advance the leaf after the first entry")
}

// TestDefaultBranchSummary_SummarizeErrorsFromFunc verifies an injected
// SummarizeFunc error surfaces.
func TestDefaultBranchSummary_SummarizeErrorsFromFunc(t *testing.T) {
	d := NewDefaultBranchSummary(func(context.Context, string) (string, error) { //nolint:errcheck
		return "", errors.New("llm down")
	}).(*DefaultBranchSummary)
	_, err := d.Summarize(context.Background(), []SessionEntry{{ID: "a", Content: "x"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "llm down")
}

// TestDefaultBranchSummary_EmptyEntriesProducesPrompt verifies summarizing an
// empty entry set still calls the summarizer (prompt still built).
func TestDefaultBranchSummary_EmptyEntriesProducesPrompt(t *testing.T) {
	called := false
	d := NewDefaultBranchSummary(func(_ context.Context, _ string) (string, error) { //nolint:errcheck
		called = true
		return "empty-sum", nil
	}).(*DefaultBranchSummary)
	sum, err := d.Summarize(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "empty-sum", sum)
	assert.True(t, called)
}

// TestDefaultBranchSummary_BuildPrompt empty content yields a prompt that still
// carries the instruction header.
func TestDefaultBranchSummary_BuildPromptHeader(t *testing.T) {
	d := NewDefaultBranchSummary(func(context.Context, string) (string, error) { return "", nil }).(*DefaultBranchSummary) //nolint:errcheck
	// We cannot call unexported buildPrompt from here? We can, same package.
	prompt := d.buildPrompt([]SessionEntry{{ID: "a", Content: "hi"}})
	assert.Contains(t, prompt, "Summarize the following departed conversation branch")
	assert.Contains(t, prompt, "hi")
}

// TestMemoryStoreDefaultTypeAlias verifies NewDefaultSessionStore returns the
// alias type and behaves like MemoryStore.
func TestMemoryStoreDefaultTypeAlias(t *testing.T) {
	s := NewDefaultSessionStore()
	require.NoError(t, s.Append(context.Background(), newTestEntry("e", "", EntryTypeUser)))
	got, err := s.Get(context.Background(), "e")
	require.NoError(t, err)
	assert.Equal(t, "content-e", got.Content)
	require.NoError(t, s.Save(context.Background()))
}

// TestMemoryStoreAppendValidation verifies validation short-circuits nil/id/type
// before touching state.
func TestMemoryStoreAppendValidation(t *testing.T) {
	s := NewMemoryStore().(*MemoryStore) //nolint:errcheck
	require.Error(t, s.Append(context.Background(), nil))
	require.Error(t, s.Append(context.Background(), &SessionEntry{Type: EntryTypeUser}))
	require.Error(t, s.Append(context.Background(), &SessionEntry{ID: "x"}))
	assert.Equal(t, 0, len(s.entries))
}

// TestWriteJSONLineNilFile verifies writing to a nil writer reports an error.
func TestWriteJSONLineNilFile(t *testing.T) {
	s := &JSONLSessionStore{}
	err := s.writeJSONLine(newTestEntry("a", "", EntryTypeUser))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not open")
}

// TestSessionContextJSON verifies SessionContext round-trips through JSON.
func TestSessionContextJSON(t *testing.T) {
	ts := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	sc := &SessionContext{
		LeafID:          "leaf",
		RootID:          "root",
		Messages:        []SessionEntry{{ID: "m1", Type: EntryTypeUser, Content: "hi"}},
		EntryCount:      1,
		EstimatedTokens: 4,
		LastUpdate:      ts,
	}
	data, err := json.Marshal(sc)
	require.NoError(t, err)

	var decoded SessionContext
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, "leaf", decoded.LeafID)
	assert.Equal(t, "root", decoded.RootID)
	assert.Len(t, decoded.Messages, 1)
	assert.Equal(t, 1, decoded.EntryCount)
	assert.Equal(t, 4, decoded.EstimatedTokens)
	assert.True(t, decoded.LastUpdate.Equal(ts))
}

// TestBranchMetaJSON verifies BranchMeta serializes its fields.
func TestBranchMetaJSON(t *testing.T) {
	ts := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	m := BranchMeta{BranchID: "b", ParentID: "p", CreatedAt: ts, BaseLeafID: "bl"}
	data, err := json.Marshal(m)
	require.NoError(t, err)

	var decoded BranchMeta
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, "b", decoded.BranchID)
	assert.Equal(t, "bl", decoded.BaseLeafID)
	assert.True(t, decoded.CreatedAt.Equal(ts))
}

// TestContextManagerCompactionTraversedHasRawEntries verifies Traversed keeps
// the raw compaction entry (with empty Content) while Messages folds it.
func TestContextManagerCompactionTraversedHasRawEntries(t *testing.T) {
	tree := newConcreteTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newCompactionEntry("comp", "a", "folded")))

	mg := NewDefaultContextManager(tree)
	sc, err := mg.BuildContext(context.Background(), "comp")
	require.NoError(t, err)

	// Traversed preserves the raw compaction entry (Content empty by default).
	require.Len(t, sc.Traversed, 2)
	assert.Equal(t, EntryTypeCompaction, sc.Traversed[0].Type, "traversed is leaf-to-root, starting with the leaf")

	// Messages folds the summary into Content. With compaction-point behavior,
	// the compaction entry is the first (and only) message.
	assert.Equal(t, "folded", sc.Messages[0].Content)
}

// TestContextManagerBuildContextWithSystemEntries verifies system entries pass
// through into Messages unchanged (no special folding).
func TestContextManagerBuildContextWithSystemEntries(t *testing.T) {
	tree := newConcreteTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), &SessionEntry{
		ID: "sys", ParentID: "a", Type: EntryTypeSystem, Content: "system note", Timestamp: time.Now().UTC(),
	}))

	mg := NewDefaultContextManager(tree)
	sc, err := mg.BuildContext(context.Background(), "sys")
	require.NoError(t, err)
	require.Len(t, sc.Messages, 2)
	assert.Equal(t, EntryTypeSystem, sc.Messages[1].Type)
	assert.Equal(t, "system note", sc.Messages[1].Content)
}
