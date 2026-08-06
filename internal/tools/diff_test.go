package tools

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestUnifiedDiffGeneratorImplementsInterface(t *testing.T) {
	var _ DiffGenerator = (*UnifiedDiffGenerator)(nil)
}

func TestDiffNewFileAllAddedLines(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	g := &UnifiedDiffGenerator{}
	out, err := g.Generate("", "line1\nline2\nline3\n", "new.txt")
	require.NoError(t, err)

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	require.Len(t, lines, 3)
	for _, l := range lines {
		assert.True(t, strings.HasPrefix(l, "+"), "expected line to start with '+', got %q", l)
	}
	assert.Contains(t, out, "+line1")
	assert.Contains(t, out, "+line2")
	assert.Contains(t, out, "+line3")
	assert.NotContains(t, out, "-line")
}

func TestDiffDeletedFileAllRemovedLines(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	g := &UnifiedDiffGenerator{}
	out, err := g.Generate("gone1\ngone2\n", "", "gone.txt")
	require.NoError(t, err)

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	require.Len(t, lines, 2)
	for _, l := range lines {
		assert.True(t, strings.HasPrefix(l, "-"), "expected line to start with '-', got %q", l)
	}
	assert.Contains(t, out, "-gone1")
	assert.Contains(t, out, "-gone2")
	assert.NotContains(t, out, "+gone")
}

func TestDiffModifiedFileHasAddAndRemove(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	g := &UnifiedDiffGenerator{}
	old := "alpha\nbeta\ngamma\n"
	new := "alpha\ndelta\ngamma\n"
	out, err := g.Generate(old, new, "mod.txt")
	require.NoError(t, err)

	assert.Contains(t, out, "--- a/mod.txt")
	assert.Contains(t, out, "+++ b/mod.txt")
	assert.Contains(t, out, "-beta")
	assert.Contains(t, out, "+delta")
	// Unchanged lines appear as context.
	assert.Contains(t, out, " alpha")
	assert.Contains(t, out, " gamma")
}

func TestDiffNoChangesReturnsEmpty(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	g := &UnifiedDiffGenerator{}
	out, err := g.Generate("same\ncontent\n", "same\ncontent\n", "same.txt")
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestDiffLargeFileTruncation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// Build 30 lines of old content that all change so the diff has 60 lines.
	var oldSB, newSB strings.Builder
	for i := 1; i <= 30; i++ {
		oldSB.WriteString("old")
		oldSB.WriteString(strconv.Itoa(i))
		oldSB.WriteString("\n")
		newSB.WriteString("new")
		newSB.WriteString(strconv.Itoa(i))
		newSB.WriteString("\n")
	}

	g := &UnifiedDiffGenerator{maxLines: 10}
	out, err := g.Generate(oldSB.String(), newSB.String(), "big.txt")
	require.NoError(t, err)

	assert.Contains(t, out, "...")
	// A middle line that should have been truncated away.
	assert.NotContains(t, out, "old15")
	assert.NotContains(t, out, "new15")
	// First and last lines should still be present.
	assert.Contains(t, out, "old1")
	assert.Contains(t, out, "new30")

	// Compare against an unbounded generator: truncated output must be shorter.
	unbounded := &UnifiedDiffGenerator{}
	full, err := unbounded.Generate(oldSB.String(), newSB.String(), "big.txt")
	require.NoError(t, err)
	assert.Less(t, len(out), len(full))
}

func TestDiffColorOutputContainsANSICodes(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	g := &UnifiedDiffGenerator{color: true}
	out, err := g.Generate("a\n", "b\n", "color.txt")
	require.NoError(t, err)

	assert.Contains(t, out, "\033[31m") // red
	assert.Contains(t, out, "\033[32m") // green
	assert.Contains(t, out, "\033[0m")  // reset
}

func TestDiffNoColorHasNoANSICodes(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	g := &UnifiedDiffGenerator{color: false}
	out, err := g.Generate("a\n", "b\n", "plain.txt")
	require.NoError(t, err)

	assert.NotContains(t, out, "\033[")
}

func TestDiffPathInHeader(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	g := &UnifiedDiffGenerator{}
	out, err := g.Generate("x\n", "y\n", "src/main.go")
	require.NoError(t, err)

	assert.Contains(t, out, "--- a/src/main.go")
	assert.Contains(t, out, "+++ b/src/main.go")
}

func TestDiffAppendsAndRemovesMultipleLines(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	g := &UnifiedDiffGenerator{}
	old := "keep\nremove1\nremove2\nkeep2\n"
	new := "keep\nadd1\nadd2\nkeep2\n"
	out, err := g.Generate(old, new, "multi.txt")
	require.NoError(t, err)

	assert.Contains(t, out, "-remove1")
	assert.Contains(t, out, "-remove2")
	assert.Contains(t, out, "+add1")
	assert.Contains(t, out, "+add2")
	assert.Contains(t, out, " keep")
	assert.Contains(t, out, " keep2")
}

func TestWriteToolDiffPreviewInMetadata(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	require.NoError(t, os.WriteFile(path, []byte("alpha\nbeta\n"), 0o600))

	tool := NewWriteTool(
		WithWriteWorkdir(dir),
		WithOverwrite(true),
		WithDiffGenerator(&UnifiedDiffGenerator{}),
	)
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "out.txt", "content": "alpha\ngamma\n"},
	})
	require.NoError(t, err)

	diff, ok := res.Metadata["diff"].(string)
	require.True(t, ok, "expected diff in metadata, got %v", res.Metadata)
	assert.Contains(t, diff, "--- a/out.txt")
	assert.Contains(t, diff, "+++ b/out.txt")
	assert.Contains(t, diff, "-beta")
	assert.Contains(t, diff, "+gamma")

	data, rerr := os.ReadFile(path)
	require.NoError(t, rerr)
	assert.Equal(t, "alpha\ngamma\n", string(data))
}

func TestWriteToolDiffSkippedForNewFile(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	tool := NewWriteTool(
		WithWriteWorkdir(dir),
		WithDiffGenerator(&UnifiedDiffGenerator{}),
	)
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "fresh.txt", "content": "hello\n"},
	})
	require.NoError(t, err)

	_, hasDiff := res.Metadata["diff"]
	assert.False(t, hasDiff, "new file should not produce a diff preview")
}

func TestWriteToolNoDiffWhenGeneratorNil(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	require.NoError(t, os.WriteFile(path, []byte("old\n"), 0o600))

	tool := NewWriteTool(WithWriteWorkdir(dir), WithOverwrite(true))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": "out.txt", "content": "new\n"},
	})
	require.NoError(t, err)

	_, hasDiff := res.Metadata["diff"]
	assert.False(t, hasDiff, "nil diff generator should not produce a diff")
}

func TestEditToolDiffPreviewInMetadata(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(path, []byte("package main\n\nvar version = 1\n"), 0o600))

	tool := NewEditFileTool(WithEditDiffGenerator(&UnifiedDiffGenerator{}))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"file_path":  path,
			"old_string": "var version = 1",
			"new_string": "var version = 2",
		},
	})
	require.NoError(t, err)

	diff, ok := res.Metadata["diff"].(string)
	require.True(t, ok, "expected diff in metadata, got %v", res.Metadata)
	assert.Contains(t, diff, "-var version = 1")
	assert.Contains(t, diff, "+var version = 2")

	data, rerr := os.ReadFile(path)
	require.NoError(t, rerr)
	assert.Contains(t, string(data), "var version = 2")
}

func TestEditToolNoDiffWhenGeneratorNil(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello\n"), 0o600))

	tool := NewEditFileTool()
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"file_path":  path,
			"old_string": "hello",
			"new_string": "world",
		},
	})
	require.NoError(t, err)

	_, hasDiff := res.Metadata["diff"]
	assert.False(t, hasDiff, "nil diff generator should not produce a diff")
}
