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

func TestPendingSessionWrites_FlushPartialFailureRemovesFlushedEntries(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	pw := NewPendingSessionWrites()
	store := NewMemoryStore()

	// Pre-seed the store with "e2" so it will fail when the flush reaches it.
	require.NoError(t, store.Append(context.Background(), &SessionEntry{ID: "e2", Type: EntryTypeUser, Content: "pre-existing"}))

	// Enqueue e1 (will succeed), e2 (will fail - duplicate), e3 (never reached).
	pw.Enqueue(SessionWrite{
		SessionID: "sess1",
		Entry:     SessionEntry{ID: "e1", Type: EntryTypeUser, Content: "first"},
	})
	pw.Enqueue(SessionWrite{
		SessionID: "sess1",
		Entry:     SessionEntry{ID: "e2", Type: EntryTypeUser, Content: "second"},
	})
	pw.Enqueue(SessionWrite{
		SessionID: "sess1",
		Entry:     SessionEntry{ID: "e3", Type: EntryTypeUser, Content: "third"},
	})

	// First flush should fail on e2 (duplicate).
	err := pw.Flush(context.Background(), store)
	require.Error(t, err, "flush should fail on duplicate e2")

	// e1 was successfully flushed and should be removed from the buffer.
	// e2 and e3 should remain.
	assert.Equal(t, 2, pw.PendingCount(), "only unflushed entries should remain in buffer")
	assert.Equal(t, 1, pw.FlushedCount(), "one entry was successfully flushed before the error")

	// Verify e1 is in the store with the correct content.
	got, err := store.Get(context.Background(), "e1")
	require.NoError(t, err)
	assert.Equal(t, "first", got.Content)

	// Remove the pre-existing e2 so retry can succeed.
	// Use a fresh store to simulate fixing the conflict.
	store2 := NewMemoryStore()
	// Re-add e1 so the new store has it (simulating the already-persisted state).
	require.NoError(t, store2.Append(context.Background(), &SessionEntry{ID: "e1", Type: EntryTypeUser, Content: "first"}))

	// Retry flush — now e2 and e3 should both succeed.
	// But e1 is already in store2, and e1 is NOT in the pending buffer anymore.
	require.NoError(t, pw.Flush(context.Background(), store2))
	assert.Equal(t, 0, pw.PendingCount(), "buffer should be empty after successful retry")
	assert.Equal(t, 3, pw.FlushedCount())

	// All three entries should be in the store.
	_, err = store2.Get(context.Background(), "e2")
	require.NoError(t, err)
	_, err = store2.Get(context.Background(), "e3")
	require.NoError(t, err)
}

func TestPendingSessionWrites_FlushToSavepointPartialFailureRemovesFlushedEntries(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	pw := NewPendingSessionWrites()
	store := NewMemoryStore()

	// Pre-seed the store with "e2" so it will fail when the flush reaches it.
	require.NoError(t, store.Append(context.Background(), &SessionEntry{ID: "e2", Type: EntryTypeUser, Content: "pre-existing"}))

	pw.Enqueue(SessionWrite{
		SessionID: "sess1",
		Entry:     SessionEntry{ID: "e1", Type: EntryTypeUser, Content: "first"},
	})
	pw.Enqueue(SessionWrite{
		SessionID: "sess1",
		Entry:     SessionEntry{ID: "e2", Type: EntryTypeUser, Content: "second"},
	})

	sp := pw.CreateSavepoint()

	// FlushToSavepoint should fail on e2 (duplicate).
	err := pw.FlushToSavepoint(context.Background(), sp, store)
	require.Error(t, err)

	// e1 was successfully flushed, e2 remains.
	assert.Equal(t, 1, pw.PendingCount(), "only unflushed entry should remain")
	assert.Equal(t, 1, pw.FlushedCount())
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
