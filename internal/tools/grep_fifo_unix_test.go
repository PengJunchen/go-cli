//go:build darwin || linux

package tools

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGrepPureGoDoesNotBlockOpeningFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "a.pipe")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	tool := NewGrepTool(WithGrepWorkdir(dir), WithForcePureGo(true))

	type response struct {
		result *ToolResult
		err    error
	}
	done := make(chan response, 1)
	go func() {
		result, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"pattern": "needle"}})
		done <- response{result: result, err: err}
	}()

	select {
	case got := <-done:
		require.NoError(t, got.err)
		require.NotNil(t, got.result)
		require.Empty(t, got.result.Output)
	case <-time.After(200 * time.Millisecond):
		// Release the old implementation's blocking read so the test process
		// cannot leak a goroutine after reporting the regression.
		writer, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = writer.Close()
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("GrepTool remained blocked after the FIFO writer was released")
		}
		t.Fatal("GrepTool blocked while opening a FIFO instead of skipping the special file")
	}
}
