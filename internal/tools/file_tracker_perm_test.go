package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRestorePreservesPermissions verifies that Restore writes the file back
// with the original permissions captured during Backup, not a hardcoded 0o600.
func TestRestorePreservesPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "perm_test.txt")

	originalContent := []byte("hello world")
	require.NoError(t, os.WriteFile(path, originalContent, 0o644))

	// Use a distinctive permission that differs from the 0o600 default so the
	// assertion is meaningful. 0o644 is common but we also chmod to be safe
	// against umask.
	require.NoError(t, os.Chmod(path, 0o644))

	ft := NewFileTracker()
	id, err := ft.Backup(path)
	require.NoError(t, err)

	// Modify both content and permissions after backup.
	require.NoError(t, os.WriteFile(path, []byte("changed"), 0o600))
	require.NoError(t, os.Chmod(path, 0o600))

	require.NoError(t, ft.Restore(id))

	// Content should be restored.
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, originalContent, got)

	// Permissions should match the original 0o644, not the 0o600 we set after.
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
}

// TestRestorePreservesCustomPermissions uses a non-default permission (0o755)
// to ensure Restore is genuinely reading the stored Mode rather than coinciding
// with the fallback.
func TestRestorePreservesCustomPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom_perm.txt")

	require.NoError(t, os.WriteFile(path, []byte("data"), 0o755))
	require.NoError(t, os.Chmod(path, 0o755))

	ft := NewFileTracker()
	id, err := ft.Backup(path)
	require.NoError(t, err)

	// Corrupt permissions.
	require.NoError(t, os.Chmod(path, 0o600))

	require.NoError(t, ft.Restore(id))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

// TestRestoreDefaultPermWhenNotSet verifies backward compatibility: when a
// checkpoint has Mode==0 (as created by older code that predates the Mode
// field), Restore falls back to 0o600.
func TestRestoreDefaultPermWhenNotSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backward_compat.txt")

	ft := NewFileTracker()

	// Manually store a checkpoint with Mode==0 to simulate an old checkpoint.
	ft.mu.Lock()
	ft.storeCheckpoint(CheckpointMeta{
		ID:      "cp_old",
		Path:    path,
		Existed: true,
		Mode:    0, // not set — backward compat
	}, []byte("restored content"))
	ft.mu.Unlock()

	require.NoError(t, ft.Restore("cp_old"))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, []byte("restored content"), got)
}
