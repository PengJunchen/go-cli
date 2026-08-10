package session

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// SessionWrite is a single buffered session entry pending persistence.
type SessionWrite struct {
	SessionID string
	Entry     SessionEntry
	Timestamp time.Time
}

// Savepoint marks a position in the pending-writes buffer. Items enqueued at or
// before a savepoint can be flushed independently of later items.
type Savepoint struct {
	// index is the number of items that were pending when the savepoint was
	// created.
	index int
}

// PendingSessionWrites buffers session writes and flushes them at savepoints.
// It is safe for concurrent use.
type PendingSessionWrites struct {
	mu      sync.Mutex
	flushMu sync.Mutex // serializes flush operations to prevent duplicate writes
	pending []SessionWrite
	flushed int
}

// NewPendingSessionWrites returns an empty PendingSessionWrites buffer.
func NewPendingSessionWrites() *PendingSessionWrites {
	return &PendingSessionWrites{}
}

// Enqueue adds a write to the pending buffer. A zero Timestamp is replaced with
// the current time.
func (p *PendingSessionWrites) Enqueue(write SessionWrite) {
	p.mu.Lock()
	if write.Timestamp.IsZero() {
		write.Timestamp = time.Now().UTC()
	}
	p.pending = append(p.pending, write)
	count := len(p.pending)
	p.mu.Unlock()

	slog.Info("session.pending_writes.enqueue",
		"session_id", write.SessionID,
		"entry_id", write.Entry.ID,
		"pending_count", count,
	)
}

// PendingCount returns the number of buffered writes that have not yet been
// flushed.
func (p *PendingSessionWrites) PendingCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pending)
}

// FlushedCount returns the total number of writes that have been successfully
// flushed since the buffer was created.
func (p *PendingSessionWrites) FlushedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.flushed
}

// CreateSavepoint returns a Savepoint capturing the current position of the
// pending buffer. Items enqueued after the savepoint are not affected by a
// FlushToSavepoint call.
func (p *PendingSessionWrites) CreateSavepoint() Savepoint {
	p.mu.Lock()
	defer p.mu.Unlock()
	sp := Savepoint{index: len(p.pending)}
	slog.Debug("session.pending_writes.savepoint", "index", sp.index)
	return sp
}

// Flush writes all pending entries to the store and clears the buffer. On
// success the buffer is empty; on error the buffer is left untouched so the
// caller may retry.
func (p *PendingSessionWrites) Flush(ctx context.Context, store SessionStore) error {
	if store == nil {
		return fmt.Errorf("session: nil store")
	}
	p.flushMu.Lock()
	defer p.flushMu.Unlock()

	p.mu.Lock()
	writes := make([]SessionWrite, len(p.pending))
	copy(writes, p.pending)
	p.mu.Unlock()

	for i, w := range writes {
		entry := w.Entry
		if err := store.Append(ctx, &entry); err != nil {
			slog.Error("session.pending_writes.flush",
				"error_type", "append_failed",
				"entry_id", w.Entry.ID,
				"err", err,
			)
			// Remove entries that were already flushed so they are not
			// re-written on retry. Without this, a partial failure would
			// leave already-persisted entries in the buffer, causing every
			// subsequent retry to fail on the first duplicate.
			p.mu.Lock()
			p.flushed += i
			if i <= len(p.pending) {
				p.pending = p.pending[i:]
			}
			p.mu.Unlock()
			return fmt.Errorf("session: flush entry %q: %w", w.Entry.ID, err)
		}
	}

	p.mu.Lock()
	p.flushed += len(writes)
	// Remove only the flushed prefix; items enqueued concurrently
	// between the two lock acquisitions must be preserved.
	if len(writes) <= len(p.pending) {
		p.pending = p.pending[len(writes):]
	}
	p.mu.Unlock()

	slog.Info("session.pending_writes.flush",
		"flushed_count", len(writes),
	)
	return nil
}

// FlushToSavepoint flushes only the entries that were pending at the time the
// savepoint was created. Entries enqueued after the savepoint remain in the
// buffer.
func (p *PendingSessionWrites) FlushToSavepoint(ctx context.Context, sp Savepoint, store SessionStore) error {
	if store == nil {
		return fmt.Errorf("session: nil store")
	}
	p.flushMu.Lock()
	defer p.flushMu.Unlock()

	p.mu.Lock()
	if sp.index > len(p.pending) {
		sp.index = len(p.pending)
	}
	writes := make([]SessionWrite, sp.index)
	copy(writes, p.pending[:sp.index])
	p.mu.Unlock()

	for i, w := range writes {
		entry := w.Entry
		if err := store.Append(ctx, &entry); err != nil {
			slog.Error("session.pending_writes.flush_to_savepoint",
				"error_type", "append_failed",
				"entry_id", w.Entry.ID,
				"err", err,
			)
			// Remove entries that were already flushed so they are not
			// re-written on retry.
			p.mu.Lock()
			p.flushed += i
			if i <= len(p.pending) {
				p.pending = p.pending[i:]
			}
			p.mu.Unlock()
			return fmt.Errorf("session: flush entry %q: %w", w.Entry.ID, err)
		}
	}

	p.mu.Lock()
	p.flushed += len(writes)
	// Remove the flushed prefix from the pending buffer.
	if sp.index <= len(p.pending) {
		p.pending = p.pending[sp.index:]
	}
	p.mu.Unlock()

	slog.Info("session.pending_writes.flush_to_savepoint",
		"flushed_count", len(writes),
		"remaining", p.PendingCount(),
	)
	return nil
}
