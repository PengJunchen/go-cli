//go:build darwin

package tools

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestResourceMemoryUnsupportedDarwin(t *testing.T) {
	for _, mode := range resourceLimitModes {
		t.Run(mode, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "unexpected")
			call := ToolCall{ID: "resource-darwin-memory", Name: "bash", Args: map[string]any{"command": "printf ran > \"$LIMIT_MARKER\""}}
			_, invocation, err := runResourceLimitMode(context.Background(), t, mode, call, WithNoSandbox(), WithResourceLimits(ResourceLimits{MaxCPU: 2 * time.Second, MaxMemory: 256 << 20}), WithEnv(map[string]string{"LIMIT_MARKER": marker}))
			require.Error(t, err)
			require.Contains(t, err.Error(), "unsupported")
			require.NoFileExists(t, marker)
			if invocation != nil {
				require.False(t, PreparedBashOwnTimeoutKnown(invocation, err))
				require.Equal(t, CompletionInlinePipelineJoined, invocation.CompletionSignal().Proof())
			}
		})
	}
}
