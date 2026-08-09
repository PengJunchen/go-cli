package session

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestPendingSessionWrites_EnqueueAndCount(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	pw := NewPendingSessionWrites()
	assert.Equal(t, 0, pw.PendingCount())

	pw.Enqueue(SessionWrite{
		SessionID: "sess1",
		Entry:     SessionEntry{ID: "e1", Type: EntryTypeUser, Content: "hello"},
	})
	pw.Enqueue(SessionWrite{
		SessionID: "sess1",
		Entry:     SessionEntry{ID: "e2", Type: EntryTypeAssistant, Content: "world"},
	})

	assert.Equal(t, 2, pw.PendingCount())
	assert.Equal(t, 0, pw.FlushedCount())
}

func TestPendingSessionWrites_Flush(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	pw := NewPendingSessionWrites()
	store := NewMemoryStore()

	pw.Enqueue(SessionWrite{
		SessionID: "sess1",
		Entry:     SessionEntry{ID: "e1", Type: EntryTypeUser, Content: "hello"},
	})
	pw.Enqueue(SessionWrite{
		SessionID: "sess1",
		Entry:     SessionEntry{ID: "e2", Type: EntryTypeAssistant, Content: "world"},
	})

	require.NoError(t, pw.Flush(context.Background(), store))
	assert.Equal(t, 0, pw.PendingCount(), "buffer should be empty after flush")
	assert.Equal(t, 2, pw.FlushedCount())

	got, err := store.Get(context.Background(), "e1")
	require.NoError(t, err)
	assert.Equal(t, "hello", got.Content)

	got, err = store.Get(context.Background(), "e2")
	require.NoError(t, err)
	assert.Equal(t, "world", got.Content)
}

func TestPendingSessionWrites_FlushNilStore(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	pw := NewPendingSessionWrites()
	pw.Enqueue(SessionWrite{
		SessionID: "sess1",
		Entry:     SessionEntry{ID: "e1", Type: EntryTypeUser, Content: "hello"},
	})
	err := pw.Flush(context.Background(), nil)
	require.Error(t, err)
	assert.Equal(t, 1, pw.PendingCount(), "buffer should be preserved on error")
}

func TestPendingSessionWrites_FlushDuplicatePreservesBuffer(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	pw := NewPendingSessionWrites()
	store := NewMemoryStore()

	// Pre-seed the store so the flush will hit a duplicate.
	require.NoError(t, store.Append(context.Background(), &SessionEntry{ID: "e1", Type: EntryTypeUser, Content: "pre-existing"}))

	pw.Enqueue(SessionWrite{
		SessionID: "sess1",
		Entry:     SessionEntry{ID: "e1", Type: EntryTypeUser, Content: "hello"},
	})

	err := pw.Flush(context.Background(), store)
	require.Error(t, err, "duplicate append should cause flush error")
	assert.Equal(t, 1, pw.PendingCount(), "buffer should be preserved on error")
}

func TestPendingSessionWrites_CreateSavepointAndFlushToSavepoint(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	pw := NewPendingSessionWrites()
	store := NewMemoryStore()

	pw.Enqueue(SessionWrite{
		SessionID: "sess1",
		Entry:     SessionEntry{ID: "e1", Type: EntryTypeUser, Content: "first"},
	})
	pw.Enqueue(SessionWrite{
		SessionID: "sess1",
		Entry:     SessionEntry{ID: "e2", Type: EntryTypeUser, Content: "second"},
	})

	sp := pw.CreateSavepoint()
	assert.Equal(t, 2, pw.PendingCount())

	// Enqueue more after the savepoint.
	pw.Enqueue(SessionWrite{
		SessionID: "sess1",
		Entry:     SessionEntry{ID: "e3", Type: EntryTypeUser, Content: "third"},
	})
	assert.Equal(t, 3, pw.PendingCount())

	// Flush only up to the savepoint.
	require.NoError(t, pw.FlushToSavepoint(context.Background(), sp, store))
	assert.Equal(t, 1, pw.PendingCount(), "only post-savepoint items should remain")
	assert.Equal(t, 2, pw.FlushedCount())

	// The first two entries should be in the store.
	_, err := store.Get(context.Background(), "e1")
	require.NoError(t, err)
	_, err = store.Get(context.Background(), "e2")
	require.NoError(t, err)

	// The third entry should not yet be in the store.
	_, err = store.Get(context.Background(), "e3")
	require.ErrorIs(t, err, ErrNotFound)

	// Now flush the remaining.
	require.NoError(t, pw.Flush(context.Background(), store))
	assert.Equal(t, 0, pw.PendingCount())
	_, err = store.Get(context.Background(), "e3")
	require.NoError(t, err)
}

func TestPendingSessionWrites_FlushToSavepointNilStore(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	pw := NewPendingSessionWrites()
	pw.Enqueue(SessionWrite{
		SessionID: "sess1",
		Entry:     SessionEntry{ID: "e1", Type: EntryTypeUser, Content: "hello"},
	})
	sp := pw.CreateSavepoint()
	err := pw.FlushToSavepoint(context.Background(), sp, nil)
	require.Error(t, err)
}

func TestPendingSessionWrites_Concurrent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	pw := NewPendingSessionWrites()
	store := NewMemoryStore()

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			pw.Enqueue(SessionWrite{
				SessionID: "sess1",
				Entry: SessionEntry{
					ID:      "e" + itoa(i),
					Type:    EntryTypeUser,
					Content: "content",
				},
			})
		}(i)
	}
	wg.Wait()

	assert.Equal(t, n, pw.PendingCount())

	require.NoError(t, pw.Flush(context.Background(), store))
	assert.Equal(t, 0, pw.PendingCount())
	assert.Equal(t, n, pw.FlushedCount())
}
