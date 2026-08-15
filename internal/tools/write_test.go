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

func TestWriteDescription(t *testing.T) {
	tool := NewWriteTool()
	desc := tool.Description()
	assert.Contains(t, desc, "write")
	assert.Contains(t, desc, "path")
}

func TestWriteWithWriteMaxBytes(t *testing.T) {
	tool := NewWriteTool(WithWriteMaxBytes(100))
	assert.Equal(t, 100, tool.MaxBytes)
}

func TestWriteWithOverwrite(t *testing.T) {
	tool := NewWriteTool(WithOverwrite(true))
	assert.True(t, tool.Overwrite)
}

func TestWriteAppendMode(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := filepath.Join(dir, "append.txt")
	require.NoError(t, os.WriteFile(path, []byte("original"), 0o600))

	tool := NewWriteTool(WithWriteWorkdir(dir))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "append.txt", "content": " appended", "append": true},
	})
	require.NoError(t, err)
	assert.NotNil(t, res)

	data, rerr := os.ReadFile(path)
	require.NoError(t, rerr)
	assert.Equal(t, "original appended", string(data))
}

func TestWriteAppendNewFile(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	tool := NewWriteTool(WithWriteWorkdir(dir))

	// Append to a file that doesn't exist yet should create it.
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "new.txt", "content": "fresh", "append": true},
	})
	require.NoError(t, err)
	assert.NotNil(t, res)

	data, rerr := os.ReadFile(filepath.Join(dir, "new.txt"))
	require.NoError(t, rerr)
	assert.Equal(t, "fresh", string(data))
}

func TestWriteDirectoryError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	// Create a directory where we want to write a file.
	path := filepath.Join(dir, "mydir")
	require.NoError(t, os.MkdirAll(path, 0o750)) //nolint:gosec

	tool := NewWriteTool(WithWriteWorkdir(dir))
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "mydir", "content": "data"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "directory")
}

func TestWriteMissingContentArg(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	tool := NewWriteTool(WithWriteWorkdir(dir), WithOverwrite(true))

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "empty.txt"},
	})
	require.NoError(t, err)
	assert.NotNil(t, res)

	data, rerr := os.ReadFile(filepath.Join(dir, "empty.txt"))
	require.NoError(t, rerr)
	assert.Empty(t, data)
}

// TestWriteWithFileTrackerOption verifies that WithFileTracker sets the
// fileTracker field so that Execute creates a backup checkpoint before
// overwriting an existing file.
func TestWriteWithFileTrackerOption(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	require.NoError(t, os.WriteFile(path, []byte("original"), 0o600))

	ft := NewFileTracker()
	tool := NewWriteTool(WithWriteWorkdir(dir), WithOverwrite(true), WithFileTracker(ft))

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "out.txt", "content": "new"},
	})
	require.NoError(t, err)

	// Backup should have created a checkpoint for the existing file.
	checkpoints := ft.ListCheckpoints()
	require.Len(t, checkpoints, 1)
	assert.Equal(t, path, checkpoints[0].Path)
	assert.True(t, checkpoints[0].Existed)
}

// TestWriteWithFileTrackerNewFile verifies that Backup is called even for
// new files (where the file does not exist yet).
func TestWriteWithFileTrackerNewFile(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	ft := NewFileTracker()
	tool := NewWriteTool(WithWriteWorkdir(dir), WithFileTracker(ft))

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "new.txt", "content": "fresh"},
	})
	require.NoError(t, err)

	checkpoints := ft.ListCheckpoints()
	require.Len(t, checkpoints, 1)
	assert.False(t, checkpoints[0].Existed)
}

// --- Path whitelist + symlink security tests (SEC-04) ---

// TestWriteRejectsAbsolutePathOutsideWhitelist verifies AC-1: an absolute path
// that falls outside the configured whitelist is rejected.
func TestWriteRejectsAbsolutePathOutsideWhitelist(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	outside := t.TempDir()
	tool := NewWriteTool(WithWriteWorkdir(dir), WithWritePathWhitelist([]string{dir}))

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": filepath.Join(outside, "evil.txt"), "content": "data"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "whitelist")

	// File must not have been created.
	_, statErr := os.Stat(filepath.Join(outside, "evil.txt"))
	require.Error(t, statErr)
}

// TestWriteRejectsSymlinkDirectoryEscape verifies AC-2: a symlink in an
// intermediate directory that resolves outside the whitelist is rejected.
func TestWriteRejectsSymlinkDirectoryEscape(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	outside := t.TempDir()

	// Create a symlink directory inside dir pointing to outside.
	linkDir := filepath.Join(dir, "escape")
	require.NoError(t, os.Symlink(outside, linkDir))

	tool := NewWriteTool(WithWriteWorkdir(dir), WithWritePathWhitelist([]string{dir}))

	// Writing to escape/file.txt — the file itself doesn't exist (so
	// Lstat won't flag it), but resolveSymlinks resolves escape → outside
	// which is not in the whitelist.
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": filepath.Join("escape", "file.txt"), "content": "data"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "whitelist")

	// File must not have been created in the outside directory.
	_, statErr := os.Stat(filepath.Join(outside, "file.txt"))
	require.Error(t, statErr)
}

// TestWriteRejectsSymlinkTarget verifies AC-3: O_NOFOLLOW semantics — if the
// target path itself is a symlink, the write is rejected even when the symlink
// points within the whitelist.
func TestWriteRejectsSymlinkTarget(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()

	// Create a regular file and a symlink to it (both inside dir).
	target := filepath.Join(dir, "target.txt")
	require.NoError(t, os.WriteFile(target, []byte("original"), 0o600))
	link := filepath.Join(dir, "link.txt")
	require.NoError(t, os.Symlink(target, link))

	tool := NewWriteTool(
		WithWriteWorkdir(dir),
		WithWritePathWhitelist([]string{dir}),
		WithOverwrite(true),
	)

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "link.txt", "content": "new"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")

	// Original file must be unchanged.
	data, rerr := os.ReadFile(target)
	require.NoError(t, rerr)
	assert.Equal(t, "original", string(data))
}

// TestWriteValidPathWithinWhitelist verifies that a valid path inside the
// whitelist is accepted (AC-4 positive case).
func TestWriteValidPathWithinWhitelist(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	tool := NewWriteTool(WithWriteWorkdir(dir), WithWritePathWhitelist([]string{dir}))

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "safe.txt", "content": "hello"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "wrote 5 bytes")

	data, rerr := os.ReadFile(filepath.Join(dir, "safe.txt"))
	require.NoError(t, rerr)
	assert.Equal(t, "hello", string(data))
}

// TestWriteNestedPathWithinWhitelist verifies that a nested relative path
// inside the whitelist is accepted.
func TestWriteNestedPathWithinWhitelist(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	tool := NewWriteTool(WithWriteWorkdir(dir), WithWritePathWhitelist([]string{dir}))

	rel := filepath.Join("sub", "deep", "file.txt")
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": rel, "content": "nested"},
	})
	require.NoError(t, err)

	data, rerr := os.ReadFile(filepath.Join(dir, rel))
	require.NoError(t, rerr)
	assert.Equal(t, "nested", string(data))
}

// TestWriteNoWhitelistAllowsSymlink verifies backward compatibility: when no
// whitelist is configured, writing to a non-symlink path still works (the
// O_NOFOLLOW check still rejects symlinks, but non-symlinks are fine).
func TestWriteNoWhitelistAllowsRegularFile(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	tool := NewWriteTool(WithWriteWorkdir(dir))

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "file.txt", "content": "ok"},
	})
	require.NoError(t, err)
	assert.NotNil(t, res)

	data, rerr := os.ReadFile(filepath.Join(dir, "file.txt"))
	require.NoError(t, rerr)
	assert.Equal(t, "ok", string(data))
}
