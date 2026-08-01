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

func TestTree_AppendMoveCurrentLeaf(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	assert.Equal(t, "", tree.CurrentLeaf())

	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	assert.Equal(t, "a", tree.CurrentLeaf())

	require.NoError(t, tree.Append(context.Background(), newTestEntry("b", "a", EntryTypeAssistant)))
	require.NoError(t, tree.MoveTo(context.Background(), "b"))
	assert.Equal(t, "b", tree.CurrentLeaf())
}

func TestTree_GetBranchOrder(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	for _, id := range []string{"a", "b", "c", "d"} {
		parent := ""
		if id != "a" {
			parent = string(rune(id[0] - 1))
		}
		require.NoError(t, tree.Append(context.Background(), newTestEntry(id, parent, EntryTypeUser)))
	}

	branch, err := tree.GetBranch(context.Background(), "d")
	require.NoError(t, err)
	require.Len(t, branch, 4)
	assert.Equal(t, "a", branch[0].ID)
	assert.Equal(t, "b", branch[1].ID)
	assert.Equal(t, "c", branch[2].ID)
	assert.Equal(t, "d", branch[3].ID)
}

func TestTree_MoveToUnknownLeaf(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.ErrorIs(t, tree.MoveTo(context.Background(), "nope"), ErrLeafNotFound)
}

func TestTree_GetBranchUnknownLeaf(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	branch, err := tree.GetBranch(context.Background(), "nope")
	require.ErrorIs(t, err, ErrLeafNotFound)
	assert.Nil(t, branch)
}

func TestTree_AppendNoOverwrite(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.Error(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeAssistant)))
}

func TestTree_AppendUnknownParent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	err := tree.Append(context.Background(), newTestEntry("x", "missing-parent", EntryTypeUser))
	require.ErrorIs(t, err, ErrParentNotFound)
}

func TestTree_AppendNilOrInvalid(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	require.Error(t, tree.Append(context.Background(), nil))
	require.Error(t, tree.Append(context.Background(), &SessionEntry{Type: EntryTypeUser}))
	require.Error(t, tree.Append(context.Background(), &SessionEntry{ID: "x"}))
}

func TestTree_BranchBuildContext(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("b", "a", EntryTypeAssistant)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("c1", "b", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("c2", "b", EntryTypeUser)))

	// Both branches of "b" are independently replayable.
	sc1, err := tree.BuildContext(context.Background(), "c1")
	require.NoError(t, err)
	assert.Equal(t, "c1", sc1.LeafID)
	assert.Equal(t, 3, sc1.EntryCount)
	require.Len(t, sc1.Messages, 3)
	assert.Equal(t, "a", sc1.Messages[0].ID)
	assert.Equal(t, "b", sc1.Messages[1].ID)
	assert.Equal(t, "c1", sc1.Messages[2].ID)

	sc2, err := tree.BuildContext(context.Background(), "c2")
	require.NoError(t, err)
	assert.Equal(t, "c2", sc2.LeafID)
	assert.Equal(t, "c2", sc2.Messages[2].ID)

	// MoveTo switches the current branch.
	require.NoError(t, tree.MoveTo(context.Background(), "c2"))
	assert.Equal(t, "c2", tree.CurrentLeaf())
}

func TestTree_BuildContextUnknownLeaf(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	sc, err := tree.BuildContext(context.Background(), "nope")
	require.ErrorIs(t, err, ErrLeafNotFound)
	assert.Nil(t, sc)
}

func TestTree_BuildContextLastUpdate(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("b", "a", EntryTypeUser)))

	sc, err := tree.BuildContext(context.Background(), "b")
	require.NoError(t, err)
	assert.False(t, sc.LastUpdate.IsZero())
}

func TestTree_ConcurrentAppendBranch(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("root", "", EntryTypeUser)))

	const n = 64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			require.NoError(t, tree.Append(context.Background(), newTestEntry(fmt.Sprintf("child-%d", i), "root", EntryTypeUser)))
			_, err := tree.GetBranch(context.Background(), fmt.Sprintf("child-%d", i))
			require.NoError(t, err)
		}(i)
	}
	wg.Wait()
}

func TestTree_TraceSpans(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, exp := tracedCtx(t)
	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(ctx, newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(ctx, newTestEntry("b", "a", EntryTypeUser)))
	_, err := tree.BuildContext(ctx, "b")
	require.NoError(t, err)

	// Spans are exported asynchronously; wait for the expected ones.
	require.Eventually(t, func() bool {
		return exp.SpanCount() >= 2
	}, 2*time.Second, 5*time.Millisecond)

	validateSpan(t, exp, "session.tree.append")
	validateSpan(t, exp, "session.tree.build")
}
