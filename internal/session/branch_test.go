package session

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// newConcreteTree returns a *DefaultSessionTree for tests that need access to
// concrete-only methods not exposed on the SessionTree interface.
func newConcreteTree() *DefaultSessionTree {
	tree, ok := NewDefaultSessionTree().(*DefaultSessionTree)
	if !ok {
		panic("NewDefaultSessionTree() should return *DefaultSessionTree")
	}
	return tree
}

func TestBranch_ZeroCopyEntryCountUnchanged(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := newConcreteTree()
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("e%d", i)
		parent := ""
		if i > 0 {
			parent = fmt.Sprintf("e%d", i-1)
		}
		require.NoError(t, tree.Append(context.Background(), newTestEntry(id, parent, EntryTypeUser)))
	}
	before := tree.EntryCount()
	assert.Equal(t, 10, before)

	// Branch zero-copy onto an internal node: no entries are copied or added.
	require.NoError(t, tree.Branch(context.Background(), "e5"))
	assert.Equal(t, before, tree.EntryCount())
	assert.Equal(t, "e5", tree.CurrentLeaf())
}

func TestBranch_LeafIdAndBranchRecovery(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := newConcreteTree()
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("e%d", i)
		parent := ""
		if i > 0 {
			parent = fmt.Sprintf("e%d", i-1)
		}
		require.NoError(t, tree.Append(context.Background(), newTestEntry(id, parent, EntryTypeUser)))
	}
	require.NoError(t, tree.Append(context.Background(), newTestEntry("tail", "e9", EntryTypeUser)))
	// Advance the current leaf to the newest entry via MoveTo (Append only sets
	// the leaf when the tree is empty).
	require.NoError(t, tree.MoveTo(context.Background(), "tail"))
	assert.Equal(t, "tail", tree.CurrentLeaf())

	// Fork onto e5; CurrentLeaf becomes e5 with zero new entries.
	require.NoError(t, tree.Branch(context.Background(), "e5"))
	assert.Equal(t, "e5", tree.CurrentLeaf())
	assert.Equal(t, 11, tree.EntryCount())

	// GetBranch from the new leaf reconstructs root -> e5 correctly.
	branch, err := tree.GetBranch(context.Background(), "e5")
	require.NoError(t, err)
	require.Len(t, branch, 6)
	assert.Equal(t, "e0", branch[0].ID)
	assert.Equal(t, "e5", branch[5].ID)
}

func TestBranch_UnknownFromID(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := newConcreteTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.ErrorIs(t, tree.Branch(context.Background(), "nope"), ErrLeafNotFound)
}

func TestBranch_WithBranchIDAndMeta(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := newConcreteTree()
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("e%d", i)
		parent := ""
		if i > 0 {
			parent = fmt.Sprintf("e%d", i-1)
		}
		require.NoError(t, tree.Append(context.Background(), newTestEntry(id, parent, EntryTypeUser)))
	}

	require.NoError(t, tree.Branch(context.Background(), "e3", WithBranchID("fork-1")))
	assert.Equal(t, "e3", tree.CurrentLeaf())

	meta, ok := tree.BranchMetaFor("fork-1")
	require.True(t, ok)
	assert.Equal(t, "fork-1", meta.BranchID)
	assert.Equal(t, "e3", meta.ParentID)
	assert.Equal(t, "e3", meta.BaseLeafID)
	assert.False(t, meta.CreatedAt.IsZero())
}

func TestBranch_Span(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, exp := tracedCtx(t)
	tree := newConcreteTree()
	require.NoError(t, tree.Append(ctx, newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(ctx, newTestEntry("b", "a", EntryTypeUser)))

	require.NoError(t, tree.Branch(ctx, "a", WithBranchID("b1")))

	validateSpan(t, exp, "session.branch")
	time.Sleep(50 * time.Millisecond)
	exp.AssertSpanExists(t, "session.branch")
}

func TestBranch_Concurrent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := newConcreteTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("root", "", EntryTypeUser)))

	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("c-%d", i)
			require.NoError(t, tree.Append(context.Background(), newTestEntry(id, "root", EntryTypeUser)))
			require.NoError(t, tree.Branch(context.Background(), id))
			branch, err := tree.GetBranch(context.Background(), id)
			require.NoError(t, err)
			require.Equal(t, id, branch[len(branch)-1].ID)
		}(i)
	}
	wg.Wait()
	assert.Equal(t, n+1, tree.EntryCount())
}
