//go:build darwin || linux

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

// Direct-OS companion to the durable-core recovery RED. It deliberately has
// no descendants and never treats an interrupted command as success/rollback.
func TestBashUnpreparedOwnTimeoutRemainsConservative(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("real managed process group contract requires Linux or Darwin")
	}
	for _, mode := range []string{"ordinary", "streaming"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			pidPath := filepath.Join(root, "leader.pid")
			counterPath := filepath.Join(root, "one-effect.count")
			parent, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			options := []BashToolOption{WithTimeout(time.Second), WithNoSandbox(), WithBashWorkdir(root), WithEnv(map[string]string{"TIMEOUT_CONTRACT_PID": pidPath, "TIMEOUT_CONTRACT_COUNT": counterPath})}
			call := ToolCall{ID: "own-timeout", Name: "bash", Args: map[string]any{"command": `printf 'effect\n' >> "$TIMEOUT_CONTRACT_COUNT"; printf 'timeout-sentinel\n'; printf '%s' "$$" > "$TIMEOUT_CONTRACT_PID"; exec sleep 10`}}
			var result *ToolResult
			var err error
			if mode == "ordinary" {
				result, err = NewBashTool(options...).Execute(parent, call)
			} else {
				result, err = NewStreamingBashTool(options...).ExecuteStreaming(parent, call, nil)
			}
			require.NoError(t, parent.Err())
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.NotNil(t, result)
			require.Contains(t, result.Output, "timeout-sentinel", "the real command must have started before semantic assertions")
			require.Contains(t, err.Error(), "timed out")
			pidBytes, readErr := os.ReadFile(pidPath)
			require.NoError(t, readErr)
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
			require.NoError(t, parseErr)
			require.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH)
			require.ErrorIs(t, syscall.Kill(-pid, 0), syscall.ESRCH)
			effect, readErr := os.ReadFile(counterPath)
			require.NoError(t, readErr)
			require.Equal(t, "effect\n", string(effect), "one actual pre-timeout effect, neither rollback nor replay")
			var pending PendingExecutionError
			require.False(t, errors.As(err, &pending), "the owned leader/group/readers must already be joined")
			var outcome interface{ OutcomeUnknown() bool }
			require.True(t, errors.As(err, &outcome), "the local failure must carry an explicit outcome classification")
			require.True(t, outcome.OutcomeUnknown(), "an unprepared direct call lacks current sealed invocation authority; full join alone does not grant core recovery")
		})
	}
}
