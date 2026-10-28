package memory_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/memory"
)

func populatedSnapshotStore(t testing.TB, count int) *memory.FileMemoryStore {
	t.Helper()
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	for i := 0; i < count; i++ {
		require.NoError(t, encoder.Encode(memory.Memory{ID: fmt.Sprintf("fixture-%04d", i), Content: "fixture memory search baseline", Category: "fact"}))
	}
	path := filepath.Join(t.TempDir(), "memory.jsonl")
	require.NoError(t, os.WriteFile(path, data.Bytes(), 0600))
	store, err := memory.NewFileMemoryStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func TestMemoryUnchangedSnapshotReadAndOwnAppendHaveBoundedAllocations(t *testing.T) {
	for _, operation := range []string{"List", "Add"} {
		t.Run(operation, func(t *testing.T) {
			store := populatedSnapshotStore(t, 1000)
			var operationErr error
			allocations := testing.AllocsPerRun(3, func() {
				if operation == "List" {
					_, operationErr = store.List(t.Context())
				} else {
					operationErr = store.Add(t.Context(), memory.Memory{Content: "new memory", Category: "fact"})
				}
			})
			require.NoError(t, operationErr)
			t.Logf("%s allocations per operation at1000 records: %.0f", operation, allocations)
			limit := float64(100)
			want := 1000
			if operation == "Add" {
				limit = 300
				want += 4
			}
			require.Less(t, allocations, limit, "acknowledged unchanged state must reuse the decoded index")
			items, err := store.List(t.Context())
			require.NoError(t, err)
			require.Len(t, items, want)
		})
	}
}

func TestMemorySnapshotRefreshesSameSizeRewriteWithChangedTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.jsonl")
	store, err := memory.NewFileMemoryStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	require.NoError(t, store.Add(t.Context(), memory.Memory{ID: "stable", Content: "old"}))
	original, err := os.Stat(path)
	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	changed := bytes.Replace(raw, []byte(`"content":"old"`), []byte(`"content":"new"`), 1)
	require.NotEqual(t, raw, changed)
	require.NoError(t, os.WriteFile(path, changed, 0600))
	require.NoError(t, os.Chtimes(path, original.ModTime().Add(2*time.Second), original.ModTime().Add(2*time.Second)))
	current, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(original, current))
	require.Equal(t, original.Size(), current.Size())
	item, err := store.Get(t.Context(), "stable")
	require.NoError(t, err)
	require.Equal(t, "new", item.Content)
}

func TestMemorySnapshotRefreshesReplacementWithSameSizeAndTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.jsonl")
	store, err := memory.NewFileMemoryStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	require.NoError(t, store.Add(t.Context(), memory.Memory{ID: "stable", Content: "old"}))
	original, err := os.Stat(path)
	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	changed := bytes.Replace(raw, []byte(`"content":"old"`), []byte(`"content":"new"`), 1)
	require.NotEqual(t, raw, changed)
	replacement := path + ".replacement"
	require.NoError(t, os.WriteFile(replacement, changed, 0600))
	require.NoError(t, os.Chtimes(replacement, original.ModTime(), original.ModTime()))
	require.NoError(t, os.Rename(replacement, path))
	current, err := os.Stat(path)
	require.NoError(t, err)
	require.False(t, os.SameFile(original, current))
	require.Equal(t, original.Size(), current.Size())
	require.True(t, original.ModTime().Equal(current.ModTime()))
	item, err := store.Get(t.Context(), "stable")
	require.NoError(t, err)
	require.Equal(t, "new", item.Content)
	require.NoError(t, store.Add(t.Context(), memory.Memory{ID: "after", Content: "append after replacement"}))
	reopened, err := memory.NewFileMemoryStore(path)
	require.NoError(t, err)
	defer reopened.Close()
	items, err := reopened.List(t.Context())
	require.NoError(t, err)
	require.Len(t, items, 2)
}

func TestMemorySnapshotRefreshesOtherHandleExtractionAndDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.jsonl")
	first, err := memory.NewFileMemoryStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	require.NoError(t, first.Add(t.Context(), memory.Memory{ID: "seed", Content: "seed"}))
	second, err := memory.NewFileMemoryStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	source := memory.Provenance{WorkspaceID: "workspace", SessionID: "session", SourceIDs: []string{"turn"}}
	published, err := second.PublishExtraction(t.Context(), source, []memory.Memory{{Content: "fresh extracted fact"}}, nil)
	require.NoError(t, err)
	require.True(t, published)
	items, err := first.List(t.Context())
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.NoError(t, first.Delete(t.Context(), "seed"))
	items, err = second.List(t.Context())
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, "fresh extracted fact", items[0].Content)
	source.SourceIDs = []string{"zero-fact-turn"}
	published, err = first.PublishExtraction(t.Context(), source, nil, nil)
	require.NoError(t, err)
	require.True(t, published)
	processed, err := second.Processed(t.Context(), source)
	require.NoError(t, err)
	require.True(t, processed)
}

func BenchmarkMemorySnapshot(b *testing.B) {
	for _, operation := range []string{"List", "Search", "Add"} {
		b.Run(operation, func(b *testing.B) {
			store := populatedSnapshotStore(b, 1000)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				switch operation {
				case "List":
					if _, err := store.List(b.Context()); err != nil {
						b.Fatal(err)
					}
				case "Search":
					if _, err := store.Search(b.Context(), "fixture", 10); err != nil {
						b.Fatal(err)
					}
				case "Add":
					if err := store.Add(b.Context(), memory.Memory{ID: fmt.Sprintf("append-%d", i), Content: "new bounded memory", Category: "fact"}); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
