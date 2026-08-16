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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// jsonlScannerMaxBuffer bounds the maximum length of a single JSONL line while
// loading, guarding against a corrupt line exhausting memory.
const jsonlScannerMaxBuffer = 1024 * 1024 // 1 MiB

// filePerm is the permission bits used when creating a session store file.
const filePerm = 0o600

// dirPerm is the permission bits used when creating store directories.
const dirPerm = 0o755

// sessionPreviewMaxRunes is the maximum number of runes shown in a session
// list preview.
const sessionPreviewMaxRunes = 80

// sessionTimestampFmt is the timestamp format embedded in per-session
// filenames. It is lexicographically sortable.
const sessionTimestampFmt = "20060102-150405"

// SessionMeta holds metadata about a stored session file, used by the
// /resume list selector.
type SessionMeta struct {
	// ID is the session ID parsed from the filename.
	ID string
	// Filename is the base filename (e.g. "20060102-150405-abc123.jsonl").
	Filename string
	// FilePath is the full path to the session file.
	FilePath string
	// Timestamp is the session creation time parsed from the filename.
	Timestamp time.Time
	// Preview is the first user message content, truncated for display.
	Preview string
	// EntryCount is the number of entries in the session file.
	EntryCount int
}

// SessionLister is an optional interface implemented by stores that can
// enumerate available session files. The /resume slash command uses it to
// list candidate sessions for selection.
type SessionLister interface {
	ListSessions(ctx context.Context) ([]SessionMeta, error)
}

// JSONLSessionStore is a file-backed SessionStore. Every entry is persisted as
// one JSON object per line (JSONL) using append-only writes, and existing
// entries are lazily loaded into memory on first use.
//
// The store supports two modes:
//   - Legacy (single-file): the path points to a .jsonl file. All entries
//     for all sessions are appended to this single file.
//   - Directory: the path points to a directory. Each session gets its own
//     file under <storeDir>/sessions/<timestamp>-<id>.jsonl. This mode is
//     auto-detected when the path is (or will be) a directory.
type JSONLSessionStore struct {
	mu      sync.Mutex
	path    string // current file path (session file in dirMode, original path otherwise)
	entries map[string]*SessionEntry
	file    *os.File
	bw      *bufio.Writer // buffered writer wrapping file to batch write syscalls
	loaded  bool

	// dirMode fields (zero-valued in legacy mode)
	dirMode   bool   // true when the store manages per-session files
	storeDir  string // the store directory (only set in dirMode)
	sessionID string // current session ID (only set in dirMode)
}

var _ SessionStore = (*JSONLSessionStore)(nil)
var _ SessionLister = (*JSONLSessionStore)(nil)

// NewJSONLSessionStore returns a file-backed store for the given path. The file
// is not touched until Append/Get/Save is first called, or Open is used
// explicitly to preload existing entries.
//
// When path is (or will be) a directory, the store operates in directory mode:
// each session is stored as an independent file under
// <path>/sessions/<timestamp>-<id>.jsonl. Otherwise, the path is treated as a
// single JSONL file (legacy mode).
func NewJSONLSessionStore(path string) *JSONLSessionStore {
	s := &JSONLSessionStore{
		path:    path,
		entries: make(map[string]*SessionEntry),
	}
	// Auto-detect directory mode.
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		s.dirMode = true
		s.storeDir = path
	} else if err != nil && os.IsNotExist(err) {
		// Path doesn't exist yet. If it has no file extension, treat it
		// as a directory so per-session files are used.
		if filepath.Ext(path) == "" {
			s.dirMode = true
			s.storeDir = path
		}
	}
	return s
}

// FilePath returns the backing file path. In directory mode this is the
// current session file; in legacy mode it is the original path.
func (s *JSONLSessionStore) FilePath() string { return s.path }

// DirMode reports whether the store is operating in per-session directory mode.
func (s *JSONLSessionStore) DirMode() bool { return s.dirMode }

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

// SetSessionID configures the current session for directory-mode stores. When
// newSession is true, a fresh session file is created and the in-memory entry
// cache is cleared. When newSession is false, the store keeps using the
// session file loaded during Open (the resume path).
//
// In legacy (single-file) mode this method is a no-op.
func (s *JSONLSessionStore) SetSessionID(id string, newSession bool) error {
	if !s.dirMode {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !newSession {
		// Resume: keep the current session file (loaded during Open).
		s.sessionID = id
		return nil
	}

	// New session: close the current file, clear entries, create a new file.
	if err := s.closeFileLocked(); err != nil {
		return fmt.Errorf("session: close previous session file: %w", err)
	}
	s.entries = make(map[string]*SessionEntry)

	s.sessionID = id
	filename := fmt.Sprintf("%s-%s.jsonl", time.Now().UTC().Format(sessionTimestampFmt), id)
	s.path = filepath.Join(s.storeDir, "sessions", filename)

	// Ensure the sessions subdirectory exists before creating the file.
	sessionsDir := filepath.Join(s.storeDir, "sessions")
	if err := os.MkdirAll(sessionsDir, dirPerm); err != nil {
		return fmt.Errorf("session: create sessions dir: %w", err)
	}

	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return fmt.Errorf("session: create session file: %w", err)
	}
	if err := flockExclusive(f); err != nil {
		f.Close()
		return fmt.Errorf("session: lock session file: %w", err)
	}
	s.file = f
	s.bw = bufio.NewWriter(f)
	s.loaded = true
	return nil
}

// ListSessions returns metadata for all session files in the store directory,
// sorted by timestamp descending (most recent first). In legacy mode it
// returns nil.
func (s *JSONLSessionStore) ListSessions(ctx context.Context) ([]SessionMeta, error) {
	if !s.dirMode {
		return nil, nil
	}

	sessionsDir := filepath.Join(s.storeDir, "sessions")
	dirEntries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: read sessions dir: %w", err)
	}

	var metas []SessionMeta
	for _, de := range dirEntries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".jsonl") {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		fullPath := filepath.Join(sessionsDir, de.Name())
		meta := SessionMeta{
			Filename:  de.Name(),
			FilePath:  fullPath,
			Timestamp: info.ModTime(),
		}
		// Parse ID from filename: <ts>-<id>.jsonl. Use the file's ModTime
		// (sub-second precision) for sorting rather than the filename
		// timestamp (second-level precision) so sessions created within the
		// same second are ordered correctly.
		name := strings.TrimSuffix(de.Name(), ".jsonl")
		parts := strings.SplitN(name, "-", 3)
		if len(parts) >= 3 {
			meta.ID = parts[2]
		}
		meta.Preview, meta.EntryCount = readSessionPreview(fullPath)
		metas = append(metas, meta)
	}

	sort.Slice(metas, func(i, j int) bool {
		return metas[i].Timestamp.After(metas[j].Timestamp)
	})
	return metas, nil
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
	if err := s.writeJSONLine(cp); err != nil {
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
		if s.bw != nil {
			if err := s.bw.Flush(); err != nil {
				span.SetStatus(tracing.SpanStatusError, err.Error())
				logger.Error("session_flush", "op", "session.flush", "error_type", "flush_failed", "err", err)
				return fmt.Errorf("session: flush store file: %w", err)
			}
		}
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
	return s.closeFileLocked()
}

// closeFileLocked releases the flock and closes the file. Caller must hold
// s.mu.
func (s *JSONLSessionStore) closeFileLocked() error {
	if s.file == nil {
		return nil
	}
	if s.bw != nil {
		_ = s.bw.Flush() //nolint:errcheck // best-effort flush before close
	}
	_ = flockUnlock(s.file) //nolint:errcheck // best-effort unlock
	err := s.file.Close()
	s.file = nil
	s.bw = nil
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

	// In directory mode, create the directory structure and find the most
	// recent session file (for potential resume).
	if s.dirMode {
		if err := s.dirModeInitLocked(); err != nil {
			return err
		}
	}

	// If path still points to the store directory (no session file found),
	// mark as loaded without opening a file. SetSessionID or Append will
	// create one when needed.
	if s.dirMode && s.path == s.storeDir {
		s.loaded = true
		return nil
	}

	if err := s.loadEntriesLocked(); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return fmt.Errorf("session: open store file: %w", err)
	}
	// Acquire advisory flock to prevent concurrent writes from other
	// processes corrupting the file.
	if err := flockExclusive(f); err != nil {
		f.Close()
		return fmt.Errorf("session: lock store file: %w", err)
	}
	s.file = f
	s.bw = bufio.NewWriter(f)
	s.loaded = true
	return nil
}

// dirModeInitLocked creates the sessions subdirectory and finds the most
// recent session file for potential resume. Caller must hold s.mu.
func (s *JSONLSessionStore) dirModeInitLocked() error {
	sessionsDir := filepath.Join(s.storeDir, "sessions")
	if err := os.MkdirAll(sessionsDir, dirPerm); err != nil {
		return fmt.Errorf("session: create sessions dir: %w", err)
	}
	mostRecent, err := s.findMostRecentSessionLocked()
	if err != nil {
		return err
	}
	if mostRecent != "" {
		s.path = mostRecent
	}
	// If no session found, s.path stays as s.storeDir. The file will be
	// created by SetSessionID or on first Append.
	return nil
}

// findMostRecentSessionLocked scans the sessions directory and returns the
// full path of the most recently modified .jsonl file, or "" when none exist.
// Caller must hold s.mu.
func (s *JSONLSessionStore) findMostRecentSessionLocked() (string, error) {
	sessionsDir := filepath.Join(s.storeDir, "sessions")
	dirEntries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("session: read sessions dir: %w", err)
	}
	var best string
	var bestTime time.Time
	for _, de := range dirEntries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".jsonl") {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		if best == "" || info.ModTime().After(bestTime) {
			best = filepath.Join(sessionsDir, de.Name())
			bestTime = info.ModTime()
		}
	}
	return best, nil
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
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e SessionEntry
		if err := json.Unmarshal(line, &e); err != nil {
			slog.Warn("session.jsonl.corrupt_line", "line", lineNum, "err", err)
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

// writeJSONLine encodes entry as a single JSONL line written to the buffered
// writer. Caller must hold s.mu.
func (s *JSONLSessionStore) writeJSONLine(entry *SessionEntry) error {
	if s.bw == nil {
		return errors.New("session: store writer is not open")
	}
	return json.NewEncoder(s.bw).Encode(entry)
}

// readSessionPreview reads the first user-type entry from a session file and
// returns a truncated preview and the total entry count.
func readSessionPreview(path string) (string, int) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0
	}
	defer func() { _ = f.Close() }() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), jsonlScannerMaxBuffer)
	preview := ""
	count := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		count++
		var e SessionEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		if preview == "" && e.Content != "" {
			preview = e.Content
		}
	}
	// Truncate preview to sessionPreviewMaxRunes runes.
	if r := []rune(preview); len(r) > sessionPreviewMaxRunes {
		preview = string(r[:sessionPreviewMaxRunes]) + "..."
	}
	return preview, count
}
