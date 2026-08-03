package tools

import (
	"crypto/md5"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"
)

// FileChange records a single detected change to a tracked file.
type FileChange struct {
	// Path is the file path that changed.
	Path string `json:"path"`
	// OldHash is the MD5 hash of the previous content, empty when the file
	// is tracked for the first time.
	OldHash string `json:"old_hash"`
	// NewHash is the MD5 hash of the current content.
	NewHash string `json:"new_hash"`
	// Timestamp is when the change was recorded.
	Timestamp time.Time `json:"timestamp"`
}

// FileTracker records file states and detects changes between successive Track
// calls. It is concurrency-safe.
type FileTracker struct {
	mu      sync.RWMutex
	hashes  map[string]string // path -> current content hash
	changes []FileChange
}

// NewFileTracker returns an empty FileTracker.
func NewFileTracker() *FileTracker {
	return &FileTracker{
		hashes:  make(map[string]string),
		changes: make([]FileChange, 0),
	}
}

// Track records the state of the file at path with the given content. If the
// content differs from the previously recorded state (or the file is tracked
// for the first time), a FileChange entry is appended.
func (ft *FileTracker) Track(path string, content string) {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	newHash := hashContent(content)
	oldHash, existed := ft.hashes[path]

	if !existed {
		// First time tracking this path: record as a new file.
		slog.Debug("file_tracker.track_new", "path", path)
		ft.changes = append(ft.changes, FileChange{
			Path:      path,
			OldHash:   "",
			NewHash:   newHash,
			Timestamp: time.Now(),
		})
		ft.hashes[path] = newHash
		return
	}

	if oldHash != newHash {
		slog.Debug("file_tracker.track_changed", "path", path, "old_hash", oldHash, "new_hash", newHash)
		ft.changes = append(ft.changes, FileChange{
			Path:      path,
			OldHash:   oldHash,
			NewHash:   newHash,
			Timestamp: time.Now(),
		})
		ft.hashes[path] = newHash
		return
	}

	slog.Debug("file_tracker.track_unchanged", "path", path)
}

// GetChanges returns a copy of all recorded file changes in insertion order.
func (ft *FileTracker) GetChanges() []FileChange {
	ft.mu.RLock()
	defer ft.mu.RUnlock()

	out := make([]FileChange, len(ft.changes))
	copy(out, ft.changes)
	return out
}

// HasChanged reports whether any change has been recorded for the given path.
func (ft *FileTracker) HasChanged(path string) bool {
	ft.mu.RLock()
	defer ft.mu.RUnlock()

	for i := range ft.changes {
		if ft.changes[i].Path == path {
			return true
		}
	}
	return false
}

// Reset clears all tracked state and recorded changes.
func (ft *FileTracker) Reset() {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	ft.hashes = make(map[string]string)
	ft.changes = make([]FileChange, 0)
	slog.Debug("file_tracker.reset")
}

// hashContent computes the MD5 hash of content and returns its hex encoding.
func hashContent(content string) string {
	sum := md5.Sum([]byte(content))
	return hex.EncodeToString(sum[:])
}
