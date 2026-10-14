package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestPathWhitelistConstructorAbsFailureProducesInvalidEmptyScope(t *testing.T) {
	wantErr := errors.New("cwd unavailable")
	whitelist := newPathWhitelist([]string{t.TempDir(), "relative-root"}, func(path string) (string, error) {
		if path == "relative-root" {
			return "", wantErr
		}
		return filepath.Abs(path)
	})

	require.True(t, whitelist.invalid)
	assert.Empty(t, whitelist.paths, "a partial whitelist must not survive a later base-path resolution error")
	assert.False(t, whitelist.IsAllowed(filepath.Join(t.TempDir(), "candidate")))
}

func TestInvalidPathWhitelistRejectsEveryFileTool(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	file := filepath.Join(dir, "existing.txt")
	require.NoError(t, os.WriteFile(file, []byte("original marker"), 0o600))

	whitelist := newPathWhitelist([]string{"relative-root"}, func(string) (string, error) {
		return "", errors.New("injected filepath.Abs failure")
	})
	require.True(t, whitelist.invalid)
	require.Empty(t, whitelist.paths)

	t.Run("read", func(t *testing.T) {
		tool := NewReadTool()
		tool.whitelist = whitelist
		_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": file}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "whitelist")
	})
	t.Run("grep", func(t *testing.T) {
		tool := NewGrepTool(WithForcePureGo(true))
		tool.whitelist = whitelist
		_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"pattern": "marker", "path": dir}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "whitelist")
	})
	t.Run("find", func(t *testing.T) {
		tool := NewFindTool(WithFindForceNode(true))
		tool.whitelist = whitelist
		_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"pattern": "*.txt", "path": dir}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "whitelist")
	})
	t.Run("ls", func(t *testing.T) {
		tool := NewLSTool()
		tool.whitelist = whitelist
		_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": dir}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "whitelist")
	})
	t.Run("write", func(t *testing.T) {
		created := filepath.Join(dir, "new.txt")
		tool := NewWriteTool()
		tool.whitelist = whitelist
		_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": created, "content": "must not write"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "whitelist")
		assert.NoFileExists(t, created)
	})
	t.Run("edit", func(t *testing.T) {
		tool := NewEditFileTool()
		tool.whitelist = whitelist
		_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{
			"file_path": file, "old_string": "original", "new_string": "changed",
		}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "whitelist")
		contents, readErr := os.ReadFile(file)
		require.NoError(t, readErr)
		assert.Equal(t, "original marker", string(contents))
	})
}

func TestPathWhitelistFilesystemRootAllowsAbsoluteChildren(t *testing.T) {
	root := filepath.VolumeName(os.TempDir()) + string(filepath.Separator)
	whitelist := NewPathWhitelist([]string{root})
	require.False(t, whitelist.invalid)
	assert.True(t, whitelist.IsAllowed(root))
	assert.True(t, whitelist.IsAllowed(filepath.Join(root, "path-whitelist-root-probe", "child")))
}
