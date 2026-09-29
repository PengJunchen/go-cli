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
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("later file\n"), 0o600))

	ctx := &cancelDuringWalkContext{
		Context:        context.Background(),
		done:           make(chan struct{}),
		cancelErrAfter: 11, // cancel after the first file's matching line.
	}
	tool := NewGrepTool(WithGrepWorkdir(dir), WithForcePureGo(true))

	result, err := tool.Execute(ctx, ToolCall{Args: map[string]any{"pattern": "needle"}})
	select {
	case <-ctx.done:
	default:
		t.Fatal("cancellation did not occur before GrepTool returned")
	}
	require.ErrorIs(t, err, context.Canceled)
	if result != nil {
		require.Empty(t, result.Output)
	}
}

// cancelDuringWalkContext cancels after the first file's matching line.
// Tests inspect done directly so an assertion cannot itself trigger it.
type cancelDuringWalkContext struct {
	context.Context
	done            chan struct{}
	doneChecks      int
	errChecks       int
	cancelDoneAfter int
	cancelErrAfter  int
	closeOnce       sync.Once
}

func (c *cancelDuringWalkContext) Done() <-chan struct{} {
	c.doneChecks++
	if c.cancelDoneAfter > 0 && c.doneChecks == c.cancelDoneAfter {
		c.closeOnce.Do(func() { close(c.done) })
	}
	return c.done
}

func (c *cancelDuringWalkContext) Err() error {
	c.errChecks++
	if c.cancelErrAfter > 0 && c.errChecks == c.cancelErrAfter {
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
	require.ErrorIs(t, err, errGrepCaptureLimit)
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
