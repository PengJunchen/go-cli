package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestSessionEntryClone_DeepCopySlices verifies that clone() deep copies
// ContentBlocks and ToolCalls slices so the clone is independent of the
// original.
func TestSessionEntryClone_DeepCopySlices(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	orig := &SessionEntry{
		ID:      "e1",
		Type:    EntryTypeAssistant,
		Content: "hello",
		ContentBlocks: []llm.ContentBlock{
			{Type: "text", Text: "block1"},
		},
		ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "read"},
		},
		Timestamp: time.Now(),
	}

	cp := orig.clone()

	// Mutate the clone's slices; the original must be unaffected.
	cp.ContentBlocks[0].Text = "mutated"
	cp.ToolCalls[0].Name = "mutated"

	assert.Equal(t, "block1", orig.ContentBlocks[0].Text, "original ContentBlocks must be unchanged")
	assert.Equal(t, "read", orig.ToolCalls[0].Name, "original ToolCalls must be unchanged")
}

// TestClone_PreservesContentBlocks verifies that Clone deep-copies entries
// including ContentBlocks and ToolCalls into the new branch.
func TestClone_PreservesContentBlocks(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	tree := newConcreteTree()

	entryWithBlocks := &SessionEntry{
		ID:        "e0",
		ParentID:  "",
		Type:      EntryTypeAssistant,
		Content:   "assistant msg",
		Timestamp: time.Now(),
		ContentBlocks: []llm.ContentBlock{
			{Type: "text", Text: "block-content"},
		},
		ToolCalls: []llm.ToolCall{
			{ID: "tc-1", Name: "write"},
		},
	}
	require.NoError(t, tree.Append(ctx, entryWithBlocks))
	require.NoError(t, tree.MoveTo(ctx, "e0"))

	require.NoError(t, tree.Clone(ctx, "e0", "copy"))

	branch, err := tree.GetBranch(ctx, "copy-e0")
	require.NoError(t, err)
	require.Len(t, branch, 1)

	cloned := branch[0]
	assert.Equal(t, "copy-e0", cloned.ID)
	require.Len(t, cloned.ContentBlocks, 1)
	assert.Equal(t, "block-content", cloned.ContentBlocks[0].Text)
	require.Len(t, cloned.ToolCalls, 1)
	assert.Equal(t, "tc-1", cloned.ToolCalls[0].ID)
	assert.Equal(t, "write", cloned.ToolCalls[0].Name)
}

// TestBranchMeta_PersistedToJSONL verifies that a Branch operation persists
// BranchMeta to the JSONLBranchStore and that reloading restores the branch.
func TestBranchMeta_PersistedToJSONL(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	branchPath := filepath.Join(t.TempDir(), "branches.jsonl")

	// Phase 1: create tree, wire BranchStore, create a branch.
	tree1 := newConcreteTree()
	require.NoError(t, tree1.Append(ctx, newTestEntry("e0", "", EntryTypeUser)))
	require.NoError(t, tree1.Append(ctx, newTestEntry("e1", "e0", EntryTypeUser)))
	require.NoError(t, tree1.MoveTo(ctx, "e1"))

	bs1 := NewJSONLBranchStore(branchPath)
	tree1.SetBranchStore(bs1)

	require.NoError(t, tree1.Branch(ctx, "e1", WithBranchID("feature")))
	require.NoError(t, bs1.Close())

	// Phase 2: create a new tree and BranchStore from the same file; the
	// persisted branch should be loaded.
	tree2 := newConcreteTree()
	require.NoError(t, tree2.Append(ctx, newTestEntry("e0", "", EntryTypeUser)))
	require.NoError(t, tree2.Append(ctx, newTestEntry("e1", "e0", EntryTypeUser)))

	bs2 := NewJSONLBranchStore(branchPath)
	tree2.SetBranchStore(bs2)

	meta, ok := tree2.BranchMetaFor("feature")
	require.True(t, ok, "branch 'feature' should be restored from JSONL")
	assert.Equal(t, "feature", meta.BranchID)
	assert.Equal(t, "e1", meta.BaseLeafID)
	require.NoError(t, bs2.Close())
}

// TestBranchMeta_GitBranchPersisted verifies that a BranchMeta with GitBranch
// persists and restores correctly through JSONLBranchStore.
func TestBranchMeta_GitBranchPersisted(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	branchPath := filepath.Join(t.TempDir(), "git_branches.jsonl")

	tree := newConcreteTree()
	require.NoError(t, tree.Append(ctx, newTestEntry("r", "", EntryTypeUser)))
	require.NoError(t, tree.MoveTo(ctx, "r"))

	bs := NewJSONLBranchStore(branchPath)
	tree.SetBranchStore(bs)

	require.NoError(t, tree.Branch(ctx, "r", WithBranchID("dev"), WithGitBranch("feature-xyz")))
	require.NoError(t, bs.Close())

	// Reload via a fresh store and tree.
	tree2 := newConcreteTree()
	require.NoError(t, tree2.Append(ctx, newTestEntry("r", "", EntryTypeUser)))

	bs2 := NewJSONLBranchStore(branchPath)
	tree2.SetBranchStore(bs2)

	meta, ok := tree2.BranchMetaFor("dev")
	require.True(t, ok)
	assert.Equal(t, "feature-xyz", meta.GitBranch)
	require.NoError(t, bs2.Close())
}

// TestJSONLBranchStore_AppendAfterClose verifies that calling AppendBranch
// after Close returns an error without panicking.
func TestJSONLBranchStore_AppendAfterClose(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	path := filepath.Join(t.TempDir(), "after_close.jsonl")
	bs := NewJSONLBranchStore(path)

	// LoadBranches triggers lazy open so the file is created and opened.
	_, err := bs.LoadBranches(context.Background())
	require.NoError(t, err)

	require.NoError(t, bs.Close())

	// AppendBranch after Close should error, not panic.
	err = bs.AppendBranch(context.Background(), BranchMeta{BranchID: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "closed")
}

// TestJSONLBranchStore_CorruptLineSkipped verifies that corrupt JSON lines are
// skipped during LoadBranches while valid lines are still loaded.
func TestJSONLBranchStore_CorruptLineSkipped(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	path := filepath.Join(t.TempDir(), "corrupt.jsonl")

	// Write one valid line, one corrupt line, and one valid line.
	valid1 := `{"branch_id":"b1","parent_id":"p1","created_at":"2024-01-01T00:00:00Z","base_leaf_id":"p1"}`
	corrupt := `{not valid json`
	valid2 := `{"branch_id":"b2","parent_id":"p2","created_at":"2024-01-02T00:00:00Z","base_leaf_id":"p2"}`
	content := valid1 + "\n" + corrupt + "\n" + valid2 + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	bs := NewJSONLBranchStore(path)
	loaded, err := bs.LoadBranches(context.Background())
	require.NoError(t, err)
	require.Len(t, loaded, 2)

	byID := make(map[string]bool, len(loaded))
	for _, m := range loaded {
		byID[m.BranchID] = true
	}
	assert.True(t, byID["b1"])
	assert.True(t, byID["b2"])
	require.NoError(t, bs.Close())
}
