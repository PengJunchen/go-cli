package tools

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGrepPureGoPreCanceledContextIsNotSuccessfulPartialResult(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "match.txt"), []byte("needle\n"), 0o600))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tool := NewGrepTool(WithGrepWorkdir(dir), WithForcePureGo(true))

	result, err := tool.Execute(ctx, ToolCall{Args: map[string]any{"pattern": "needle"}})
	require.ErrorIs(t, err, context.Canceled)
	if result != nil {
		require.Empty(t, result.Output)
	}
}

func TestGrepPureGoMidWalkCancellationDoesNotReturnPartialSuccess(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("needle first\nsecond line\n"), 0o600))

	ctx := &cancelAfterErrContext{
		Context:     context.Background(),
		done:        make(chan struct{}),
		cancelAfter: 5, // after the first matching line, before scanning the second.
	}
	tool := NewGrepTool(WithGrepWorkdir(dir), WithForcePureGo(true))

	result, err := tool.Execute(ctx, ToolCall{Args: map[string]any{"pattern": "needle"}})
	require.ErrorIs(t, err, context.Canceled)
	if result != nil {
		require.Empty(t, result.Output)
	}
}

type cancelAfterErrContext struct {
	context.Context
	done        chan struct{}
	errChecks   int
	cancelAfter int
	closeOnce   sync.Once
}

func (c *cancelAfterErrContext) Done() <-chan struct{} { return c.done }

func (c *cancelAfterErrContext) Err() error {
	c.errChecks++
	if c.cancelAfter > 0 && c.errChecks == c.cancelAfter {
		c.closeOnce.Do(func() { close(c.done) })
	}
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

func TestGrepPureGoScannerErrorDoesNotReturnPartialSuccess(t *testing.T) {
	dir := t.TempDir()
	content := append([]byte("needle first\n"), bytes.Repeat([]byte("x"), 1024*1024+1)...)
	content = append(content, '\n')
	require.NoError(t, os.WriteFile(filepath.Join(dir, "large.txt"), content, 0o600))
	tool := NewGrepTool(WithGrepWorkdir(dir), WithForcePureGo(true))

	result, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"pattern": "needle"}})
	require.Error(t, err)
	require.ErrorContains(t, err, "token too long")
	if result != nil {
		require.Empty(t, result.Output)
	}
}

func TestGrepPureGoDoesNotReadNestedSymlinkOutsideWhitelist(t *testing.T) {
	allowedDir := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "private.txt")
	const sentinel = "outside-scope-sentinel"
	require.NoError(t, os.WriteFile(outsideFile, []byte(sentinel+"\n"), 0o600))
	require.NoError(t, os.Symlink(outsideFile, filepath.Join(allowedDir, "linked.txt")))

	tool := NewGrepTool(
		WithGrepWorkdir(allowedDir),
		WithForcePureGo(true),
		WithGrepPathWhitelist([]string{allowedDir}),
	)
	result, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": allowedDir, "pattern": sentinel},
	})
	require.NoError(t, err)
	require.NotContains(t, result.Output, sentinel)
}
