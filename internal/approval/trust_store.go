package approval

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// TrustEntry records the trust relationship for a single project path. It is
// persisted by TrustStore implementations so trusted-projects survive process
// restarts.
type TrustEntry struct {
	// Path is the canonical filesystem path of the trusted project.
	Path string `json:"path"`
	// Fingerprint is an optional fingerprint of the path (e.g. its SHA-256),
	// letting a client verify the entry against the original input.
	Fingerprint string `json:"fingerprint,omitempty"`
	// TrustedAt is the RFC3339 timestamp at which the project was trusted.
	TrustedAt string `json:"trusted_at,omitempty"`
	// ExpiresAt is the RFC3339 timestamp (empty meaning never) after which the
	// trust entry must be treated as untrusted.
	ExpiresAt string `json:"expires_at,omitempty"`
}

// TrustStore persists project trust entries keyed by canonical path. It is the
// storage backend behind TrustManager.
type TrustStore interface {
	// Load returns all stored entries keyed by path. A store with no entries
	// (including a missing backing file) returns an empty, non-nil map.
	Load() (map[string]TrustEntry, error)
	// Save replaces the entire set of entries.
	Save(entries map[string]TrustEntry) error
	// Add upserts a single entry under its path.
	Add(path string, entry TrustEntry) error
	// Remove deletes the entry under path (no-op when absent).
	Remove(path string) error
}

// FileTrustStore is a JSON-file-backed TrustStore. Writes are atomic: it writes
// to a temporary file in the same directory and renames it into place, so a
// crash never leaves a half-written trust file. It is safe for concurrent use.
type FileTrustStore struct {
	mu   sync.RWMutex
	path string
}

var _ TrustStore = (*FileTrustStore)(nil)

// NewFileTrustStore creates a FileTrustStore persisting to path.
func NewFileTrustStore(path string) *FileTrustStore {
	return &FileTrustStore{path: path}
}

// Load reads the trust file and returns the parsed entries. A missing file is
// not an error and yields an empty map.
func (s *FileTrustStore) Load() (map[string]TrustEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadLocked()
}

// Save writes the entries atomically: it creates a temp file sibling to the
// destination and renames it over the destination.
func (s *FileTrustStore) Save(entries map[string]TrustEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLocked(entries)
}

// Add upserts a single entry without touching the rest of the store.
func (s *FileTrustStore) Add(path string, entry TrustEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := s.loadLocked()
	if err != nil {
		return err
	}
	entries[path] = entry
	return s.writeLocked(entries)
}

// Remove deletes the entry under path. Absent paths are a no-op.
func (s *FileTrustStore) Remove(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := s.loadLocked()
	if err != nil {
		return err
	}
	delete(entries, path)
	return s.writeLocked(entries)
}

// loadLocked reads and decodes the trust file. Callers must hold the lock. A
// missing file is not an error and yields an empty map.
func (s *FileTrustStore) loadLocked() (map[string]TrustEntry, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]TrustEntry), nil
		}
		return nil, err
	}

	entries := make(map[string]TrustEntry)
	if len(data) == 0 {
		return entries, nil
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// writeLocked persists entries atomically. Callers must hold s.mu.
func (s *FileTrustStore) writeLocked(entries map[string]TrustEntry) error {
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, "trust-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() //nolint:errcheck // cleanup no-op after a successful rename.

	written, err := tmp.Write(data)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		_ = tmp.Close() //nolint:errcheck // write already failed; close is best-effort.
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

// InMemoryTrustStore is an in-memory, concurrency-safe TrustStore. It is used
// as the default backend when no persistent store is configured.
type InMemoryTrustStore struct {
	mu      sync.RWMutex
	entries map[string]TrustEntry
}

var _ TrustStore = (*InMemoryTrustStore)(nil)

// NewInMemoryTrustStore creates an empty in-memory trust store.
func NewInMemoryTrustStore() *InMemoryTrustStore {
	return &InMemoryTrustStore{entries: make(map[string]TrustEntry)}
}

// Load returns a copy of all stored entries.
func (s *InMemoryTrustStore) Load() (map[string]TrustEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries := make(map[string]TrustEntry, len(s.entries))
	for path, entry := range s.entries {
		entries[path] = entry
	}
	return entries, nil
}

// Save replaces the entire set of entries.
func (s *InMemoryTrustStore) Save(entries map[string]TrustEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = entries
	return nil
}

// Add upserts a single entry under its path.
func (s *InMemoryTrustStore) Add(path string, entry TrustEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[path] = entry
	return nil
}

// Remove deletes the entry under path (no-op when absent).
func (s *InMemoryTrustStore) Remove(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, path)
	return nil
}
