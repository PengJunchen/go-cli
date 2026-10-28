package memory_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/memory"
)

func TestMemoryIndependentHandlesPreserveAddDeleteAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.jsonl")
	first, err := memory.NewFileMemoryStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	require.NoError(t, first.Add(t.Context(), memory.Memory{ID: "old", Content: "old"}))
	second, err := memory.NewFileMemoryStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	require.NoError(t, first.Add(t.Context(), memory.Memory{ID: "new", Content: "new"}))
	require.NoError(t, second.Delete(t.Context(), "old"))
	require.NoError(t, first.Add(t.Context(), memory.Memory{ID: "later", Content: "later"}))
	third, err := memory.NewFileMemoryStore(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, third.Close()) }()
	items, err := third.List(t.Context())
	require.NoError(t, err)
	require.Len(t, items, 2)
	_, err = third.Get(t.Context(), "new")
	require.NoError(t, err)
	_, err = third.Get(t.Context(), "later")
	require.NoError(t, err)
}
