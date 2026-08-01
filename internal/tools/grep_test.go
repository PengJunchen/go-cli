package tools

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// setupGrepDir creates a temp dir with files that do and do not contain the
// target token, returning the dir path.
func setupGrepDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n// TODO: fix me\nvar x = 1\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.go"), []byte("package b\n\nvar y = 2\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "c.txt"), []byte("plain text\nTODO here too\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "skip.md"), []byte("TODO in markdown\n"), 0o600))
	return dir
}

func TestGrepPureGoFindsMatches(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupGrepDir(t)
	tool := NewGrepTool(WithGrepWorkdir(dir), WithForcePureGo(true))

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"pattern": "TODO"},
	})
	require.NoError(t, err)

	assert.Contains(t, res.Output, "a.go:2:// TODO: fix me")
	assert.Contains(t, res.Output, "c.txt:2:TODO here too")
	assert.Contains(t, res.Output, "skip.md:1:TODO in markdown")
	// b.go has no TODO.
	assert.NotContains(t, res.Output, "b.go")

	assert.Equal(t, 3, res.Metadata["matches"])
}

func TestGrepPureGoRespectsPathAndGlob(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupGrepDir(t)
	tool := NewGrepTool(WithGrepWorkdir(dir), WithForcePureGo(true))

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"pattern": "TODO",
			"glob":    "*.go",
		},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "a.go:2:// TODO: fix me")
	assert.NotContains(t, res.Output, ".txt")
	assert.NotContains(t, res.Output, ".md")

	res2, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"pattern": "TODO",
			"path":    ".",
			"glob":    "*.txt",
		},
	})
	require.NoError(t, err)
	assert.Contains(t, res2.Output, "c.txt:2:TODO here too")
	assert.NotContains(t, res2.Output, "a.go")
}

func TestGrepNoMatchesReturnsEmpty(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupGrepDir(t)
	tool := NewGrepTool(WithGrepWorkdir(dir), WithForcePureGo(true))

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"pattern": "NOT_PRESENT_ANYWHERE"},
	})
	require.NoError(t, err)
	assert.Equal(t, "", res.Output)
	assert.Equal(t, 0, res.Metadata["matches"])
}

func TestGrepInvalidPattern(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewGrepTool(WithForcePureGo(true))
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"pattern": "["},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid pattern")
}

func TestGrepMissingPattern(t *testing.T) {
	tool := NewGrepTool(WithForcePureGo(true))
	_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{}})
	assert.Error(t, err)
}

func TestGrepTruncatesByMatchCount(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	var sb strings.Builder
	for i := 0; i < 100; i++ {
		sb.WriteString("match-line\n")
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "big.txt"), []byte(sb.String()), 0o600))

	tool := NewGrepTool(WithGrepWorkdir(dir), WithForcePureGo(true), WithGrepMaxMatches(10))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"pattern": "match-line"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "[results truncated]")
	assert.Contains(t, res.Output, "big.txt:1:match-line")
	truncated, ok := res.Metadata["truncated"].(bool)
	require.True(t, ok)
	assert.True(t, truncated)
}

func TestGrepDefaultPathBehavior(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// Without forcing pure-Go, the tool probes for rg and falls back to Go if
	// rg is missing or fails. Either way it must produce identical output.
	dir := setupGrepDir(t)
	tool := NewGrepTool(WithGrepWorkdir(dir))

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"pattern": "TODO"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "a.go:2:// TODO: fix me")
	assert.Contains(t, res.Output, "c.txt:2:TODO here too")
}

func TestGrepEmitsToolCallSpan(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupGrepDir(t)

	e := &captureExporter{}
	tracer := tracing.NewTracer("trace-grep", e)
	root, ctx := tracer.Start(context.Background(), "cli.invocation", tracing.SpanKindInternal)

	reg := NewDefaultToolRegistry()
	require.NoError(t, reg.Register(ctx, NewGrepTool(WithGrepWorkdir(dir), WithForcePureGo(true))))

	_, err := reg.Execute(ctx, ToolCall{
		Name: "grep",
		Args: map[string]any{"pattern": "TODO"},
	})
	require.NoError(t, err)

	root.SetStatus(tracing.SpanStatusOK, "")
	root.End()

	require.Eventually(t, func() bool { return e.count() >= 2 }, 2*time.Second, 10*time.Millisecond)
	assert.True(t, e.hasSpan("tool.call"), "expected a tool.call span to be exported")
}

func TestGrepPureGoFunctionDirectly(t *testing.T) {
	dir := setupGrepDir(t)
	re := mustCompile(t, "TODO")

	ms := grepPureGo(context.Background(), re, dir, "")
	got := map[string]bool{}
	for _, m := range ms {
		got[strings.Join([]string{filepath.Base(m.File), strconv.Itoa(m.Line), m.Content}, ":")] = true
	}
	assert.True(t, got["a.go:2:// TODO: fix me"])
	assert.True(t, got["c.txt:2:TODO here too"])
	assert.True(t, got["skip.md:1:TODO in markdown"])
}

func TestParseRipgrepOutput(t *testing.T) {
	ms := parseRipgrepOutput([]byte("a.go:2: // TODO\nb.txt:10:TODO\n"), "TODO")
	require.Len(t, ms, 2)
	assert.Equal(t, "a.go", ms[0].File)
	assert.Equal(t, 2, ms[0].Line)
	assert.Equal(t, " // TODO", ms[0].Content)
	assert.Equal(t, "b.txt", ms[1].File)
	assert.Equal(t, 10, ms[1].Line)
	assert.Equal(t, "TODO", ms[1].Content)
}

func TestGrepName(t *testing.T) {
	assert.Equal(t, "grep", NewGrepTool().Name())
	assert.Contains(t, NewGrepTool().Description(), "pattern")
}

func mustCompile(t *testing.T, pattern string) *regexp.Regexp {
	t.Helper()
	re, err := regexp.Compile(pattern)
	require.NoError(t, err)
	return re
}
