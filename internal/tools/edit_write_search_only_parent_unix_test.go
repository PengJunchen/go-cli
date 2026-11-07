//go:build darwin || linux

package tools

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEditThroughWriteSearchOnlyParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may bypass directory permissions")
	}
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent")
	path := filepath.Join(parent, "file")
	require.NoError(t, os.Mkdir(parent, 0700))
	require.NoError(t, os.WriteFile(path, []byte("old content"), 0640))
	require.NoError(t, os.Chmod(path, 0640))
	t.Cleanup(func() {
		if err := os.Chmod(parent, 0700); err != nil {
			t.Errorf("restore parent permissions: %v", err)
		}
	})
	require.NoError(t, os.Chmod(parent, 0300))

	parentFile, openErr := os.Open(parent)
	if openErr == nil {
		require.NoError(t, parentFile.Close())
		t.Skip("read-only directory open was permitted despite mode 0300")
	}
	require.ErrorIs(t, openErr, syscall.EACCES)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "old content", string(content))
	originalInfo, err := os.Stat(path)
	require.NoError(t, err)
	_, err = resolveAndValidatePath("edit", "", path, NewPathWhitelist([]string{dir}))
	require.NoError(t, err)
	t.Log("preconditions met: parent read denied, target readable and legacy path validation allowed")

	result, err := NewEditFileTool(WithEditPathWhitelist([]string{dir})).Execute(context.Background(), ToolCall{Args: map[string]any{
		"file_path": path, "old_string": "old", "new_string": "new",
	}})
	require.NoError(t, err)
	require.NotNil(t, result)
	content, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "new content", string(content))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.False(t, os.SameFile(originalInfo, info), "edit should replace the file through its parent directory")
	require.Equal(t, os.FileMode(0640), info.Mode().Perm())
}
