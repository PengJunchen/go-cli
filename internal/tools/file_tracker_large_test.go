package tools

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBackupWarnsOnLargeFile verifies that Backup logs a warning when the file
// exceeds maxBackupFileSize (10MB) and still completes the backup successfully.
func TestBackupWarnsOnLargeFile(t *testing.T) {
	// Capture slog output.
	var logBuf bytes.Buffer
	handler := slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(oldLogger)

	dir := t.TempDir()
	path := filepath.Join(dir, "large.txt")

	// Create a file just over 10MB. Fill with non-null bytes so it is not
	// treated as binary.
	size := maxBackupFileSize + 1
	data := make([]byte, size)
	for i := range data {
		data[i] = 'A'
	}
	require.NoError(t, os.WriteFile(path, data, 0o644))

	ft := NewFileTracker()
	id, err := ft.Backup(path)
	require.NoError(t, err)
	assert.NotEmpty(t, id)

	// The warning should be present in the log output.
	logOutput := logBuf.String()
	assert.Contains(t, logOutput, "file_tracker.backup_large_file")
	assert.Contains(t, logOutput, path)

	// Backup content should be stored so that Restore can recover it.
	content, ok := ft.BackupContent(id)
	require.True(t, ok)
	assert.Len(t, content, size)
}

// TestBackupNoWarnForSmallFile ensures the warning is NOT emitted for files
// under the threshold.
func TestBackupNoWarnForSmallFile(t *testing.T) {
	var logBuf bytes.Buffer
	handler := slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(oldLogger)

	dir := t.TempDir()
	path := filepath.Join(dir, "small.txt")
	require.NoError(t, os.WriteFile(path, []byte("tiny"), 0o644))

	ft := NewFileTracker()
	_, err := ft.Backup(path)
	require.NoError(t, err)

	assert.NotContains(t, logBuf.String(), "file_tracker.backup_large_file")
}
