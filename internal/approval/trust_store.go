package approval

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/pengjunchen/go-cli/internal/privatefile"
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
	// ManifestVersion is zero for legacy path-only trust entries. Such entries
	// remain readable but cannot authorize project executable sources.
	ManifestVersion int `json:"manifest_version,omitempty"`
	// CanonicalRoot is the canonical workspace root bound to the source manifest.
	CanonicalRoot string `json:"canonical_root,omitempty"`
	// ProjectSources records every captured file source, including absent files.
	ProjectSources []ProjectSourceRecord `json:"project_sources,omitempty"`
	// ProjectSourceDirectories records each tracked directory and its sorted
	// captured membership, including absent directories.
	ProjectSourceDirectories []ProjectSourceDirectoryRecord `json:"project_source_directories,omitempty"`
	// ManifestDigest is the digest of the complete source and membership set.
	ManifestDigest string `json:"manifest_digest,omitempty"`
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

// FileTrustStore is a JSON-file-backed TrustStore. Writes use a private sibling
// file and atomic replace, so a crash never leaves a half-written trust file.
// It is safe for concurrent use.
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

// Save writes the entries atomically through a private sibling file.
func (s *FileTrustStore) Save(entries map[string]TrustEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Defensive copy so json.Marshal does not race with caller mutations.
	copied := make(map[string]TrustEntry, len(entries))
	for k, v := range entries {
		copied[k] = cloneTrustEntry(v)
	}
	return s.writeLocked(copied)
}

// Add upserts a single entry without touching the rest of the store.
func (s *FileTrustStore) Add(path string, entry TrustEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := s.loadLocked()
	if err != nil {
		return err
	}
	entries[path] = cloneTrustEntry(entry)
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
	if _, err := os.Lstat(s.path); err != nil {
		if os.IsNotExist(err) {
			return make(map[string]TrustEntry), nil
		}
		return nil, err
	}
	data, err := readPrivateTrustFile(s.path)
	if err != nil {
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

// readPrivateTrustFile returns bytes only from a current-user-owned, private
// regular file in a current-user-owned, non-replaceable parent directory. The
// opened file is checked against the path before reading to reject symlinks
// and replacement races.
func readPrivateTrustFile(path string) ([]byte, error) {
	parent := filepath.Dir(path)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return nil, fmt.Errorf("approval: inspect trust store parent: %w", err)
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return nil, fmt.Errorf("approval: trust store parent %s must be a directory", parent)
	}
	if parentInfo.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("approval: trust store parent %s is accessible by group or others", parent)
	}
	if err := validateFileOwner(parentInfo, parent); err != nil {
		return nil, fmt.Errorf("approval: insecure trust store parent: %w", err)
	}

	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("approval: inspect trust store file: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("approval: trust store %s must be a regular file", path)
	}
	if pathInfo.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("approval: trust store %s has permissions %o, expected 0600", path, pathInfo.Mode().Perm())
	}
	if err := validateFileOwner(pathInfo, path); err != nil {
		return nil, fmt.Errorf("approval: insecure trust store file: %w", err)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("approval: open trust store: %w", err)
	}
	openedInfo, statErr := file.Stat()
	currentInfo, pathErr := os.Lstat(path)
	if statErr != nil || pathErr != nil {
		_ = file.Close()
		if statErr != nil {
			return nil, fmt.Errorf("approval: stat opened trust store: %w", statErr)
		}
		return nil, fmt.Errorf("approval: restat trust store: %w", pathErr)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) || !os.SameFile(openedInfo, currentInfo) {
		_ = file.Close()
		return nil, fmt.Errorf("approval: trust store %s changed while opening", path)
	}
	if openedInfo.Mode().Perm() != 0o600 {
		_ = file.Close()
		return nil, fmt.Errorf("approval: trust store %s has permissions %o, expected 0600", path, openedInfo.Mode().Perm())
	}
	if err := validateFileOwner(openedInfo, path); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("approval: insecure opened trust store: %w", err)
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("approval: read trust store: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("approval: close trust store: %w", closeErr)
	}
	return data, nil
}

// writeLocked persists entries atomically. Callers must hold s.mu.
func (s *FileTrustStore) writeLocked(entries map[string]TrustEntry) error {
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	return privatefile.WriteAtomic(s.path, data)
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
		entries[path] = cloneTrustEntry(entry)
	}
	return entries, nil
}

// Save replaces the entire set of entries.
func (s *InMemoryTrustStore) Save(entries map[string]TrustEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = make(map[string]TrustEntry, len(entries))
	for k, v := range entries {
		s.entries[k] = cloneTrustEntry(v)
	}
	return nil
}

// Add upserts a single entry under its path.
func (s *InMemoryTrustStore) Add(path string, entry TrustEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[path] = cloneTrustEntry(entry)
	return nil
}

func cloneTrustEntry(entry TrustEntry) TrustEntry {
	entry.ProjectSources = append([]ProjectSourceRecord(nil), entry.ProjectSources...)
	entry.ProjectSourceDirectories = cloneDirectoryRecords(entry.ProjectSourceDirectories)
	return entry
}

// Remove deletes the entry under path (no-op when absent).
func (s *InMemoryTrustStore) Remove(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, path)
	return nil
}
