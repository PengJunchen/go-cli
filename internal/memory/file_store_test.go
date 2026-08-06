package memory

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestStore returns a FileMemoryStore backed by a fresh temp file. The store
// is closed automatically when the test finishes.
func newTestStore(t *testing.T) (*FileMemoryStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "memories.jsonl")
	s, err := NewFileMemoryStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestNewFileMemoryStore_NonExistentFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memories.jsonl")
	s, err := NewFileMemoryStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	list, err := s.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, list)

	// The backing file is created on construction.
	_, err = os.Stat(path)
	assert.NoError(t, err)
}

func TestNewFileMemoryStore_EmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memories.jsonl")
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	s, err := NewFileMemoryStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	list, err := s.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestNewFileMemoryStore_OpenError(t *testing.T) {
	// Passing a directory path makes OpenFile fail with EISDIR.
	dir := t.TempDir()
	_, err := NewFileMemoryStore(dir)
	assert.Error(t, err)
}

func TestNewFileMemoryStore_LoadCorruptLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memories.jsonl")
	valid := `{"id":"mem_1","content":"hello","category":"fact","source":"manual","created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-01T00:00:00Z"}` + "\n"
	corrupt := "{not valid json\n"
	require.NoError(t, os.WriteFile(path, []byte(valid+corrupt), 0o600))

	s, err := NewFileMemoryStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	list, err := s.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "mem_1", list[0].ID)
}

func TestAddAndGet(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.Add(ctx, Memory{Content: "prefers dark mode", Category: "preference", Source: "manual"}))

	// Add takes Memory by value, so the generated ID is discovered via List.
	list, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	id := list[0].ID
	assert.NotEmpty(t, id)

	got, err := s.Get(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "prefers dark mode", got.Content)
	assert.Equal(t, "preference", got.Category)
	assert.Equal(t, "manual", got.Source)
	assert.False(t, got.CreatedAt.IsZero())
	assert.False(t, got.UpdatedAt.IsZero())
}

func TestAdd_WithPresetID(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	mem := Memory{ID: "custom-id", Content: "data", Category: "fact", Source: "auto"}
	require.NoError(t, s.Add(ctx, mem))

	got, err := s.Get(ctx, "custom-id")
	require.NoError(t, err)
	assert.Equal(t, "custom-id", got.ID)
	assert.Equal(t, "fact", got.Category)
}

func TestAdd_PreservesTimestamps(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	ts := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, s.Add(ctx, Memory{ID: "ts", Content: "x", CreatedAt: ts, UpdatedAt: ts}))

	got, err := s.Get(ctx, "ts")
	require.NoError(t, err)
	assert.Equal(t, ts, got.CreatedAt)
	assert.Equal(t, ts, got.UpdatedAt)
}

func TestAdd_DuplicateID(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Add(ctx, Memory{ID: "dup", Content: "first"}))
	err := s.Add(ctx, Memory{ID: "dup", Content: "second"})
	assert.Error(t, err)
}

func TestGet_NotFound(t *testing.T) {
	s, _ := newTestStore(t)
	_, err := s.Get(context.Background(), "nope")
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrMemoryNotFound)
}

func TestList_SortedDescByCreatedAt(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	base := time.Now()

	require.NoError(t, s.Add(ctx, Memory{ID: "a", Content: "a", CreatedAt: base.Add(-2 * time.Hour), UpdatedAt: base.Add(-2 * time.Hour)}))
	require.NoError(t, s.Add(ctx, Memory{ID: "b", Content: "b", CreatedAt: base, UpdatedAt: base}))
	require.NoError(t, s.Add(ctx, Memory{ID: "c", Content: "c", CreatedAt: base.Add(-1 * time.Hour), UpdatedAt: base.Add(-1 * time.Hour)}))

	list, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, "b", list[0].ID)
	assert.Equal(t, "c", list[1].ID)
	assert.Equal(t, "a", list[2].ID)
}

func TestList_Empty(t *testing.T) {
	s, _ := newTestStore(t)
	list, err := s.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestDelete(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Add(ctx, Memory{ID: "x", Content: "x"}))

	require.NoError(t, s.Delete(ctx, "x"))
	_, err := s.Get(ctx, "x")
	assert.ErrorIs(t, err, ErrMemoryNotFound)
}

func TestDelete_NotFound(t *testing.T) {
	s, _ := newTestStore(t)
	err := s.Delete(context.Background(), "nope")
	assert.ErrorIs(t, err, ErrMemoryNotFound)
}

func TestDelete_RewritesFile(t *testing.T) {
	s, path := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Add(ctx, Memory{ID: "keep", Content: "keep this"}))
	require.NoError(t, s.Add(ctx, Memory{ID: "drop", Content: "drop this"}))
	require.NoError(t, s.Delete(ctx, "drop"))
	require.NoError(t, s.Close())

	// Reopen and verify only "keep" remains.
	s2, err := NewFileMemoryStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	list, err := s2.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "keep", list[0].ID)
}

func TestSearch_KeywordMatch(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	docs := []Memory{
		{ID: "d1", Content: "The user prefers Go for backend services", Category: "preference"},
		{ID: "d2", Content: "Python is used for data analysis", Category: "fact"},
		{ID: "d3", Content: "Deployments happen on Fridays", Category: "convention"},
	}
	for _, d := range docs {
		require.NoError(t, s.Add(ctx, d))
	}

	// "backend" appears only in d1 (df=1, N=3 => idf>0).
	res, err := s.Search(ctx, "backend", 10)
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, "d1", res[0].ID)
	assert.InDelta(t, 1.0, res[0].Relevance, 1e-9)
}

func TestSearch_RelevanceRanking(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// N=4; "go" in d1 and d2 (df=2 => idf=log(4/3)>0).
	require.NoError(t, s.Add(ctx, Memory{ID: "d1", Content: "go go go"}))                    // tf=3/3=1.0
	require.NoError(t, s.Add(ctx, Memory{ID: "d2", Content: "go something else long"}))      // tf=1/4=0.25
	require.NoError(t, s.Add(ctx, Memory{ID: "d3", Content: "rust programming language"}))
	require.NoError(t, s.Add(ctx, Memory{ID: "d4", Content: "java enterprise beans"}))

	res, err := s.Search(ctx, "go", 10)
	require.NoError(t, err)
	require.Len(t, res, 2)
	assert.Equal(t, "d1", res[0].ID)
	assert.Equal(t, "d2", res[1].ID)
	assert.Greater(t, res[0].Relevance, res[1].Relevance)
	assert.InDelta(t, 1.0, res[0].Relevance, 1e-9)
	assert.True(t, res[1].Relevance > 0 && res[1].Relevance < 1)
}

func TestSearch_PartialMatch(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Add(ctx, Memory{ID: "d1", Content: "the user likes go"}))
	require.NoError(t, s.Add(ctx, Memory{ID: "d2", Content: "the user likes rust"}))
	require.NoError(t, s.Add(ctx, Memory{ID: "d3", Content: "the user likes java"}))

	// "go" matches d1 (df=1, idf>0); "nonsenseword" matches nothing (df=0).
	res, err := s.Search(ctx, "go nonsenseword", 10)
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, "d1", res[0].ID)
}

func TestSearch_NoMatch(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Add(ctx, Memory{ID: "d1", Content: "alpha beta"}))
	require.NoError(t, s.Add(ctx, Memory{ID: "d2", Content: "gamma delta"}))
	require.NoError(t, s.Add(ctx, Memory{ID: "d3", Content: "epsilon zeta"}))

	res, err := s.Search(ctx, "nonexistent", 10)
	require.NoError(t, err)
	assert.Empty(t, res)
}

func TestSearch_EmptyQuery(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Add(ctx, Memory{ID: "d1", Content: "some content"}))

	res, err := s.Search(ctx, "", 10)
	require.NoError(t, err)
	assert.Empty(t, res)

	// No alphanumeric tokens.
	res, err = s.Search(ctx, "!!! ???", 10)
	require.NoError(t, err)
	assert.Empty(t, res)
}

func TestSearch_EmptyStore(t *testing.T) {
	s, _ := newTestStore(t)
	res, err := s.Search(context.Background(), "anything", 10)
	require.NoError(t, err)
	assert.Empty(t, res)
}

func TestSearch_Limit(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	// N=5; "kotlin" in d1,d2,d3 (df=3 => idf=log(5/4)>0).
	for _, id := range []string{"d1", "d2", "d3"} {
		require.NoError(t, s.Add(ctx, Memory{ID: id, Content: "kotlin stuff"}))
	}
	require.NoError(t, s.Add(ctx, Memory{ID: "d4", Content: "other thing"}))
	require.NoError(t, s.Add(ctx, Memory{ID: "d5", Content: "another thing"}))

	res, err := s.Search(ctx, "kotlin", 2)
	require.NoError(t, err)
	assert.Len(t, res, 2)
}

func TestSearch_ZeroOrNegativeLimit(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Add(ctx, Memory{ID: "d1", Content: "alpha"}))
	require.NoError(t, s.Add(ctx, Memory{ID: "d2", Content: "beta"}))
	require.NoError(t, s.Add(ctx, Memory{ID: "d3", Content: "gamma"}))

	res, err := s.Search(ctx, "alpha", 0)
	require.NoError(t, err)
	assert.Empty(t, res)

	res, err = s.Search(ctx, "alpha", -1)
	require.NoError(t, err)
	assert.Empty(t, res)
}

func TestSearch_CommonTermNotDiscriminating(t *testing.T) {
	// N=3, "shared" in 2 of 3 docs (df=2 => idf=log(3/3)=0). All matching
	// documents score 0, so none are returned.
	s, _ := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Add(ctx, Memory{ID: "d1", Content: "shared content here"}))
	require.NoError(t, s.Add(ctx, Memory{ID: "d2", Content: "shared other text"}))
	require.NoError(t, s.Add(ctx, Memory{ID: "d3", Content: "unique words only"}))

	res, err := s.Search(ctx, "shared", 10)
	require.NoError(t, err)
	assert.Empty(t, res)
}

func TestPersistence_Reload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memories.jsonl")
	ctx := context.Background()

	s1, err := NewFileMemoryStore(path)
	require.NoError(t, err)
	require.NoError(t, s1.Add(ctx, Memory{ID: "p1", Content: "alpha memory one"}))
	require.NoError(t, s1.Add(ctx, Memory{ID: "p2", Content: "gamma memory two"}))
	require.NoError(t, s1.Add(ctx, Memory{ID: "p3", Content: "epsilon memory three"}))
	require.NoError(t, s1.Close())

	s2, err := NewFileMemoryStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	list, err := s2.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 3)

	got, err := s2.Get(ctx, "p1")
	require.NoError(t, err)
	assert.Equal(t, "alpha memory one", got.Content)

	// Search index is rebuilt on reload.
	res, err := s2.Search(ctx, "alpha", 10)
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, "p1", res[0].ID)
}

func TestConcurrentWrites(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	const n = 50
	errs := make(chan error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			errs <- s.Add(ctx, Memory{Content: "concurrent memory content"})
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		assert.NoError(t, err)
	}

	list, err := s.List(ctx)
	require.NoError(t, err)
	assert.Len(t, list, n)
}

func TestClose(t *testing.T) {
	s, _ := newTestStore(t)
	assert.NoError(t, s.Close())
}

func TestClose_Twice(t *testing.T) {
	s, _ := newTestStore(t)
	require.NoError(t, s.Close())
	assert.NoError(t, s.Close()) // idempotent
}
