package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestReadFileContents(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := filepath.Join(dir, "sample.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello world"), 0o600))

	tool := NewReadTool(WithWorkdir(dir))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "sample.txt"},
	})
	require.NoError(t, err)
	assert.Equal(t, "hello world", res.Output)
	assert.Equal(t, len("hello world"), res.Metadata["bytes"])
	assert.Equal(t, path, res.Metadata["path"])
}

func TestReadMissingPathArg(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewReadTool()
	_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{}})
	assert.Error(t, err)

	_, err = tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": 42}})
	assert.Error(t, err)
}

func TestReadMissingFile(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewReadTool(WithWorkdir(t.TempDir()))
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "does-not-exist.txt"},
	})
	assert.Error(t, err)
}

func TestReadDirectoryListsEntries(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o750))

	tool := NewReadTool(WithWorkdir(dir))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "."},
	})
	require.NoError(t, err)

	assert.Contains(t, res.Output, "a.txt")
	assert.Contains(t, res.Output, "b.txt")
	assert.Contains(t, res.Output, "sub/")
	assert.Equal(t, 3, res.Metadata["entries"])
}

func TestReadMaxBytesError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", 128)), 0o600))

	tool := NewReadTool(WithWorkdir(dir), WithMaxBytes(16))
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "big.txt"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "exceeding the maximum")
}

func TestReadRelativePathResolution(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	base := t.TempDir()
	sub := filepath.Join(base, "nested")
	require.NoError(t, os.MkdirAll(sub, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "inner.txt"), []byte("inner"), 0o600))

	tool := NewReadTool(WithWorkdir(base))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": filepath.Join("nested", "inner.txt")},
	})
	require.NoError(t, err)
	assert.Equal(t, "inner", res.Output)
}

func TestReadNameAndNames(t *testing.T) {
	tool := NewReadTool()
	assert.Equal(t, "read", tool.Name())
	assert.Contains(t, tool.Names(), "read")
	assert.Contains(t, tool.Names(), "read_file")
}

func TestReadRefusesSymlinkWhenNotFollowing(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	require.NoError(t, os.WriteFile(target, []byte("secret"), 0o600))

	link := filepath.Join(dir, "link.txt")
	require.NoError(t, os.Symlink(target, link))

	tool := NewReadTool(WithWorkdir(dir), WithFollowSymlinks(false))
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "link.txt"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")
}

func TestReadFollowsSymlinkWhenEnabled(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	require.NoError(t, os.WriteFile(target, []byte("via-link"), 0o600))

	link := filepath.Join(dir, "link.txt")
	require.NoError(t, os.Symlink(target, link))

	tool := NewReadTool(WithWorkdir(dir), WithFollowSymlinks(true))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "link.txt"},
	})
	require.NoError(t, err)
	assert.Equal(t, "via-link", res.Output)
}

func TestReadRejectsSpecialFile(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	if _, err := os.Lstat("/dev/null"); err != nil {
		t.Skip("/dev/null not available on this platform")
	}

	tool := NewReadTool()
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "/dev/null"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "special file")
}
