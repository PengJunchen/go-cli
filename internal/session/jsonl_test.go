package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestJSONL_RoundTrip(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	path := filepath.Join(t.TempDir(), "session.jsonl")

	entries := []*SessionEntry{
		newTestEntry("a", "", EntryTypeUser),
		newTestEntry("b", "a", EntryTypeAssistant),
		newTestEntry("c", "b", EntryTypeTool),
	}

	// Write phase.
	store := NewJSONLSessionStore(path)
	for _, e := range entries {
		require.NoError(t, store.Append(context.Background(), e))
	}
	require.NoError(t, store.Save(context.Background()))
	require.NoError(t, store.Close())

	// Re-open and read back.
	reopened := NewJSONLSessionStore(path)
	require.NoError(t, reopened.Open(context.Background()))
	for _, e := range entries {
		got, err := reopened.Get(context.Background(), e.ID)
		require.NoError(t, err)
		assert.Equal(t, e.Type, got.Type)
		assert.Equal(t, e.Content, got.Content)
		assert.Equal(t, e.ParentID, got.ParentID)
	}
	require.NoError(t, reopened.Close())
}

func TestJSONL_LazyLoadGetFirstUse(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	path := filepath.Join(t.TempDir(), "session.jsonl")

	store := NewJSONLSessionStore(path)
	require.NoError(t, store.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, store.Close())

	// A fresh store loads entries lazily on first Get without an explicit Open.
	fresh := NewJSONLSessionStore(path)
	got, err := fresh.Get(context.Background(), "a")
	require.NoError(t, err)
	assert.Equal(t, "content-a", got.Content)
	require.NoError(t, fresh.Close())
}

func TestJSONL_AppendNoOverwriteAcrossReopen(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	path := filepath.Join(t.TempDir(), "session.jsonl")

	store := NewJSONLSessionStore(path)
	require.NoError(t, store.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.Error(t, store.Append(context.Background(), newTestEntry("a", "", EntryTypeAssistant)))
	require.NoError(t, store.Close())

	reopened := NewJSONLSessionStore(path)
	require.NoError(t, reopened.Open(context.Background()))
	require.Error(t, reopened.Append(context.Background(), newTestEntry("a", "", EntryTypeAssistant)))
	got, err := reopened.Get(context.Background(), "a")
	require.NoError(t, err)
	assert.Equal(t, EntryTypeUser, got.Type)
	require.NoError(t, reopened.Close())
}

func TestJSONL_GetNotFound(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	store := NewJSONLSessionStore(path)
	got, err := store.Get(context.Background(), "missing")
	require.ErrorIs(t, err, ErrNotFound)
	assert.Nil(t, got)
	require.NoError(t, store.Close())
}

func TestJSONL_OpenCreatesFile(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	path := filepath.Join(t.TempDir(), "new-session.jsonl")
	require.NoError(t, NewJSONLSessionStore(path).Open(context.Background()))

	_, err := os.Stat(path)
	require.NoError(t, err)
}

func TestJSONL_PersistsContentsOnDisk(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	store := NewJSONLSessionStore(path)
	require.NoError(t, store.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, store.Close())

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "content-a")
}

func TestJSONL_ConcurrentAppend(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	store := NewJSONLSessionStore(path)
	defer func() { require.NoError(t, store.Close()) }()

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("conc-%d", i)
			require.NoError(t, store.Append(context.Background(), newTestEntry(id, "", EntryTypeUser)))
			got, err := store.Get(context.Background(), id)
			require.NoError(t, err)
			assert.Equal(t, id, got.ID)
		}(i)
	}
	wg.Wait()
}

func TestJSONL_TraceSpans(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, exp := tracedCtx(t)
	path := filepath.Join(t.TempDir(), "session.jsonl")
	store := NewJSONLSessionStore(path)
	require.NoError(t, store.Open(ctx))
	require.NoError(t, store.Append(ctx, newTestEntry("a", "", EntryTypeUser)))
	_, err := store.Get(ctx, "a")
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return exp.SpanCount() >= 2
	}, 2*time.Second, 5*time.Millisecond)

	validateSpan(t, exp, "session.open")
	validateSpan(t, exp, "session.save")
	validateSpan(t, exp, "session.load")
}

// --- Directory mode tests ---

func TestJSONL_DirectoryMode_SetSessionID(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	storeDir := t.TempDir()
	store := NewJSONLSessionStore(storeDir)
	require.True(t, store.DirMode(), "store should be in directory mode")

	require.NoError(t, store.SetSessionID("sess-abc", true))
	require.NoError(t, store.Append(context.Background(), newTestEntry("e1", "", EntryTypeUser)))
	require.NoError(t, store.Append(context.Background(), newTestEntry("e2", "e1", EntryTypeAssistant)))
	require.NoError(t, store.Save(context.Background()))
	require.NoError(t, store.Close())

	entries, err := os.ReadDir(filepath.Join(storeDir, "sessions"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.True(t, strings.HasSuffix(entries[0].Name(), "-sess-abc.jsonl"))
}

func TestJSONL_DirectoryMode_ListSessions(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	storeDir := t.TempDir()

	store1 := NewJSONLSessionStore(storeDir)
	require.NoError(t, store1.SetSessionID("sess-first", true))
	require.NoError(t, store1.Append(context.Background(), &SessionEntry{
		ID: "a1", Type: EntryTypeUser, Content: "first message",
		Timestamp: time.Now(),
	}))
	require.NoError(t, store1.Save(context.Background()))
	require.NoError(t, store1.Close())

	time.Sleep(10 * time.Millisecond)

	store2 := NewJSONLSessionStore(storeDir)
	require.NoError(t, store2.SetSessionID("sess-second", true))
	require.NoError(t, store2.Append(context.Background(), &SessionEntry{
		ID: "b1", Type: EntryTypeUser, Content: "second message",
		Timestamp: time.Now(),
	}))
	require.NoError(t, store2.Save(context.Background()))
	require.NoError(t, store2.Close())

	store3 := NewJSONLSessionStore(storeDir)
	metas, err := store3.ListSessions(context.Background())
	require.NoError(t, err)
	require.Len(t, metas, 2)

	assert.Equal(t, "sess-second", metas[0].ID)
	assert.Contains(t, metas[0].Preview, "second message")
	assert.Equal(t, "sess-first", metas[1].ID)
	assert.Contains(t, metas[1].Preview, "first message")
	assert.GreaterOrEqual(t, metas[0].EntryCount, 1)
	assert.GreaterOrEqual(t, metas[1].EntryCount, 1)

	require.NoError(t, store3.Close())
}

func TestJSONL_DirectoryMode_ResumeKeepsSessionFile(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	storeDir := t.TempDir()

	store1 := NewJSONLSessionStore(storeDir)
	require.NoError(t, store1.SetSessionID("sess-resume", true))
	require.NoError(t, store1.Append(context.Background(), newTestEntry("r1", "", EntryTypeUser)))
	require.NoError(t, store1.Append(context.Background(), newTestEntry("r2", "r1", EntryTypeAssistant)))
	require.NoError(t, store1.Save(context.Background()))
	require.NoError(t, store1.Close())

	store2 := NewJSONLSessionStore(storeDir)
	require.NoError(t, store2.Open(context.Background()))
	require.NoError(t, store2.SetSessionID("sess-resume", false))

	got, err := store2.Get(context.Background(), "r1")
	require.NoError(t, err)
	assert.Equal(t, "content-r1", got.Content)

	require.NoError(t, store2.Append(context.Background(), newTestEntry("r3", "r2", EntryTypeUser)))
	require.NoError(t, store2.Save(context.Background()))
	require.NoError(t, store2.Close())

	entries, err := os.ReadDir(filepath.Join(storeDir, "sessions"))
	require.NoError(t, err)
	require.Len(t, entries, 1, "resume should not create a new session file")
}

func TestJSONL_DirectoryMode_LegacyFileNotAffected(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	store := NewJSONLSessionStore(path)
	assert.False(t, store.DirMode(), "store with .jsonl path should be in legacy mode")

	require.NoError(t, store.SetSessionID("ignored", true))
	require.NoError(t, store.Append(context.Background(), newTestEntry("x1", "", EntryTypeUser)))
	require.NoError(t, store.Close())

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "content-x1")

	_, err = os.Stat(filepath.Join(filepath.Dir(path), "sessions"))
	assert.True(t, os.IsNotExist(err), "legacy mode should not create sessions dir")
}

func TestJSONL_DirectoryMode_EntryIDNoConflict(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	storeDir := t.TempDir()

	store1 := NewJSONLSessionStore(storeDir)
	require.NoError(t, store1.SetSessionID("sess-conflict", true))
	require.NoError(t, store1.Append(context.Background(), newTestEntry("entry-0", "", EntryTypeUser)))
	require.NoError(t, store1.Append(context.Background(), newTestEntry("entry-1", "entry-0", EntryTypeAssistant)))
	require.NoError(t, store1.Save(context.Background()))
	require.NoError(t, store1.Close())

	store2 := NewJSONLSessionStore(storeDir)
	require.NoError(t, store2.Open(context.Background()))
	require.NoError(t, store2.SetSessionID("sess-conflict", false))

	err := store2.Append(context.Background(), newTestEntry("entry-0", "", EntryTypeUser))
	require.Error(t, err, "duplicate entry ID should be rejected")

	require.NoError(t, store2.Append(context.Background(), newTestEntry("entry-2", "entry-1", EntryTypeUser)))
	require.NoError(t, store2.Save(context.Background()))
	require.NoError(t, store2.Close())
}

// --- Buffered write tests ---

func TestJSONLSessionStore_BufferedWritePersistence(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	store := NewJSONLSessionStore(path)

	const n = 100
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("entry-%d", i)
		require.NoError(t, store.Append(context.Background(), newTestEntry(id, "", EntryTypeUser)))
	}
	require.NoError(t, store.Save(context.Background()))
	require.NoError(t, store.Close())

	reopened := NewJSONLSessionStore(path)
	require.NoError(t, reopened.Open(context.Background()))
	entries, err := reopened.List(context.Background())
	require.NoError(t, err)
	assert.Len(t, entries, n)
	for _, e := range entries {
		got, err := reopened.Get(context.Background(), e.ID)
		require.NoError(t, err)
		assert.Equal(t, "content-"+e.ID, got.Content)
	}
	require.NoError(t, reopened.Close())
}

func TestJSONLSessionStore_CloseFlushesBuffer(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	store := NewJSONLSessionStore(path)

	// Append records WITHOUT calling Save — Close must flush the buffer.
	const n = 20
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("close-%d", i)
		require.NoError(t, store.Append(context.Background(), newTestEntry(id, "", EntryTypeUser)))
	}
	require.NoError(t, store.Close())

	// Reopen and verify all data persisted even though Save was never called.
	reopened := NewJSONLSessionStore(path)
	require.NoError(t, reopened.Open(context.Background()))
	entries, err := reopened.List(context.Background())
	require.NoError(t, err)
	assert.Len(t, entries, n)
	require.NoError(t, reopened.Close())
}

func TestJSONLSessionStore_DirModeBufferedWrite(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	storeDir := t.TempDir()
	store := NewJSONLSessionStore(storeDir)
	require.True(t, store.DirMode())

	require.NoError(t, store.SetSessionID("sess-buf", true))

	const n = 50
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("dir-%d", i)
		require.NoError(t, store.Append(context.Background(), newTestEntry(id, "", EntryTypeUser)))
	}
	require.NoError(t, store.Save(context.Background()))
	require.NoError(t, store.Close())

	// Reopen in dir mode and verify.
	reopened := NewJSONLSessionStore(storeDir)
	require.NoError(t, reopened.Open(context.Background()))
	require.NoError(t, reopened.SetSessionID("sess-buf", false))
	entries, err := reopened.List(context.Background())
	require.NoError(t, err)
	assert.Len(t, entries, n)
	for _, e := range entries {
		got, err := reopened.Get(context.Background(), e.ID)
		require.NoError(t, err)
		assert.Equal(t, "content-"+e.ID, got.Content)
	}
	require.NoError(t, reopened.Close())
}

func TestJSONLSessionStore_BufferedWriteRace(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	store := NewJSONLSessionStore(path)
	defer func() { require.NoError(t, store.Close()) }()

	const numWriters = 10
	const writesPerGoroutine = 20
	var wg sync.WaitGroup
	wg.Add(numWriters + 1)

	// Writers: concurrent Append calls.
	for w := 0; w < numWriters; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < writesPerGoroutine; i++ {
				id := fmt.Sprintf("race-%d-%d", w, i)
				require.NoError(t, store.Append(context.Background(), newTestEntry(id, "", EntryTypeUser)))
			}
		}(w)
	}

	// Saver: concurrent Save calls while writers are appending.
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			_ = store.Save(context.Background()) //nolint:errcheck // best-effort flush during race test
		}
	}()

	wg.Wait()

	// Verify no corruption: every appended entry should be readable.
	require.NoError(t, store.Save(context.Background()))
	entries, err := store.List(context.Background())
	require.NoError(t, err)
	expected := numWriters * writesPerGoroutine
	assert.Len(t, entries, expected)
}

func BenchmarkJSONLSessionStore_BatchAppend(b *testing.B) {
	const n = 100

	b.Run("buffered", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			path := filepath.Join(b.TempDir(), "session.jsonl")
			store := NewJSONLSessionStore(path)
			b.StartTimer()

			for j := 0; j < n; j++ {
				_ = store.Append(context.Background(), &SessionEntry{
					ID:        fmt.Sprintf("e-%d", j),
					Type:      EntryTypeUser,
					Content:   "benchmark content",
					Timestamp: time.Now(),
				})
			}
			_ = store.Save(context.Background())

			b.StopTimer()
			_ = store.Close()
			b.StartTimer()
		}
	})

	b.Run("flush_each", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			path := filepath.Join(b.TempDir(), "session.jsonl")
			store := NewJSONLSessionStore(path)
			b.StartTimer()

			for j := 0; j < n; j++ {
				_ = store.Append(context.Background(), &SessionEntry{
					ID:        fmt.Sprintf("e-%d", j),
					Type:      EntryTypeUser,
					Content:   "benchmark content",
					Timestamp: time.Now(),
				})
				_ = store.Save(context.Background())
			}

			b.StopTimer()
			_ = store.Close()
			b.StartTimer()
		}
	})
}
