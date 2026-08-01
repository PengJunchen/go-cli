package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestWriteNewFile(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	tool := NewWriteTool(WithWriteWorkdir(dir))

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "out.txt", "content": "hello"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "wrote 5 bytes")

	data, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))
}

func TestWriteOverwriteDisabledFails(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	require.NoError(t, os.WriteFile(path, []byte("original"), 0o600))

	tool := NewWriteTool(WithWriteWorkdir(dir))
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "out.txt", "content": "new"},
	})
	assert.Error(t, err)

	data, rerr := os.ReadFile(path)
	require.NoError(t, rerr)
	assert.Equal(t, "original", string(data))
}

func TestWriteOverwriteEnabledSucceeds(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	require.NoError(t, os.WriteFile(path, []byte("original"), 0o600))
	tool := NewWriteTool(WithWriteWorkdir(dir), WithOverwrite(true))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "out.txt", "content": "new"},
	})
	require.NoError(t, err)
	assert.NotNil(t, res)

	data, rerr := os.ReadFile(path)
	require.NoError(t, rerr)
	assert.Equal(t, "new", string(data))
}

func TestWriteCreatesParentDirs(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	tool := NewWriteTool(WithWriteWorkdir(dir))

	rel := filepath.Join("deep", "nested", "file.txt")
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": rel, "content": "x"},
	})
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dir, rel))
	require.NoError(t, err)
	assert.Equal(t, "x", string(data))
}

func TestWriteEmptyContent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	tool := NewWriteTool(WithWriteWorkdir(dir))

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "empty.txt", "content": ""},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, res.Metadata["bytes"])

	data, err := os.ReadFile(filepath.Join(dir, "empty.txt"))
	require.NoError(t, err)
	assert.Empty(t, data)
}

func TestWriteMissingPath(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewWriteTool(WithWriteWorkdir(t.TempDir()))
	_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{}})
	assert.Error(t, err)
}

func TestWriteSizeLimit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	tool := NewWriteTool(WithWriteWorkdir(dir), WithWriteMaxBytes(4))
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "big.txt", "content": "toolong"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "exceeding the maximum")
}

func TestWriteName(t *testing.T) {
	tool := NewWriteTool()
	assert.Equal(t, "write", tool.Name())
}
