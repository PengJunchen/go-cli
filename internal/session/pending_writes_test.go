package session

import (
	"context"
	"fmt"
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

// countingStore wraps a SessionStore and counts Append calls. It is used to
// verify that PendingSessionWrites batches multiple entries into a single
// flush rather than writing each one individually.
type countingStore struct {
	SessionStore
	appendCount int
	mu          sync.Mutex
}

func (c *countingStore) Append(ctx context.Context, entry *SessionEntry) error {
	c.mu.Lock()
	c.appendCount++
	c.mu.Unlock()
	return c.SessionStore.Append(ctx, entry)
}

func (c *countingStore) AppendCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.appendCount
}

// TestPendingSessionWrites_BatchBuffering verifies that multiple Enqueue calls
// do not trigger any store writes, and that a single Flush writes all buffered
// entries in one batch. This is the core batching guarantee: entries are
// buffered in memory and only hit the store when Flush is called.
func TestPendingSessionWrites_BatchBuffering(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	pw := NewPendingSessionWrites()
	store := &countingStore{SessionStore: NewMemoryStore()}

	const n = 5
	// Enqueue multiple entries — none should reach the store yet.
	for i := 0; i < n; i++ {
		pw.Enqueue(SessionWrite{
			SessionID: "sess1",
			Entry: SessionEntry{
				ID:      fmt.Sprintf("e%d", i),
				Type:    EntryTypeUser,
				Content: fmt.Sprintf("content %d", i),
			},
		})
	}

	// Verify entries are buffered, not yet in the store.
	assert.Equal(t, n, pw.PendingCount(), "all entries should be buffered")
	assert.Equal(t, 0, pw.FlushedCount(), "nothing should be flushed yet")
	assert.Equal(t, 0, store.AppendCount(), "store should have zero Append calls before flush")

	// Verify none of the entries are in the store yet.
	for i := 0; i < n; i++ {
		_, err := store.Get(context.Background(), fmt.Sprintf("e%d", i))
		assert.ErrorIs(t, err, ErrNotFound, "entry should not be in store before flush")
	}

	// Flush — all entries are written in a single batch.
	require.NoError(t, pw.Flush(context.Background(), store))

	assert.Equal(t, 0, pw.PendingCount(), "buffer should be empty after flush")
	assert.Equal(t, n, pw.FlushedCount(), "all entries should be flushed")
	assert.Equal(t, n, store.AppendCount(), "store should have received all entries via Append")

	// Verify all entries are now in the store with correct content.
	for i := 0; i < n; i++ {
		got, err := store.Get(context.Background(), fmt.Sprintf("e%d", i))
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("content %d", i), got.Content)
	}
}

// TestPendingSessionWrites_PersistencePatternSimulatesReplSession verifies
// that the persistSession pattern (enqueue user + assistant entries, then a
// single Flush) results in exactly one batch flush with both entries written
// together — not two individual store writes.
func TestPendingSessionWrites_PersistencePatternSimulatesReplSession(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	pw := NewPendingSessionWrites()
	store := &countingStore{SessionStore: NewMemoryStore()}

	// Simulate persistSession: enqueue user entry, then assistant entry,
	// then flush once.
	pw.Enqueue(SessionWrite{
		SessionID: "sess-repl",
		Entry: SessionEntry{
			ID:      "entry-0",
			Type:    EntryTypeUser,
			Content: "user message",
		},
	})
	pw.Enqueue(SessionWrite{
		SessionID: "sess-repl",
		Entry: SessionEntry{
			ID:      "entry-1",
			Type:    EntryTypeAssistant,
			Content: "assistant response",
		},
	})

	// Before flush: two entries buffered, zero store writes.
	assert.Equal(t, 2, pw.PendingCount())
	assert.Equal(t, 0, store.AppendCount())

	// Single flush writes both entries in one batch.
	require.NoError(t, pw.Flush(context.Background(), store))
	assert.Equal(t, 0, pw.PendingCount())
	assert.Equal(t, 2, pw.FlushedCount())
	assert.Equal(t, 2, store.AppendCount(), "both entries written via single flush")

	// Verify both entries are in the store.
	got, err := store.Get(context.Background(), "entry-0")
	require.NoError(t, err)
	assert.Equal(t, "user message", got.Content)
	assert.Equal(t, EntryTypeUser, got.Type)

	got, err = store.Get(context.Background(), "entry-1")
	require.NoError(t, err)
	assert.Equal(t, "assistant response", got.Content)
	assert.Equal(t, EntryTypeAssistant, got.Type)
}
