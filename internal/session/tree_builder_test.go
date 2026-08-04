package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestTreeBuilder_EmptyStore(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	store := NewJSONLSessionStore(path)
	require.NoError(t, store.Open(context.Background()))
	defer func() { _ = store.Close() }()

	builder := NewDefaultSessionTreeBuilder()
	tree, err := builder.BuildFromStore(context.Background(), store)
	require.NoError(t, err)
	assert.Equal(t, "", tree.CurrentLeaf())
}

func TestTreeBuilder_MissingFile(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	path := filepath.Join(t.TempDir(), "does-not-exist.jsonl")
	store := NewJSONLSessionStore(path)
	defer func() { _ = store.Close() }()

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
	defer func() { _ = reopened.Close() }()

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

func TestTreeBuilder_CorruptedEntry(t *testing.T) {
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

	// Re-open from disk; the store skips corrupted lines during load.
	reopened := NewJSONLSessionStore(path)
	require.NoError(t, reopened.Open(context.Background()))
	defer func() { _ = reopened.Close() }()

	builder := NewDefaultSessionTreeBuilder()
	tree, err := builder.BuildFromStore(context.Background(), reopened)
	require.NoError(t, err)

	// The corrupted entry is skipped; valid entries are present.
	branch, err := tree.GetBranch(context.Background(), "b")
	require.NoError(t, err)
	require.Len(t, branch, 2)
	assert.Equal(t, "a", branch[0].ID)
	assert.Equal(t, "b", branch[1].ID)
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
	defer func() { _ = reopened.Close() }()

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
	defer func() { _ = reopened.Close() }()

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
	defer func() { _ = reopened.Close() }()

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
