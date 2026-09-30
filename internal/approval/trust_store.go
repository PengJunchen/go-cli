package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

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
	// TrustGeneration is the monotonically increasing project trust epoch.
	TrustGeneration uint64 `json:"trust_generation,omitempty"`
	// Revoked marks a generation-bearing tombstone that fences older capabilities.
	Revoked bool `json:"revoked,omitempty"`
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

// TransactionalTrustStore exposes a short read-modify-write transaction over
// the current trust snapshot. The callback must not retain entries or call
// public TrustStore methods recursively.
type TransactionalTrustStore interface {
	Update(context.Context, func(entries map[string]TrustEntry) (dirty bool, err error)) error
}

var (
	errTrustGenerationInvalid    = errors.New("approval: invalid trust generation record")
	errTrustGenerationOverflow   = errors.New("approval: trust generation overflow")
	errTrustGenerationRegression = errors.New("approval: trust generation cannot move backwards")
	errTrustGenerationMutation   = errors.New("approval: trust record cannot change without advancing its generation")
)

// FileTrustStore is a JSON-file-backed TrustStore. Writes use a private sibling
// file and atomic replace, so a crash never leaves a half-written trust file.
// It is safe for concurrent use.
type FileTrustStore struct {
	mu           sync.Mutex
	path         string
	pathOnce     sync.Once
	canonical    string
	canonicalErr error
}

var _ TrustStore = (*FileTrustStore)(nil)
var _ TransactionalTrustStore = (*FileTrustStore)(nil)

// NewFileTrustStore creates a FileTrustStore persisting to path.
func NewFileTrustStore(path string) *FileTrustStore {
	return &FileTrustStore{path: path}
}

// Load reads the trust file and returns the parsed entries. A missing file is
// not an error and yields an empty map.
func (s *FileTrustStore) Load() (map[string]TrustEntry, error) {
	var entries map[string]TrustEntry
	err := s.update(context.Background(), func(current map[string]TrustEntry) (bool, error) {
		entries = cloneTrustEntries(current)
		return false, nil
	})
	return entries, err
}

// Save writes the entries atomically through a private sibling file.
func (s *FileTrustStore) Save(entries map[string]TrustEntry) error {
	copied := cloneTrustEntries(entries)
	return s.update(context.Background(), func(current map[string]TrustEntry) (bool, error) {
		if err := validateTrustEntryTransitions(current, copied); err != nil {
			return false, err
		}
		for key := range current {
			delete(current, key)
		}
		for key, entry := range copied {
			current[key] = cloneTrustEntry(entry)
		}
		return true, nil
	})
}

// Add upserts a single entry without touching the rest of the store.
func (s *FileTrustStore) Add(path string, entry TrustEntry) error {
	return s.update(context.Background(), func(entries map[string]TrustEntry) (bool, error) {
		if path == "" {
			return false, errors.New("approval: trust entry path is empty")
		}
		if previous, ok := entries[path]; ok {
			previousGeneration := effectiveTrustGeneration(previous)
			entryGeneration := effectiveTrustGeneration(entry)
			if previousGeneration > 0 && entryGeneration < previousGeneration {
				return false, errTrustGenerationRegression
			}
			if previousGeneration > 0 && entryGeneration == previousGeneration && !trustEntryEqual(previous, entry) {
				return false, errTrustGenerationMutation
			}
		}
		entries[path] = cloneTrustEntry(entry)
		return true, nil
	})
}

// Remove deletes the entry under path. Absent paths are a no-op.
func (s *FileTrustStore) Remove(path string) error {
	return s.update(context.Background(), func(entries map[string]TrustEntry) (bool, error) {
		entry, ok := entries[path]
		if !ok {
			return false, nil
		}
		generation := effectiveTrustGeneration(entry)
		if generation == 0 {
			delete(entries, path)
			return true, nil
		}
		if generation == ^uint64(0) {
			return false, errTrustGenerationOverflow
		}
		entries[path] = trustTombstone(path, generation+1)
		return true, nil
	})
}

// Update runs mutate while holding both the instance mutex and the stable
// sidecar OS lock. A dirty result is atomically persisted before lock release.
func (s *FileTrustStore) Update(ctx context.Context, mutate func(map[string]TrustEntry) (bool, error)) error {
	return s.update(ctx, mutate)
}

func (s *FileTrustStore) update(ctx context.Context, mutate func(map[string]TrustEntry) (bool, error)) error {
	if ctx == nil {
		return errors.New("approval: trust transaction context is nil")
	}
	if mutate == nil {
		return errors.New("approval: trust transaction callback is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.canonicalStorePath()
	if err != nil {
		return err
	}
	return privatefile.WithExclusiveLock(ctx, path, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := s.loadLocked(path)
		if err != nil {
			return fmt.Errorf("approval: load trust transaction snapshot: %w", err)
		}
		if err := validateTrustEntries(entries); err != nil {
			return err
		}
		previous := cloneTrustEntries(entries)
		dirty, err := mutate(entries)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := validateTrustEntries(entries); err != nil {
			return err
		}
		if err := validateTrustEntryTransitions(previous, entries); err != nil {
			return err
		}
		if !dirty {
			return nil
		}
		return s.writeLocked(path, entries)
	})
}

func (s *FileTrustStore) canonicalStorePath() (string, error) {
	s.pathOnce.Do(func() { s.canonical, s.canonicalErr = resolveTrustStorePath(s.path) })
	return s.canonical, s.canonicalErr
}

// loadLocked reads and decodes the trust file. Callers must hold the lock. A
// missing file is not an error and yields an empty map.
func (s *FileTrustStore) loadLocked(path string) (map[string]TrustEntry, error) {
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return make(map[string]TrustEntry), nil
		}
		return nil, err
	}
	data, err := readPrivateTrustFile(path)
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
func (s *FileTrustStore) writeLocked(path string, entries map[string]TrustEntry) error {
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	return privatefile.WriteAtomic(path, data)
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
	copied := cloneTrustEntries(entries)
	return s.Update(context.Background(), func(current map[string]TrustEntry) (bool, error) {
		if err := validateTrustEntryTransitions(current, copied); err != nil {
			return false, err
		}
		for key := range current {
			delete(current, key)
		}
		for key, entry := range copied {
			current[key] = cloneTrustEntry(entry)
		}
		return true, nil
	})
}

// Add upserts a single entry under its path.
func (s *InMemoryTrustStore) Add(path string, entry TrustEntry) error {
	return s.Update(context.Background(), func(entries map[string]TrustEntry) (bool, error) {
		if path == "" {
			return false, errors.New("approval: trust entry path is empty")
		}
		if previous, ok := entries[path]; ok {
			previousGeneration := effectiveTrustGeneration(previous)
			entryGeneration := effectiveTrustGeneration(entry)
			if previousGeneration > 0 && entryGeneration < previousGeneration {
				return false, errTrustGenerationRegression
			}
			if previousGeneration > 0 && entryGeneration == previousGeneration && !trustEntryEqual(previous, entry) {
				return false, errTrustGenerationMutation
			}
		}
		entries[path] = cloneTrustEntry(entry)
		return true, nil
	})
}

// Update applies a short trust-store transaction under the in-memory store
// mutex. The callback receives a defensive working copy.
func (s *InMemoryTrustStore) Update(ctx context.Context, mutate func(map[string]TrustEntry) (bool, error)) error {
	if ctx == nil {
		return errors.New("approval: trust transaction context is nil")
	}
	if mutate == nil {
		return errors.New("approval: trust transaction callback is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := cloneTrustEntries(s.entries)
	dirty, err := mutate(entries)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateTrustEntries(entries); err != nil {
		return err
	}
	if err := validateTrustEntryTransitions(s.entries, entries); err != nil {
		return err
	}
	if dirty {
		s.entries = cloneTrustEntries(entries)
	}
	return nil
}

func cloneTrustEntry(entry TrustEntry) TrustEntry {
	entry.ProjectSources = append([]ProjectSourceRecord(nil), entry.ProjectSources...)
	entry.ProjectSourceDirectories = cloneDirectoryRecords(entry.ProjectSourceDirectories)
	return entry
}

func cloneTrustEntries(entries map[string]TrustEntry) map[string]TrustEntry {
	copied := make(map[string]TrustEntry, len(entries))
	for path, entry := range entries {
		copied[path] = cloneTrustEntry(entry)
	}
	return copied
}

func trustEntryEqual(left, right TrustEntry) bool {
	return reflect.DeepEqual(left, right)
}

const revokedTrustExpiry = "1970-01-01T00:00:00Z"

func trustTombstone(root string, generation uint64) TrustEntry {
	return TrustEntry{
		Path:            root,
		CanonicalRoot:   root,
		TrustGeneration: generation,
		Revoked:         true,
		ExpiresAt:       revokedTrustExpiry,
	}
}

func validateTrustEntries(entries map[string]TrustEntry) error {
	for key, entry := range entries {
		if entry.TrustGeneration == 0 && !entry.Revoked && entry.ManifestVersion == 0 {
			continue
		}
		if entry.TrustGeneration == 0 && entry.Revoked {
			return fmt.Errorf("%w: tombstone %q has zero generation", errTrustGenerationInvalid, key)
		}
		if entry.Path != key || !canonicalRootShape(key) {
			return fmt.Errorf("%w: versioned path %q is not canonical", errTrustGenerationInvalid, key)
		}
		if entry.Revoked {
			if entry.CanonicalRoot != key || entry.ManifestVersion != 0 || len(entry.ProjectSources) != 0 ||
				len(entry.ProjectSourceDirectories) != 0 || entry.ManifestDigest != "" {
				return fmt.Errorf("%w: malformed tombstone for %q", errTrustGenerationInvalid, key)
			}
			expiry, err := time.Parse(time.RFC3339, entry.ExpiresAt)
			if err != nil || !expiry.Before(time.Now()) {
				return fmt.Errorf("%w: tombstone %q lacks an expired compatibility marker", errTrustGenerationInvalid, key)
			}
			continue
		}
		if entry.ManifestVersion > 0 && (entry.CanonicalRoot != key || entry.ManifestDigest == "") {
			return fmt.Errorf("%w: malformed active source trust for %q", errTrustGenerationInvalid, key)
		}
		if entry.ManifestVersion == 0 && entry.CanonicalRoot != "" && entry.CanonicalRoot != key {
			return fmt.Errorf("%w: malformed path-only trust for %q", errTrustGenerationInvalid, key)
		}
	}
	return nil
}

func validateTrustEntryTransitions(previous, next map[string]TrustEntry) error {
	if err := validateTrustEntries(next); err != nil {
		return err
	}
	for key, old := range previous {
		oldGeneration := effectiveTrustGeneration(old)
		if oldGeneration == 0 {
			continue
		}
		if current, ok := next[key]; ok {
			currentGeneration := effectiveTrustGeneration(current)
			if currentGeneration < oldGeneration {
				return fmt.Errorf("%w: %q moved from %d to %d", errTrustGenerationRegression, key, oldGeneration, currentGeneration)
			}
			if currentGeneration == oldGeneration && !trustEntryEqual(old, current) {
				return fmt.Errorf("%w: %q at %d", errTrustGenerationMutation, key, oldGeneration)
			}
			continue
		}
		root := trustEntryRoot(key, old)
		replaced := false
		for newKey, current := range next {
			if current.Revoked && current.TrustGeneration > oldGeneration &&
				(newKey == root || current.CanonicalRoot == root || current.Path == root) {
				replaced = true
				break
			}
		}
		if !replaced {
			return fmt.Errorf("%w: removal of %q must retain a newer tombstone", errTrustGenerationRegression, key)
		}
	}
	return nil
}

func trustEntryRoot(key string, entry TrustEntry) string {
	if entry.CanonicalRoot != "" {
		return entry.CanonicalRoot
	}
	if canonical, err := canonicalProjectRoot(entry.Path); err == nil {
		return canonical
	}
	return filepath.Clean(key)
}

func canonicalRootShape(root string) bool {
	return root != "" && filepath.IsAbs(root) && filepath.Clean(root) == root
}

func resolveTrustStorePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("approval: trust store path is empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("approval: resolve trust store path: %w", err)
	}
	absolute = filepath.Clean(absolute)
	missing := make([]string, 0)
	ancestor := filepath.Dir(absolute)
	for {
		info, statErr := os.Lstat(ancestor)
		if statErr == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("approval: trust store parent %q is not a directory", ancestor)
			}
			break
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", fmt.Errorf("approval: inspect trust store parent: %w", statErr)
		}
		base := filepath.Base(ancestor)
		if base == "." || base == string(filepath.Separator) {
			return "", fmt.Errorf("approval: resolve trust store parent: %w", statErr)
		}
		missing = append([]string{base}, missing...)
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", fmt.Errorf("approval: resolve trust store parent: %w", statErr)
		}
		ancestor = parent
	}
	canonicalAncestor, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", fmt.Errorf("approval: canonicalize trust store parent: %w", err)
	}
	info, err := os.Stat(canonicalAncestor)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = errors.New("parent is not a directory")
		}
		return "", fmt.Errorf("approval: canonical trust store parent: %w", err)
	}
	parent := filepath.Clean(canonicalAncestor)
	for _, component := range missing {
		parent = filepath.Join(parent, component)
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

// Remove deletes the entry under path (no-op when absent).
func (s *InMemoryTrustStore) Remove(path string) error {
	return s.Update(context.Background(), func(entries map[string]TrustEntry) (bool, error) {
		entry, ok := entries[path]
		if !ok {
			return false, nil
		}
		generation := effectiveTrustGeneration(entry)
		if generation == 0 {
			delete(entries, path)
			return true, nil
		}
		if generation == ^uint64(0) {
			return false, errTrustGenerationOverflow
		}
		entries[path] = trustTombstone(path, generation+1)
		return true, nil
	})
}
