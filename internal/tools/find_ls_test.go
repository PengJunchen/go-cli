package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// setupFindDir creates a nested temp tree for find/ls assertions.
func setupFindDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.go"), []byte("b"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "c.go"), []byte("c"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "d.txt"), []byte("d"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub", "deep"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "deep", "e.txt"), []byte("e"), 0o600))
	return dir
}

func TestFindPureGoMatchesPaths(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))

	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{}})
	require.NoError(t, err)

	for _, want := range []string{"a.txt", "b.go", "sub", "sub/c.go", "sub/d.txt", "sub/deep", "sub/deep/e.txt"} {
		assert.Contains(t, res.Output, want, "expected %q in output", want)
	}
	assert.Equal(t, 7, res.Metadata["matches"])
}

func TestFindPureGoMatchesPattern(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))

	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"pattern": "*.go"}})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "b.go")
	assert.Contains(t, res.Output, "sub/c.go")
	assert.NotContains(t, res.Output, ".txt")
}

func TestFindPureGoFiltersByTypeAndDepth(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))

	// type f returns only files.
	resDirs, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"type": "d"}})
	require.NoError(t, err)
	assert.Contains(t, resDirs.Output, "sub")
	assert.Contains(t, resDirs.Output, "sub/deep")
	assert.NotContains(t, resDirs.Output, ".txt")
	assert.NotContains(t, resDirs.Output, ".go")

	// max_depth 1 limits traversal to the top level.
	resShallow, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"max_depth": 1}})
	require.NoError(t, err)
	assert.Contains(t, resShallow.Output, "sub")
	assert.NotContains(t, resShallow.Output, "sub/c.go")
}

func TestFindSingleFile(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))

	filePath := filepath.Join(dir, "a.txt")
	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": filePath, "pattern": "*.txt"}})
	require.NoError(t, err)
	assert.Equal(t, filePath, res.Output)
}

func TestFindEmitsFindSpan(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	e := &captureExporter{}
	tracer := tracing.NewTracer("trace-find", e)
	root, ctx := tracer.Start(context.Background(), "cli.invocation", tracing.SpanKindInternal)

	dir := setupFindDir(t)
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))
	_, err := tool.Execute(ctx, ToolCall{Args: map[string]any{"pattern": "*.go"}})
	require.NoError(t, err)

	root.SetStatus(tracing.SpanStatusOK, "")
	root.End()

	require.Eventually(t, func() bool { return e.hasSpan("tool.call") }, 2*time.Second, 10*time.Millisecond)
}

func TestLSListsDirectory(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewLSTool()
	// Workdir isn't used by NewLSTool's default; pass an explicit path.
	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": dir}})
	require.NoError(t, err)

	for _, want := range []string{"a.txt", "b.go", "sub/"} {
		assert.Contains(t, res.Output, want)
	}
	assert.NotContains(t, res.Output, "c.go") // not an immediate child
	assert.Equal(t, 3, res.Metadata["entries"])
}

func TestLSAllIncludesDotfiles(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".hidden"), []byte("h"), 0o600))

	tool := NewLSTool()
	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": dir, "all": true}})
	require.NoError(t, err)
	assert.Contains(t, res.Output, ".hidden")
	assert.Equal(t, 2, res.Metadata["entries"])

	resNoAll, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": dir}})
	require.NoError(t, err)
	assert.NotContains(t, resNoAll.Output, ".hidden")
	assert.Equal(t, 1, resNoAll.Metadata["entries"])
}

func TestLSLongFormat(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewLSTool()
	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": dir, "long": true}})
	require.NoError(t, err)
	// Long format includes a mode string and size.
	assert.Contains(t, res.Output, "-rw")
	assert.Contains(t, res.Output, "a.txt")
}

func TestLSSortBehaviors(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewLSTool()

	// name sort is default and stable.
	resName, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": dir}})
	require.NoError(t, err)
	assert.True(t, strings.Index(resName.Output, "a.txt") < strings.Index(resName.Output, "sub"))

	// size sort orders single-byte files before the (larger) directory listing.
	resSize, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": dir, "sort": "size"}})
	require.NoError(t, err)
	assert.Contains(t, resSize.Output, "a.txt")

	// time sort is accepted without error.
	_, err = tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": dir, "sort": "time"}})
	require.NoError(t, err)

	// invalid sort errors.
	_, err = tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": dir, "sort": "bogus"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid sort")
}

func TestLSNonDirectoryErrors(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewLSTool()
	_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": filepath.Join(dir, "a.txt")}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}

func TestLSEmitsLSSpan(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	e := &captureExporter{}
	tracer := tracing.NewTracer("trace-ls", e)
	root, ctx := tracer.Start(context.Background(), "cli.invocation", tracing.SpanKindInternal)

	tool := NewLSTool()
	dir := setupFindDir(t)
	_, err := tool.Execute(ctx, ToolCall{Args: map[string]any{"path": dir}})
	require.NoError(t, err)

	root.SetStatus(tracing.SpanStatusOK, "")
	root.End()

	require.Eventually(t, func() bool { return e.hasSpan("tool.call") }, 2*time.Second, 10*time.Millisecond)
}

func TestFindAndLSName(t *testing.T) {
	assert.Equal(t, "find", NewFindTool().Name())
	assert.Contains(t, NewFindTool().Description(), "pattern")
	assert.Equal(t, "ls", NewLSTool().Name())
	assert.Contains(t, NewLSTool().Description(), "path")
}

func TestFindPureGoNonExistentPath(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewFindTool(WithFindForceNode(true))
	_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": "/nonexistent/path/xyz"}})
	require.Error(t, err)
}

func TestFindPureGoCancelledContext(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tool.Execute(ctx, ToolCall{Args: map[string]any{}})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestFindPureGoTypeFile(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))

	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"type": "f"}})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "a.txt")
	assert.Contains(t, res.Output, "b.go")
	// Directories should be excluded, but file paths under subdirectories remain.
	assert.NotContains(t, res.Output, "sub\n")
	assert.NotContains(t, res.Output, "sub/deep\n")
}

func TestFindSingleFileNoMatch(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))

	filePath := filepath.Join(dir, "a.txt")
	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": filePath, "pattern": "*.go"}})
	require.NoError(t, err)
	assert.Empty(t, res.Output)
}

func TestFindPureGoEmptyPatternMatchesAll(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))

	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"pattern": ""}})
	require.NoError(t, err)
	assert.Equal(t, 7, res.Metadata["matches"])
}

func TestFindMaxResultsTruncation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))

	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{}})
	require.NoError(t, err)
	// All 7 entries fit under the findMaxResults cap.
	assert.Equal(t, 7, res.Metadata["matches"])
}

func TestLSEmptyDirectory(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	tool := NewLSTool()
	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": dir}})
	require.NoError(t, err)
	assert.Equal(t, 0, res.Metadata["entries"])
}
