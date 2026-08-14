package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImageReadTool_Name(t *testing.T) {
	tool := NewImageReadTool()
	assert.Equal(t, "image_read", tool.Name())
}

func TestImageReadTool_Description(t *testing.T) {
	tool := NewImageReadTool()
	assert.Contains(t, tool.Description(), "image_read")
}

func TestImageReadTool_Parameters(t *testing.T) {
	tool := NewImageReadTool()
	schema, ok := tool.Parameters().(map[string]any)
	require.True(t, ok, "Parameters() should return a map[string]any")

	props, ok := schema["properties"].(map[string]any)
	require.True(t, ok, "schema should have a properties map")

	_, ok = props["path"]
	assert.True(t, ok, "expected 'path' property in parameters")

	required, ok := schema["required"].([]string)
	require.True(t, ok, "schema should have a required field")
	assert.Contains(t, required, "path")
}

func TestImageReadTool_Execute_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.png")
	content := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 'f', 'a', 'k', 'e'}
	require.NoError(t, os.WriteFile(path, content, 0o600))

	tool := NewImageReadTool()
	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": path}})
	require.NoError(t, err)

	assert.Contains(t, res.Output, "data:image/png;base64,")
	assert.Equal(t, "image/png", res.Metadata["mime_type"])
	assert.Equal(t, len(content), res.Metadata["file_size"])
	assert.Equal(t, path, res.Metadata["file_path"])
	assert.Contains(t, res.Metadata["data_uri"].(string), "data:image/png;base64,")
}

func TestImageReadTool_Execute_NotFound(t *testing.T) {
	tool := NewImageReadTool()
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": filepath.Join(t.TempDir(), "missing.png")},
	})
	assert.Error(t, err)
}

func TestImageReadTool_Execute_UnsupportedFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(path, []byte("plain text"), 0o600))

	tool := NewImageReadTool()
	_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": path}})
	assert.Error(t, err)
}

func TestImageReadTool_Execute_MissingPath(t *testing.T) {
	tool := NewImageReadTool()
	_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{}})
	assert.Error(t, err)
}
