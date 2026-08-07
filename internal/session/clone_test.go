package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestListBranches verifies ListBranches returns metadata for every recorded
// branch.
func TestListBranches(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := newConcreteTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("e0", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("e1", "e0", EntryTypeUser)))
	require.NoError(t, tree.MoveTo(context.Background(), "e1"))

	require.NoError(t, tree.Branch(context.Background(), "e1", WithBranchID("b1")))
	require.NoError(t, tree.Branch(context.Background(), "e0", WithBranchID("b2")))

	branches := tree.ListBranches()
	require.Len(t, branches, 2)

	byID := make(map[string]BranchMeta, len(branches))
	for _, b := range branches {
		byID[b.BranchID] = b
	}
	require.Contains(t, byID, "b1")
	require.Contains(t, byID, "b2")
	assert.Equal(t, "e1", byID["b1"].BaseLeafID)
	assert.Equal(t, "e0", byID["b2"].BaseLeafID)
}

// TestListBranches_Empty verifies an empty tree yields no branches.
func TestListBranches_Empty(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := newConcreteTree()
	branches := tree.ListBranches()
	assert.Empty(t, branches)
}

// TestClone verifies Clone deep-copies entries into a new id space, sets the new
// leaf as current, and records branch metadata.
func TestClone(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	tree := newConcreteTree()
	require.NoError(t, tree.Append(ctx, newTestEntry("e0", "", EntryTypeUser)))
	require.NoError(t, tree.Append(ctx, newTestEntry("e1", "e0", EntryTypeAssistant)))
	require.NoError(t, tree.Append(ctx, newTestEntry("e2", "e1", EntryTypeUser)))
	require.NoError(t, tree.MoveTo(ctx, "e2"))
	before := tree.EntryCount()

	require.NoError(t, tree.Clone(ctx, "e2", "copy"))

	// New entries were added (deep copy, not zero-copy).
	assert.Equal(t, before+3, tree.EntryCount(), "clone must add new entries")
	// The new branch's leaf became the current leaf.
	assert.Equal(t, "copy-e2", tree.CurrentLeaf())

	// The cloned branch is reconstructable and independent.
	branch, err := tree.GetBranch(ctx, "copy-e2")
	require.NoError(t, err)
	require.Len(t, branch, 3)
	assert.Equal(t, "copy-e0", branch[0].ID)
	assert.Equal(t, "copy-e1", branch[1].ID)
	assert.Equal(t, "copy-e2", branch[2].ID)
	// Parent links were remapped to the new id space.
	assert.Equal(t, "", branch[0].ParentID)
	assert.Equal(t, "copy-e0", branch[1].ParentID)
	assert.Equal(t, "copy-e1", branch[2].ParentID)
	// Content is preserved.
	assert.Equal(t, "content-e0", branch[0].Content)

	// Branch metadata was recorded for the clone.
	meta, ok := tree.BranchMetaFor("copy")
	require.True(t, ok)
	assert.Equal(t, "copy", meta.BranchID)
	assert.Equal(t, "e2", meta.ParentID)
	assert.Equal(t, "copy-e2", meta.BaseLeafID)
	assert.False(t, meta.CreatedAt.IsZero())

	// The original branch is untouched.
	orig, err := tree.GetBranch(ctx, "e2")
	require.NoError(t, err)
	require.Len(t, orig, 3)
	assert.Equal(t, "e0", orig[0].ID)
}

// TestClone_DeepCopyIndependence verifies that the cloned entries are distinct
// objects; appending to the clone does not alter the original branch.
func TestClone_DeepCopyIndependence(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	tree := newConcreteTree()
	require.NoError(t, tree.Append(ctx, newTestEntry("r", "", EntryTypeUser)))
	require.NoError(t, tree.Append(ctx, newTestEntry("a", "r", EntryTypeUser)))
	require.NoError(t, tree.MoveTo(ctx, "a"))

	require.NoError(t, tree.Clone(ctx, "a", "dup"))

	// Append a new entry only to the cloned branch.
	require.NoError(t, tree.Append(ctx, newTestEntry("dup-extra", "dup-a", EntryTypeUser)))
	require.NoError(t, tree.MoveTo(ctx, "dup-extra"))

	// The original branch is unchanged.
	orig, err := tree.GetBranch(ctx, "a")
	require.NoError(t, err)
	assert.Len(t, orig, 2)
	assert.Equal(t, "a", orig[len(orig)-1].ID)

	// The cloned branch now has the extra entry.
	clone, err := tree.GetBranch(ctx, "dup-extra")
	require.NoError(t, err)
	assert.Len(t, clone, 3)
	assert.Equal(t, "dup-extra", clone[len(clone)-1].ID)
}

// TestClone_UnknownSource verifies cloning a non-existent leaf errors.
func TestClone_UnknownSource(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := newConcreteTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("e0", "", EntryTypeUser)))
	err := tree.Clone(context.Background(), "nope", "copy")
	require.ErrorIs(t, err, ErrLeafNotFound)
}

// TestClone_CloneOfClone verifies cloning a previously cloned branch works and
// preserves the remapped parent chain.
func TestClone_CloneOfClone(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	tree := newConcreteTree()
	require.NoError(t, tree.Append(ctx, newTestEntry("e0", "", EntryTypeUser)))
	require.NoError(t, tree.Append(ctx, newTestEntry("e1", "e0", EntryTypeUser)))
	require.NoError(t, tree.MoveTo(ctx, "e1"))

	require.NoError(t, tree.Clone(ctx, "e1", "c1"))
	require.NoError(t, tree.Clone(ctx, "c1-e1", "c2"))

	branch, err := tree.GetBranch(ctx, "c2-c1-e1")
	require.NoError(t, err)
	require.Len(t, branch, 2)
	assert.Equal(t, "c2-c1-e0", branch[0].ID)
	assert.Equal(t, "c2-c1-e1", branch[1].ID)
	assert.Equal(t, "c2-c1-e0", branch[1].ParentID)
}

// TestSlashClone verifies the /clone slash command handler clones a branch.
func TestSlashClone(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	tree := newConcreteTree()
	require.NoError(t, tree.Append(ctx, newTestEntry("e0", "", EntryTypeUser)))
	require.NoError(t, tree.Append(ctx, newTestEntry("e1", "e0", EntryTypeUser)))
	require.NoError(t, tree.MoveTo(ctx, "e1"))

	handler := NewSessionSlashHandler(tree, nil)
	out, err := handler.Handle(ctx, SlashCommand{Name: "clone", Args: []string{"e1", "snap"}})
	require.NoError(t, err)
	assert.Contains(t, out, "snap")
	assert.Equal(t, "snap-e1", tree.CurrentLeaf())

	// The clone is reconstructable.
	branch, err := tree.GetBranch(ctx, "snap-e1")
	require.NoError(t, err)
	require.Len(t, branch, 2)
}

// TestSlashClone_MissingArgs verifies /clone without enough args errors.
func TestSlashClone_MissingArgs(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := newConcreteTree()
	handler := NewSessionSlashHandler(tree, nil)
	_, err := handler.Handle(context.Background(), SlashCommand{Name: "clone", Args: []string{"only-one"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires")
}

// TestSlashBranches verifies the /branches slash command lists branches and
// marks the current one.
func TestSlashBranches(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	tree := newConcreteTree()
	require.NoError(t, tree.Append(ctx, newTestEntry("e0", "", EntryTypeUser)))
	require.NoError(t, tree.Append(ctx, newTestEntry("e1", "e0", EntryTypeUser)))
	require.NoError(t, tree.MoveTo(ctx, "e1"))
	require.NoError(t, tree.Branch(ctx, "e1", WithBranchID("alpha")))
	require.NoError(t, tree.Branch(ctx, "e0", WithBranchID("beta")))

	handler := NewSessionSlashHandler(tree, nil)
	out, err := handler.Handle(ctx, SlashCommand{Name: "branches"})
	require.NoError(t, err)
	assert.Contains(t, out, "alpha")
	assert.Contains(t, out, "beta")
}

// TestSlashBranches_Empty verifies /branches on a tree with no branches.
func TestSlashBranches_Empty(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	handler := NewSessionSlashHandler(tree, nil)
	out, err := handler.Handle(context.Background(), SlashCommand{Name: "branches"})
	require.NoError(t, err)
	assert.Contains(t, out, "No branches")
}

// TestSlashSwitch verifies /switch moves to a branch's base leaf and invokes the
// OnResume callback with the rebuilt context.
func TestSlashSwitch(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	tree := newConcreteTree()
	require.NoError(t, tree.Append(ctx, newTestEntry("e0", "", EntryTypeUser)))
	require.NoError(t, tree.Append(ctx, newTestEntry("e1", "e0", EntryTypeUser)))
	require.NoError(t, tree.Append(ctx, newTestEntry("e2", "e1", EntryTypeUser)))
	require.NoError(t, tree.MoveTo(ctx, "e2"))
	// Create a branch at e1.
	require.NoError(t, tree.Branch(ctx, "e1", WithBranchID("early")))
	assert.Equal(t, "e1", tree.CurrentLeaf())
	// Move away to e2 so switch has something to do.
	require.NoError(t, tree.MoveTo(ctx, "e2"))

	var resumed []SessionEntry
	handler := NewSessionSlashHandler(tree, nil)
	handler.OnResume = func(_ context.Context, entries []SessionEntry) error {
		resumed = entries
		return nil
	}

	out, err := handler.Handle(ctx, SlashCommand{Name: "switch", Args: []string{"early"}})
	require.NoError(t, err)
	assert.Contains(t, out, "early")
	assert.Equal(t, "e1", tree.CurrentLeaf())
	// OnResume received the rebuilt context (root to e1).
	require.Len(t, resumed, 2)
	assert.Equal(t, "e0", resumed[0].ID)
	assert.Equal(t, "e1", resumed[1].ID)
}

// TestSlashSwitch_NotFound verifies /switch with an unknown branch errors.
func TestSlashSwitch_NotFound(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := newConcreteTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("e0", "", EntryTypeUser)))

	handler := NewSessionSlashHandler(tree, nil)
	_, err := handler.Handle(context.Background(), SlashCommand{Name: "switch", Args: []string{"ghost"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// TestSlashSwitch_MissingArg verifies /switch with no argument errors.
func TestSlashSwitch_MissingArg(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := newConcreteTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("e0", "", EntryTypeUser)))

	handler := NewSessionSlashHandler(tree, nil)
	_, err := handler.Handle(context.Background(), SlashCommand{Name: "switch"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a branch id")
}

// TestSlashTree_ShowsBranches verifies the enhanced /tree output includes branch
// information.
func TestSlashTree_ShowsBranches(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	tree := newConcreteTree()
	require.NoError(t, tree.Append(ctx, newTestEntry("e0", "", EntryTypeUser)))
	require.NoError(t, tree.Append(ctx, newTestEntry("e1", "e0", EntryTypeUser)))
	require.NoError(t, tree.MoveTo(ctx, "e1"))
	require.NoError(t, tree.Branch(ctx, "e1", WithBranchID("main-line")))

	handler := NewSessionSlashHandler(tree, nil)
	out, err := handler.Handle(ctx, SlashCommand{Name: "tree"})
	require.NoError(t, err)
	assert.Contains(t, out, "1 branches")
	assert.Contains(t, out, "main-line")
	assert.Contains(t, out, "[branch]")
}

// TestSlashClone_NilTree verifies the clone/branches/switch handlers guard
// against a nil tree.
func TestSlashClone_NilTree(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	handler := NewSessionSlashHandler(nil, nil)
	for _, name := range []string{"clone", "branches", "switch"} {
		args := []string{}
		if name == "clone" {
			args = []string{"a", "b"}
		} else if name == "switch" {
			args = []string{"a"}
		}
		_, err := handler.Handle(context.Background(), SlashCommand{Name: name, Args: args})
		require.Error(t, err, "command %q should error on nil tree", name)
		assert.Contains(t, err.Error(), "no session tree configured")
	}
}

// TestClone_Span verifies Clone records a tracing span with the expected
// attributes.
func TestClone_Span(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, exp := tracedCtx(t)
	tree := newConcreteTree()
	require.NoError(t, tree.Append(ctx, newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(ctx, newTestEntry("b", "a", EntryTypeUser)))
	require.NoError(t, tree.MoveTo(ctx, "b"))

	require.NoError(t, tree.Clone(ctx, "b", "cloned"))

	validateSpan(t, exp, "session.branch.clone")
	var foundFrom, foundNew bool
	for _, sp := range exp.Spans() {
		if sp.Name != "session.branch.clone" {
			continue
		}
		for _, attr := range sp.Attributes {
			if attr.Key == "from_branch" {
				assert.Equal(t, "b", attr.Value)
				foundFrom = true
			}
			if attr.Key == "new_branch" {
				assert.Equal(t, "cloned", attr.Value)
				foundNew = true
			}
		}
	}
	assert.True(t, foundFrom, "from_branch attribute missing")
	assert.True(t, foundNew, "new_branch attribute missing")
}
