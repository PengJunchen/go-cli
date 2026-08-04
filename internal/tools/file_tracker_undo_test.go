package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileTrackerBackupRestoreExistingFile(t *testing.T) {
	ft := NewFileTracker()
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "existing.txt")

	original := "original content"
	require.NoError(t, os.WriteFile(path, []byte(original), 0600))

	id, err := ft.Backup(path)
	require.NoError(t, err)
	assert.NotEmpty(t, id)
	assert.True(t, hasCheckpointPrefix(t, id), "checkpoint ID should have cp_ prefix")

	// Modify the file after backup.
	require.NoError(t, os.WriteFile(path, []byte("modified content"), 0600))

	// Restore should bring back the original content.
	require.NoError(t, ft.Restore(id))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, original, string(got), "restored content should match original")
}

func TestFileTrackerBackupNewFileRestoreDeletes(t *testing.T) {
	ft := NewFileTracker()
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "newfile.txt")

	// File does not exist yet - backup records it as a new file.
	id, err := ft.Backup(path)
	require.NoError(t, err)
	assert.NotEmpty(t, id)

	// Create the file (simulating a write operation after backup).
	require.NoError(t, os.WriteFile(path, []byte("newly created"), 0600))

	// Restore should delete the file since it didn't exist at backup time.
	require.NoError(t, ft.Restore(id))

	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err), "file should be deleted after restore")
}

func TestFileTrackerMultipleBackupRestoreCycles(t *testing.T) {
	ft := NewFileTracker()
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "cycle.txt")

	// --- Cycle 1: backup original, modify, restore ---
	original := "original content"
	require.NoError(t, os.WriteFile(path, []byte(original), 0600))

	cp1, err := ft.Backup(path)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(path, []byte("modified content"), 0600))
	require.NoError(t, ft.Restore(cp1))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, original, string(got))

	// --- Cycle 2: backup modified, change again, restore to modified ---
	modified := "modified content"
	require.NoError(t, os.WriteFile(path, []byte(modified), 0600))

	cp2, err := ft.Backup(path)
	require.NoError(t, err)
	assert.NotEqual(t, cp1, cp2, "checkpoint IDs should be unique")

	require.NoError(t, os.WriteFile(path, []byte("other content"), 0600))
	require.NoError(t, ft.Restore(cp2))

	got, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, modified, string(got), "second restore should bring back modified content")
}

func TestFileTrackerListCheckpoints(t *testing.T) {
	ft := NewFileTracker()
	tmpDir := t.TempDir()

	path1 := filepath.Join(tmpDir, "a.txt")
	path2 := filepath.Join(tmpDir, "b.txt")
	path3 := filepath.Join(tmpDir, "c.txt")

	require.NoError(t, os.WriteFile(path1, []byte("aaa"), 0600))
	require.NoError(t, os.WriteFile(path2, []byte("bbb"), 0600))
	require.NoError(t, os.WriteFile(path3, []byte("ccc"), 0600))

	id1, err := ft.Backup(path1)
	require.NoError(t, err)
	id2, err := ft.Backup(path2)
	require.NoError(t, err)
	id3, err := ft.Backup(path3)
	require.NoError(t, err)

	checkpoints := ft.ListCheckpoints()
	require.Len(t, checkpoints, 3)

	// Verify chronological order.
	assert.Equal(t, id1, checkpoints[0].ID)
	assert.Equal(t, id2, checkpoints[1].ID)
	assert.Equal(t, id3, checkpoints[2].ID)

	// Verify metadata.
	assert.Equal(t, path1, checkpoints[0].Path)
	assert.Equal(t, path2, checkpoints[1].Path)
	assert.Equal(t, path3, checkpoints[2].Path)

	assert.True(t, checkpoints[0].Existed)
	assert.True(t, checkpoints[1].Existed)
	assert.True(t, checkpoints[2].Existed)

	assert.Equal(t, int64(3), checkpoints[0].Size)
	assert.Equal(t, int64(3), checkpoints[1].Size)
	assert.Equal(t, int64(3), checkpoints[2].Size)

	assert.False(t, checkpoints[0].Timestamp.IsZero())
	assert.False(t, checkpoints[1].Timestamp.IsZero())
	assert.False(t, checkpoints[2].Timestamp.IsZero())
}

func TestFileTrackerBackupBinaryFileSkipped(t *testing.T) {
	ft := NewFileTracker()
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "binary.dat")

	// Content with a null byte in the first 512 bytes.
	binaryContent := []byte("hello\x00world")
	require.NoError(t, os.WriteFile(path, binaryContent, 0600))

	id, err := ft.Backup(path)
	require.NoError(t, err)
	assert.NotEmpty(t, id)

	checkpoints := ft.ListCheckpoints()
	require.Len(t, checkpoints, 1)

	// Size should be 0 for skipped binary files.
	assert.Equal(t, int64(0), checkpoints[0].Size, "binary file should have Size=0")
	assert.True(t, checkpoints[0].Existed, "file existed")

	// Verify no content was stored: modify file then restore should be a no-op.
	require.NoError(t, os.WriteFile(path, []byte("modified"), 0600))
	require.NoError(t, ft.Restore(id))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "modified", string(got), "restore should be no-op since no content was stored for binary file")
}

func TestFileTrackerConcurrentBackup(t *testing.T) {
	ft := NewFileTracker()
	tmpDir := t.TempDir()

	n := 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			path := filepath.Join(tmpDir, fmt.Sprintf("file_%d.txt", idx))
			if err := os.WriteFile(path, []byte("content"), 0600); err != nil {
				t.Errorf("write file %d: %v", idx, err)
				return
			}
			id, err := ft.Backup(path)
			if err != nil {
				t.Errorf("backup file %d: %v", idx, err)
				return
			}
			if id == "" {
				t.Errorf("empty checkpoint ID for file %d", idx)
			}
		}(i)
	}
	wg.Wait()

	checkpoints := ft.ListCheckpoints()
	assert.Len(t, checkpoints, n, "should have one checkpoint per file")
}

func TestFileTrackerRestoreNonExistentCheckpoint(t *testing.T) {
	ft := NewFileTracker()

	err := ft.Restore("cp_nonexistent")
	assert.Error(t, err, "restoring a non-existent checkpoint should return an error")
}

func TestFileTrackerCheckpointLimit(t *testing.T) {
	ft := NewFileTracker()
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "limit.txt")
	require.NoError(t, os.WriteFile(path, []byte("content"), 0600))

	var ids []string
	for i := 0; i < 55; i++ {
		id, err := ft.Backup(path)
		require.NoError(t, err)
		ids = append(ids, id)
	}

	checkpoints := ft.ListCheckpoints()
	assert.Len(t, checkpoints, 50, "should keep only the last 50 checkpoints")

	// The first 5 checkpoints should have been trimmed.
	for i := 0; i < 5; i++ {
		err := ft.Restore(ids[i])
		assert.Error(t, err, "checkpoint %d should have been trimmed", i)
	}

	// The last 50 checkpoints should still be restorable.
	for i := 5; i < 55; i++ {
		err := ft.Restore(ids[i])
		assert.NoError(t, err, "checkpoint %d should still exist", i)
	}

	// Verify the remaining checkpoints are the last 50 by ID.
	remaining := ft.ListCheckpoints()
	require.Len(t, remaining, 50)
	assert.Equal(t, ids[5], remaining[0].ID, "first remaining checkpoint should be the 6th created")
	assert.Equal(t, ids[54], remaining[49].ID, "last remaining checkpoint should be the 55th created")
}

func TestFileTrackerBackupContent(t *testing.T) {
	ft := NewFileTracker()
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "existing.txt")
	original := "original content"
	require.NoError(t, os.WriteFile(path, []byte(original), 0600))

	id, err := ft.Backup(path)
	require.NoError(t, err)

	content, ok := ft.BackupContent(id)
	require.True(t, ok, "backup content should be available for an existing-file checkpoint")
	assert.Equal(t, original, string(content), "backup content should match the original file")

	// Mutating the returned slice must not affect the stored backup.
	content[0] = 'X'
	again, _ := ft.BackupContent(id)
	assert.Equal(t, original, string(again), "stored backup content must be a defensive copy")
}

func TestFileTrackerBackupContentNewFile(t *testing.T) {
	ft := NewFileTracker()
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "newfile.txt")

	id, err := ft.Backup(path) // file does not exist -> new-file checkpoint
	require.NoError(t, err)

	_, ok := ft.BackupContent(id)
	assert.False(t, ok, "no backup content should be stored for a new-file checkpoint")
}

func TestFileTrackerBackupContentUnknownID(t *testing.T) {
	ft := NewFileTracker()
	_, ok := ft.BackupContent("cp_does_not_exist")
	assert.False(t, ok, "unknown checkpoint id should return ok=false")
}

// hasCheckpointPrefix is a test helper that verifies a checkpoint ID starts
// with "cp_".
func hasCheckpointPrefix(t *testing.T, id string) bool {
	t.Helper()
	return len(id) > 3 && id[:3] == "cp_"
}
