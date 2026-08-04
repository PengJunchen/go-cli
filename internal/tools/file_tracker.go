package tools

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
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
	mu              sync.RWMutex
	hashes          map[string]string // path -> current content hash
	changes         []FileChange
	checkpoints     map[string]CheckpointMeta
	checkpointOrder []string // checkpoint IDs in creation order
	backupContent   map[string][]byte
}

// NewFileTracker returns an empty FileTracker.
func NewFileTracker() *FileTracker {
	return &FileTracker{
		hashes:          make(map[string]string),
		changes:         make([]FileChange, 0),
		checkpoints:     make(map[string]CheckpointMeta),
		checkpointOrder: make([]string, 0),
		backupContent:   make(map[string][]byte),
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

// CheckpointMeta records metadata about a backup checkpoint.
type CheckpointMeta struct {
	// ID is the unique checkpoint ID (timestamp-based).
	ID string
	// Path is the file path that was backed up.
	Path string
	// Timestamp is when the checkpoint was created.
	Timestamp time.Time
	// Size is the size of the original file in bytes. It is 0 for new files
	// and for binary files that were skipped.
	Size int64
	// Existed reports whether the file existed before the checkpoint. false
	// means the file was new (did not exist), so Restore will delete it.
	Existed bool
}

// maxCheckpoints is the maximum number of checkpoints retained in memory.
// Older checkpoints are trimmed when this limit is exceeded.
const maxCheckpoints = 50

// Backup creates a checkpoint of the given file path. If the file exists,
// it copies the content to a backup location. If the file doesn't exist
// (new file), it records that fact so Restore can delete the file.
// Binary files (detected by null bytes in the first 512 bytes) are skipped:
// a checkpoint is returned with Size=0 but no content is stored.
// Returns the checkpoint ID.
func (ft *FileTracker) Backup(path string) (string, error) {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	now := time.Now()
	id := fmt.Sprintf("cp_%d", now.UnixNano())

	info, err := os.Stat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("backup: stat %s: %w", path, err)
		}
		// File does not exist - record as a new-file checkpoint.
		ft.storeCheckpoint(CheckpointMeta{
			ID:        id,
			Path:      path,
			Timestamp: now,
			Size:      0,
			Existed:   false,
		}, nil)
		return id, nil
	}

	// File exists - read its content.
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("backup: read %s: %w", path, err)
	}

	// Skip binary files: return a checkpoint with Size=0 and no content.
	if isBinaryContent(content) {
		ft.storeCheckpoint(CheckpointMeta{
			ID:        id,
			Path:      path,
			Timestamp: now,
			Size:      0,
			Existed:   true,
		}, nil)
		return id, nil
	}

	ft.storeCheckpoint(CheckpointMeta{
		ID:        id,
		Path:      path,
		Timestamp: now,
		Size:      info.Size(),
		Existed:   true,
	}, content)
	return id, nil
}

// Restore restores the file to the state recorded in the checkpoint.
// If the checkpoint was for an existing file, it restores the backup content.
// If the checkpoint was for a new file (Existed=false), it deletes the file.
// Checkpoints for binary files that were skipped are a no-op.
func (ft *FileTracker) Restore(checkpointID string) error {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	meta, ok := ft.checkpoints[checkpointID]
	if !ok {
		return fmt.Errorf("restore: checkpoint %s not found", checkpointID)
	}

	if !meta.Existed {
		// The file was new (did not exist before) - remove it.
		if err := os.Remove(meta.Path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("restore: remove %s: %w", meta.Path, err)
		}
		return nil
	}

	// The file existed - restore content if available.
	content, hasContent := ft.backupContent[checkpointID]
	if !hasContent {
		// Binary file was skipped during backup - nothing to restore.
		return nil
	}

	if err := os.WriteFile(meta.Path, content, 0644); err != nil {
		return fmt.Errorf("restore: write %s: %w", meta.Path, err)
	}
	return nil
}

// ListCheckpoints returns all checkpoints in chronological order.
func (ft *FileTracker) ListCheckpoints() []CheckpointMeta {
	ft.mu.RLock()
	defer ft.mu.RUnlock()

	out := make([]CheckpointMeta, len(ft.checkpointOrder))
	for i, id := range ft.checkpointOrder {
		out[i] = ft.checkpoints[id]
	}
	return out
}

// storeCheckpoint records a checkpoint and its optional backup content, then
// trims old checkpoints so that at most maxCheckpoints are retained. The
// caller must hold ft.mu.
func (ft *FileTracker) storeCheckpoint(meta CheckpointMeta, content []byte) {
	ft.checkpoints[meta.ID] = meta
	ft.checkpointOrder = append(ft.checkpointOrder, meta.ID)
	if content != nil {
		ft.backupContent[meta.ID] = content
	}

	for len(ft.checkpointOrder) > maxCheckpoints {
		oldID := ft.checkpointOrder[0]
		ft.checkpointOrder = ft.checkpointOrder[1:]
		delete(ft.checkpoints, oldID)
		delete(ft.backupContent, oldID)
	}
}

// isBinaryContent reports whether data appears to be binary by checking for
// null bytes in the first 512 bytes.
func isBinaryContent(data []byte) bool {
	n := len(data)
	if n > 512 {
		n = 512
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}
