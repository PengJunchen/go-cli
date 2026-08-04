package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// jsonlScannerMaxBuffer bounds the maximum length of a single JSONL line while
// loading, guarding against a corrupt line exhausting memory.
const jsonlScannerMaxBuffer = 1024 * 1024 // 1 MiB

// filePerm is the permission bits used when creating a session store file.
const filePerm = 0o600

// JSONLSessionStore is a file-backed SessionStore. Every entry is persisted as
// one JSON object per line (JSONL) using append-only writes, and existing
// entries are lazily loaded into memory on first use.
type JSONLSessionStore struct {
	mu      sync.Mutex
	path    string
	entries map[string]*SessionEntry
	file    *os.File
	loaded  bool
}

var _ SessionStore = (*JSONLSessionStore)(nil)

// NewJSONLSessionStore returns a file-backed store for the given path. The file
// is not touched until Append/Get/Save is first called, or Open is used
// explicitly to preload existing entries.
func NewJSONLSessionStore(path string) *JSONLSessionStore {
	return &JSONLSessionStore{
		path:    path,
		entries: make(map[string]*SessionEntry),
	}
}

// FilePath returns the backing file path.
func (s *JSONLSessionStore) FilePath() string { return s.path }

// Open lazily loads existing entries from the file into memory and opens the
// file for append-only writes. It is idempotent.
func (s *JSONLSessionStore) Open(ctx context.Context) error {
	span, _ := tracing.SpanFromContext(ctx, "session.open", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())
	span.SetAttributes(tracing.Attribute{Key: "path", Value: s.path})

	if err := s.ensureLoaded(); err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		logger.Error("session_open", "op", "session.open", "error_type", "load_failed", "path", s.path, "err", err)
		return err
	}
	span.SetAttributes(tracing.Attribute{Key: "entry_count", Value: s.count()})
	logger.Info("session_open", "op", "session.open", "path", s.path, "entry_count", s.count())
	span.SetStatus(tracing.SpanStatusOK, "")
	return nil
}

// Append persists the entry as a new JSONL line and adds it to memory. It never
// overwrites an existing id.
func (s *JSONLSessionStore) Append(ctx context.Context, entry *SessionEntry) error {
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

	if err := s.ensureLoaded(); err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		logger.Error("session_save", "op", "session.save", "error_type", "load_failed", "entry_id", entry.ID, "err", err)
		return err
	}

	cp := entry.clone()
	s.mu.Lock()
	if _, exists := s.entries[cp.ID]; exists {
		s.mu.Unlock()
		span.SetStatus(tracing.SpanStatusError, "duplicate entry")
		logger.Error("session_save", "op", "session.save", "error_type", "duplicate_entry", "entry_type", string(cp.Type), "entry_id", cp.ID)
		return fmt.Errorf("session: entry %q already exists", cp.ID)
	}
	if err := writeJSONLine(s.file, cp); err != nil {
		s.mu.Unlock()
		span.SetStatus(tracing.SpanStatusError, err.Error())
		logger.Error("session_save", "op", "session.save", "error_type", "write_failed", "entry_id", cp.ID, "err", err)
		return fmt.Errorf("session: write entry: %w", err)
	}
	s.entries[cp.ID] = cp
	s.mu.Unlock()

	logger.Info("session_save", "op", "session.save", "entry_type", string(cp.Type), "entry_id", cp.ID)
	span.SetStatus(tracing.SpanStatusOK, "")
	return nil
}

// Get returns a defensive copy of the entry with the given id, or ErrNotFound.
func (s *JSONLSessionStore) Get(ctx context.Context, id string) (*SessionEntry, error) {
	span, _ := tracing.SpanFromContext(ctx, "session.load", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())
	span.SetAttributes(tracing.Attribute{Key: "entry_id", Value: id})

	if err := s.ensureLoaded(); err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return nil, err
	}
	s.mu.Lock()
	e, ok := s.entries[id]
	s.mu.Unlock()
	if !ok {
		span.SetStatus(tracing.SpanStatusError, "entry not found")
		logger.Error("session_load", "op", "session.load", "error_type", "not_found", "entry_id", id)
		return nil, ErrNotFound
	}
	logger.Info("session_load", "op", "session.load", "entry_id", id, "entry_type", string(e.Type))
	span.SetStatus(tracing.SpanStatusOK, "")
	return e.clone(), nil
}

// List returns defensive copies of all stored entries sorted by timestamp.
func (s *JSONLSessionStore) List(ctx context.Context) ([]*SessionEntry, error) {
	if err := s.ensureLoaded(); err != nil {
		return nil, fmt.Errorf("session: list entries: %w", err)
	}
	s.mu.Lock()
	entries := make([]*SessionEntry, 0, len(s.entries))
	for _, e := range s.entries {
		entries = append(entries, e.clone())
	}
	s.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Timestamp.Before(entries[j].Timestamp)
	})
	return entries, nil
}

// Save flushes buffered bytes to the backing file.
func (s *JSONLSessionStore) Save(ctx context.Context) error {
	span, _ := tracing.SpanFromContext(ctx, "session.flush", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())

	if err := s.ensureLoaded(); err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		if err := s.file.Sync(); err != nil {
			span.SetStatus(tracing.SpanStatusError, err.Error())
			logger.Error("session_flush", "op", "session.flush", "error_type", "sync_failed", "err", err)
			return fmt.Errorf("session: flush store file: %w", err)
		}
	}
	logger.Info("session_flush", "op", "session.flush", "path", s.path)
	span.SetStatus(tracing.SpanStatusOK, "")
	return nil
}

// Close closes the backing file. It is safe to call multiple times and returns
// nil when the file is already closed.
func (s *JSONLSessionStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

func (s *JSONLSessionStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// ensureLoaded opens the backing file and loads existing entries on first use.
func (s *JSONLSessionStore) ensureLoaded() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return nil
	}
	if err := s.loadEntriesLocked(); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return fmt.Errorf("session: open store file: %w", err)
	}
	s.file = f
	s.loaded = true
	return nil
}

// loadEntriesLocked reads existing JSONL lines into memory. It must be called
// with s.mu held.
func (s *JSONLSessionStore) loadEntriesLocked() error {
	f, err := os.Open(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // brand-new store; nothing to load
		}
		return fmt.Errorf("session: open store file for reading: %w", err)
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // read-only best-effort close.

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), jsonlScannerMaxBuffer)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e SessionEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // skip corrupt lines, keep the rest.
		}
		if _, exists := s.entries[e.ID]; !exists {
			s.entries[e.ID] = e.clone()
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("session: read store file: %w", err)
	}
	return nil
}

// writeJSONLine encodes entry as a single JSONL line written to f.
func writeJSONLine(f *os.File, entry *SessionEntry) error {
	if f == nil {
		return errors.New("session: store file is not open")
	}
	return json.NewEncoder(f).Encode(entry)
}
