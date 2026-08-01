package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
