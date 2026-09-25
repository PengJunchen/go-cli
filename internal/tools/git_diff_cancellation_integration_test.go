package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPreparedDefaultMutationCancellationDuringGitDiffPreservesFile(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("managed process groups require Darwin or Linux")
	}
	for _, toolName := range []string{"write", "edit"} {
		t.Run(toolName, func(t *testing.T) {
			root := t.TempDir()
			started := filepath.Join(root, "git-child-started")
			installFakeGit(t, t.TempDir(), fmt.Sprintf(`
case "$*" in
  *rev-parse*)
    /bin/sleep 3 &
    printf '%%s' "$!" > %q
    exit 0
    ;;
  *) exit 0 ;;
esac
`, started))
			path := filepath.Join(root, "existing.txt")
			const original = "old content\n"
			require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
			base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
			generator := NewGitDiffGenerator(NewDefaultGitTool(root), NewUnifiedDiffGenerator(0, false))
			require.NoError(t, RegisterDefaults(context.Background(), base,
				WithRegisteredBuiltinWhitelist([]string{toolName}), WithRegisteredDiffGenerator(generator)))
			registry := NewMiddlewareToolRegistry(base)
			queue := NewDefaultFileMutationQueue().(*DefaultFileMutationQueue) //nolint:errcheck
			t.Cleanup(func() { require.NoError(t, queue.Close()) })
			configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{MutationQueue: queue})
			args := map[string]any{"path": path, "content": "appended\n", "append": true}
			if toolName == "edit" {
				args = map[string]any{"file_path": path, "old_string": "old", "new_string": "new"}
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: toolName, Args: args})
			require.NoError(t, err)
			returned := make(chan error, 1)
			go func() {
				_, dispatchErr := invocation.Dispatch(ctx)
				returned <- dispatchErr
			}()
			require.Eventually(t, func() bool {
				_, statErr := os.Stat(started)
				return statErr == nil
			}, 3*time.Second, 5*time.Millisecond)
			cancel()
			select {
			case err = <-returned:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(5 * time.Second):
				t.Fatal("prepared dispatch did not return after cancellation")
			}
			// Dispatch may return before the queued executor. Inspect the file
			// only after its actual completion, catching a late ignored-cancel write.
			signal := invocation.CompletionSignal()
			require.NotNil(t, signal.Done())
			select {
			case <-signal.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("accepted queue job did not join its Git helper")
			}
			contents, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, original, string(contents), "canceled Git preview must not permit a late file mutation")
			assert.Equal(t, CompletionQueuedJobJoined, signal.Proof())
		})
	}
}
