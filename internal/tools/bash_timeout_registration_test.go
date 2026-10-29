package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRegisteredBashTimeoutDefaultsAndExplicitConfiguration(t *testing.T) {
	ctx := context.Background()
	for _, configured := range []bool{false, true} {
		registry := NewDefaultToolRegistry()
		options := []RegisterDefaultsOption{WithRegisteredBuiltinWhitelist([]string{"bash"})}
		if configured {
			options = append(options, WithRegisteredBashTimeout(5*time.Minute))
		}
		require.NoError(t, RegisterDefaults(ctx, registry, options...))
		definition, err := registry.Get(ctx, "bash")
		require.NoError(t, err)
		bash, ok := definition.(*StreamingBashToolImpl)
		require.True(t, ok)
		expected := 30 * time.Second
		if configured {
			expected = 5 * time.Minute
		}
		require.Equal(t, expected, bash.Timeout)
		require.False(t, bash.TimeoutTier)
		schema, ok := bash.Parameters().(map[string]any)
		require.True(t, ok)
		require.Equal(t, map[string]any{"command": map[string]any{"type": "string"}}, schema["properties"])
	}
	for _, invalid := range []time.Duration{0, -time.Second} {
		registry := NewDefaultToolRegistry()
		require.Error(t, RegisterDefaults(ctx, registry, WithRegisteredBashTimeout(invalid)))
		definitions, err := registry.List(ctx)
		require.NoError(t, err)
		require.Empty(t, definitions, "an invalid explicit option must fail before any tool is registered")
	}
}

func TestRegisteredBashHostWindowActualOSExecution(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("managed process groups require Darwin or Linux")
	}
	for _, mode := range []string{"ordinary", "stream-execute", "stream-sink"} {
		for _, window := range []time.Duration{500 * time.Millisecond, 3 * time.Second} {
			t.Run(mode+"/"+window.String(), func(t *testing.T) {
				root := t.TempDir()
				pidPath := filepath.Join(root, "pid")
				options := []BashToolOption{WithTimeout(window), WithNoSandbox(), WithBashWorkdir(root), WithEnv(map[string]string{"BASH_TIMEOUT_PID_FILE": pidPath})}
				call := ToolCall{ID: "host-window", Name: "bash", Args: map[string]any{"command": `printf '%s' "$$" > "$BASH_TIMEOUT_PID_FILE"; exec sleep 1.2`}}
				var result *ToolResult
				var err error
				if mode == "ordinary" {
					result, err = NewBashTool(options...).Execute(t.Context(), call)
				} else {
					tool := NewStreamingBashTool(options...)
					if mode == "stream-execute" {
						result, err = tool.Execute(t.Context(), call)
					} else {
						result, err = tool.ExecuteStreaming(t.Context(), call, nil)
					}
				}
				require.NotNil(t, result)
				if window < time.Second {
					require.ErrorIs(t, err, context.DeadlineExceeded)
					var pending PendingExecutionError
					require.False(t, errors.As(err, &pending), "foreground exec must join its managed process before returning")
				} else {
					require.NoError(t, err)
					require.Equal(t, 0, result.Metadata["exit_code"])
				}
				data, err := os.ReadFile(pidPath)
				require.NoError(t, err, "the real OS child entered the synthetic command")
				pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
				require.NoError(t, err)
				require.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH, "child is gone before the tool returns, not after external cleanup")
			})
		}
	}
}

func TestRegisteredBashRuntimeAndParentOnlyShortenWindow(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("managed process groups require Darwin or Linux")
	}
	for _, scenario := range []struct {
		name           string
		host, runtime  time.Duration
		parentDeadline bool
	}{
		{"host-shorter", 500 * time.Millisecond, 3 * time.Second, false},
		{"runtime-shorter", 3 * time.Second, 500 * time.Millisecond, false},
		{"parent-shorter", 3 * time.Second, 3 * time.Second, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := t.Context()
			if scenario.parentDeadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 500*time.Millisecond)
				defer cancel()
			}
			ctx = WithBashRuntimeExecutionTimeout(ctx, scenario.runtime)
			tool := NewStreamingBashTool(WithTimeout(scenario.host), WithNoSandbox(), WithBashWorkdir(t.TempDir()))
			_, err := tool.Execute(ctx, ToolCall{ID: "shortened", Name: "bash", Args: map[string]any{"command": "exec sleep 1.2"}})
			require.ErrorIs(t, err, context.DeadlineExceeded)
			var pending PendingExecutionError
			require.False(t, errors.As(err, &pending))
		})
	}
}
