package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBashRuntimeInputChecksGuardAfterIdentityBeforeUserCode(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "forbidden")
	revoked := false
	denied := errors.New("runtime authority revoked")
	ctx := WithProcessExecutionIdentityObserver(t.Context(), func(context.Context, ProcessExecutionIdentity) error { revoked = true; return nil })
	checked := false
	ctx = WithBashRuntimeInput(ctx, "$(touch should-not-run)\n'\"", func(ctx context.Context, start func() error) error {
		checked = true
		require.True(t, revoked)
		return denied
	})
	tool := NewBashTool(WithNoSandbox())
	_, err := tool.Execute(ctx, ToolCall{Name: "bash", Args: map[string]any{"command": fmt.Sprintf("cat > %q", marker)}})
	require.ErrorIs(t, err, denied)
	require.True(t, checked)
	_, err = os.Stat(marker)
	require.True(t, os.IsNotExist(err), "user command must remain behind release guard")
}

func TestBashRuntimeInputFeedsLargeLiteralStdin(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "payload")
			payload := "'\"\n$(touch should-not-run)\n" + string(make([]byte, 400000))
			checked := 0
			ctx := WithBashRuntimeInput(t.Context(), payload, func(_ context.Context, start func() error) error { checked++; return start() })
			var tool ToolDefinition = NewBashTool(WithNoSandbox())
			if streaming {
				tool = NewStreamingBashTool(WithNoSandbox())
			}
			_, err := tool.Execute(ctx, ToolCall{Name: "bash", Args: map[string]any{"command": fmt.Sprintf("cat > %q", marker)}})
			require.NoError(t, err)
			require.Equal(t, 1, checked)
			data, err := os.ReadFile(marker)
			require.NoError(t, err)
			require.Equal(t, payload, string(data))
		})
	}
}

func TestBashRuntimeExecutionTimeoutDoesNotExtendToolLimit(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			ctx := WithBashRuntimeExecutionTimeout(t.Context(), time.Second)
			var tool ToolDefinition = NewBashTool(WithNoSandbox(), WithTimeout(100*time.Millisecond))
			if streaming {
				tool = NewStreamingBashTool(WithNoSandbox(), WithTimeout(100*time.Millisecond))
			}
			started := time.Now()
			_, err := tool.Execute(ctx, ToolCall{Name: "bash", Args: map[string]any{"command": "sleep 2"}})
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.Less(t, time.Since(started), time.Second, "runtime hook budget must not extend the tool's existing execution cap")
		})
	}
}
