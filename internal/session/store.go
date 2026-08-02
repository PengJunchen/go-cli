package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// ErrNotFound is returned by SessionStore.Get when no entry with the given id
// has been appended.
var ErrNotFound = errors.New("session: entry not found")

// MemoryStore is an entirely in-memory SessionStore. It is safe for concurrent
// use and never overwrites an existing entry id (append-only semantics).
type MemoryStore struct {
	mu      sync.RWMutex
	entries map[string]*SessionEntry
}

var _ SessionStore = (*MemoryStore)(nil)

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() SessionStore {
	return &MemoryStore{entries: make(map[string]*SessionEntry)}
}

// Append inserts the entry only if its id is not already present.
func (s *MemoryStore) Append(ctx context.Context, entry *SessionEntry) error {
	span, _ := tracing.SpanFromContext(ctx, "session.save", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())

	if entry == nil {
		return errors.New("session: nil entry")
	}
	if entry.ID == "" {
		return errors.New("session: entry id is required")
	}
	if entry.Type == "" {
		return errors.New("session: entry type is required")
	}
	span.SetAttributes(
		tracing.Attribute{Key: "entry_type", Value: string(entry.Type)},
		tracing.Attribute{Key: "entry_id", Value: entry.ID},
	)

	cp := entry.clone()
	s.mu.Lock()
	if _, exists := s.entries[cp.ID]; exists {
		s.mu.Unlock()
		span.SetStatus(tracing.SpanStatusError, "duplicate entry")
		logger.Error("session_save", "op", "session.save", "error_type", "duplicate_entry", "entry_type", string(cp.Type), "entry_id", cp.ID)
		return fmt.Errorf("session: entry %q already exists", cp.ID)
	}
	s.entries[cp.ID] = cp
	s.mu.Unlock()

	logger.Info("session_save", "op", "session.save", "entry_type", string(cp.Type), "entry_id", cp.ID)
	span.SetStatus(tracing.SpanStatusOK, "")
	return nil
}

// Get returns a defensive copy of the entry with the given id, or ErrNotFound.
func (s *MemoryStore) Get(ctx context.Context, id string) (*SessionEntry, error) {
	span, _ := tracing.SpanFromContext(ctx, "session.load", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())
	span.SetAttributes(tracing.Attribute{Key: "entry_id", Value: id})

	s.mu.RLock()
	e, ok := s.entries[id]
	s.mu.RUnlock()
	if !ok {
		span.SetStatus(tracing.SpanStatusError, "entry not found")
		logger.Error("session_load", "op", "session.load", "error_type", "not_found", "entry_id", id)
		return nil, ErrNotFound
	}
	logger.Info("session_load", "op", "session.load", "entry_id", id, "entry_type", string(e.Type))
	span.SetStatus(tracing.SpanStatusOK, "")
	return e.clone(), nil
}

// Save is a no-op for the in-memory store; entries are immediately available.
func (s *MemoryStore) Save(ctx context.Context) error {
	span, _ := tracing.SpanFromContext(ctx, "session.flush", tracing.SpanKindInternal)
	defer span.End()
	tracing.NewTraceLogger(span, slog.Default()).Info("session_flush", "op", "session.flush")
	return nil
}

// DefaultSessionStore is the default in-memory SessionStore. It is an alias of
// MemoryStore so callers may refer to either name interchangeably.
type DefaultSessionStore = MemoryStore

var _ SessionStore = (*DefaultSessionStore)(nil)

// NewDefaultSessionStore returns the default in-memory session store.
func NewDefaultSessionStore() SessionStore {
	return NewMemoryStore()
}
