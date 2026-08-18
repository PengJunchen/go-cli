package tools

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// captureExporter is a minimal in-memory tracing.TraceExporter used to assert
// that a tool emitted a `tool.call` span. It mirrors MockTraceExporter's
// behavior without importing internal/mock (which imports internal/tools and
// would create an import cycle from this test package).
type captureExporter struct {
	mu    sync.Mutex
	spans []tracing.SpanData
}

func (e *captureExporter) ExportSpan(_ context.Context, span tracing.TraceSpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, tracing.SpanToData(span))
	return nil
}

func (e *captureExporter) Shutdown(_ context.Context) error { return nil }

func (e *captureExporter) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.spans)
}

func (e *captureExporter) hasSpan(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range e.spans {
		if s.Name == name {
			return true
		}
	}
	return false
}

func TestEditReplacesSingleMatch(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(path, []byte("package main\n\nvar version = 1\n"), 0o600))

	tool := NewEditFileTool()
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"file_path":  path,
			"old_string": "var version = 1",
			"new_string": "var version = 2",
		},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, path)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "var version = 1")
	assert.Contains(t, string(data), "var version = 2")
}

func TestEditEmptyNewStringRemovesBlock(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(path, []byte("keep\ndelete me\nkeep\n"), 0o600))

	tool := NewEditFileTool()
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"file_path":  path,
			"old_string": "delete me\n",
			"new_string": "",
		},
	})
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "keep\nkeep\n", string(data))
}

func TestEditZeroMatchErrors(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello world\n"), 0o600))

	tool := NewEditFileTool()
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"file_path":  path,
			"old_string": "does not exist",
			"new_string": "x",
		},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestEditMultipleMatchErrors(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	require.NoError(t, os.WriteFile(path, []byte("dup\ndup\ndup\n"), 0o600))

	tool := NewEditFileTool()
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"file_path":  path,
			"old_string": "dup",
			"new_string": "x",
		},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "expected exactly once")
}

func TestEditMissingFileErrors(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewEditFileTool()
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"file_path":  filepath.Join(t.TempDir(), "missing.txt"),
			"old_string": "a",
			"new_string": "b",
		},
	})
	assert.Error(t, err)
}

func TestEditMissingArgs(t *testing.T) {
	tool := NewEditFileTool()

	_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{}})
	assert.Error(t, err)

	_, err = tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"file_path": "/tmp/x", "old_string": 42},
	})
	assert.Error(t, err)
}

func TestEditName(t *testing.T) {
	assert.Equal(t, "edit", NewEditFileTool().Name())
	assert.Contains(t, NewEditFileTool().Description(), "file_path")
}

func TestEditEmitsToolCallSpan(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := filepath.Join(dir, "b.txt")
	require.NoError(t, os.WriteFile(path, []byte("alpha\n"), 0o600))

	e := &captureExporter{}
	tracer := tracing.NewTracer("trace-edit", e)
	root, ctx := tracer.Start(context.Background(), "cli.invocation", tracing.SpanKindInternal)

	reg := NewDefaultToolRegistry()
	require.NoError(t, reg.Register(ctx, NewEditFileTool()))

	// Execute is a method on *DefaultToolRegistry, not the ToolRegistry interface.
	dreg := reg.(*DefaultToolRegistry) //nolint:errcheck
	_, err := dreg.Execute(ctx, ToolCall{
		Name: "edit",
		Args: map[string]any{
			"file_path":  path,
			"old_string": "alpha",
			"new_string": "beta",
		},
	})
	require.NoError(t, err)

	root.SetStatus(tracing.SpanStatusOK, "")
	root.End()

	require.Eventually(t, func() bool { return e.count() >= 2 }, 2*time.Second, 10*time.Millisecond)
	assert.True(t, e.hasSpan("tool.call"), "expected a tool.call span to be exported")
}

func TestEditPreservesFilePermissions(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := filepath.Join(dir, "executable.sh")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\necho old\n"), 0o755)) //nolint:gosec // G306: executable bit is the fixture under test

	tool := NewEditFileTool()
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"file_path":  path,
			"old_string": "echo old",
			"new_string": "echo new",
		},
	})
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "echo new")
	assert.NotContains(t, string(data), "echo old")

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestEditDescription(t *testing.T) {
	tool := NewEditFileTool()
	desc := tool.Description()
	assert.Contains(t, desc, "edit")
	assert.Contains(t, desc, "old_string")
}

// TestEditWithFileTrackerOption verifies that WithEditFileTracker sets the
// fileTracker field so that Execute creates a backup checkpoint before
// editing an existing file.
func TestEditWithFileTrackerOption(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(path, []byte("package main\n\nvar version = 1\n"), 0o600))

	ft := NewFileTracker()
	tool := NewEditFileTool(WithEditFileTracker(ft))

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"file_path":  path,
			"old_string": "var version = 1",
			"new_string": "var version = 2",
		},
	})
	require.NoError(t, err)

	// Backup should have created a checkpoint for the existing file.
	checkpoints := ft.ListCheckpoints()
	require.Len(t, checkpoints, 1)
	assert.Equal(t, path, checkpoints[0].Path)
	assert.True(t, checkpoints[0].Existed)

	// The backup content should match the original file content.
	content, ok := ft.BackupContent(checkpoints[0].ID)
	require.True(t, ok)
	assert.Equal(t, "package main\n\nvar version = 1\n", string(content))
}

// --- Path whitelist + symlink security tests (SEC-04) ---

// TestEditRejectsAbsolutePathOutsideWhitelist verifies AC-1: an absolute path
// that falls outside the configured whitelist is rejected.
func TestEditRejectsAbsolutePathOutsideWhitelist(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	outside := t.TempDir()

	// Create a file outside the whitelist.
	outsideFile := filepath.Join(outside, "target.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("data"), 0o600))

	tool := NewEditFileTool(WithEditPathWhitelist([]string{dir}))

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"file_path":  outsideFile,
			"old_string": "data",
			"new_string": "modified",
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "whitelist")

	// File must be unchanged.
	data, rerr := os.ReadFile(outsideFile)
	require.NoError(t, rerr)
	assert.Equal(t, "data", string(data))
}

// TestEditRejectsSymlinkDirectoryEscape verifies AC-2: a symlink in an
// intermediate directory that resolves outside the whitelist is rejected.
func TestEditRejectsSymlinkDirectoryEscape(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	outside := t.TempDir()

	// Create a real file in the outside directory.
	realFile := filepath.Join(outside, "real.txt")
	require.NoError(t, os.WriteFile(realFile, []byte("original"), 0o600))

	// Create a symlink directory inside dir pointing to outside.
	linkDir := filepath.Join(dir, "escape")
	require.NoError(t, os.Symlink(outside, linkDir))

	tool := NewEditFileTool(WithEditPathWhitelist([]string{dir}))

	// Editing dir/escape/real.txt — the file itself is a regular file (so
	// Lstat won't flag it as a symlink), but resolveSymlinks resolves
	// dir/escape → outside which is not in the whitelist.
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"file_path":  filepath.Join(dir, "escape", "real.txt"),
			"old_string": "original",
			"new_string": "modified",
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "whitelist")

	// File must be unchanged.
	data, rerr := os.ReadFile(realFile)
	require.NoError(t, rerr)
	assert.Equal(t, "original", string(data))
}

// TestEditRejectsSymlinkTarget verifies AC-3: O_NOFOLLOW semantics — if the
// target file itself is a symlink, the edit is rejected even when the symlink
// points within the whitelist.
func TestEditRejectsSymlinkTarget(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()

	// Create a regular file and a symlink to it (both inside dir).
	target := filepath.Join(dir, "target.txt")
	require.NoError(t, os.WriteFile(target, []byte("original"), 0o600))
	link := filepath.Join(dir, "link.txt")
	require.NoError(t, os.Symlink(target, link))

	tool := NewEditFileTool(WithEditPathWhitelist([]string{dir}))

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"file_path":  link,
			"old_string": "original",
			"new_string": "modified",
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")

	// Original file must be unchanged.
	data, rerr := os.ReadFile(target)
	require.NoError(t, rerr)
	assert.Equal(t, "original", string(data))
}

// TestEditValidPathWithinWhitelist verifies that editing a valid file inside
// the whitelist succeeds.
func TestEditValidPathWithinWhitelist(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(path, []byte("package main\n"), 0o600))

	tool := NewEditFileTool(WithEditPathWhitelist([]string{dir}))

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"file_path":  path,
			"old_string": "package main",
			"new_string": "package main // edited",
		},
	})
	require.NoError(t, err)

	data, rerr := os.ReadFile(path)
	require.NoError(t, rerr)
	assert.Contains(t, string(data), "edited")
}
