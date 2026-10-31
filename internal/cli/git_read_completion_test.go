package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestDurableGitStatusAndParallelReadsReachFollowupWithQuiescentActions(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("owned process groups require Darwin or Linux")
	}
	for _, parallel := range []bool{false, true} {
		name := "single status"
		if parallel {
			name = "parallel bash read status read"
		}
		t.Run(name, func(t *testing.T) {
			defer verify.AssertNoGoroutineLeak(t)()
			configureDurableCLISessionTest(t)
			root := t.TempDir()
			t.Chdir(root)
			require.NoError(t, os.WriteFile(filepath.Join(root, "sample.txt"), []byte("synthetic local fixture\n"), 0o600))
			require.NoError(t, exec.Command("git", "init", "-q").Run())
			calls := []map[string]any{{"id": "status", "type": "function", "function": map[string]any{"name": "git_status", "arguments": "{}"}}}
			if parallel {
				calls = append(calls,
					map[string]any{"id": "bash", "type": "function", "function": map[string]any{"name": "bash", "arguments": `{"command":"printf managed"}`}},
					map[string]any{"id": "read-1", "type": "function", "function": map[string]any{"name": "read", "arguments": `{"path":"sample.txt"}`}},
					map[string]any{"id": "read-2", "type": "function", "function": map[string]any{"name": "read", "arguments": `{"path":"sample.txt"}`}},
				)
			}
			first, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": calls}}}})
			require.NoError(t, err)
			server, script := newDurablePromptServer(t, string(first), `{"choices":[{"message":{"role":"assistant","content":"managed followup"},"finish_reason":"stop"}]}`)
			defer server.Close()
			store := filepath.Join(t.TempDir(), "sessions")
			cfg := durablePromptConfig(t, server.URL, store)
			require.False(t, cfg.Memory.AutomaticEnabled(), "the synthetic fixture must not inject or extract cross-session memory")
			cfg.Git.WorkDir = root
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			out := &bytes.Buffer{}
			err = newPromptCmd(out).Run(ctx, cfg, []string{"--approve", "auto", "--no-sandbox", "inspect the synthetic local fixture"})
			require.NoError(t, err)
			require.Contains(t, out.String(), "managed followup")
			require.Len(t, script.requestBodies(), 2)
			physicalID := physicalSessionIDFromOutput(t, out.String())
			resumed, err := newDurableCLISession(t.Context(), store, "inspect", physicalID, "")
			require.NoError(t, err)
			snapshot, err := resumed.snapshotSelected()
			require.NoError(t, err)
			pending, err := session.InspectPendingActions(snapshot)
			require.NoError(t, err)
			require.Empty(t, pending, "every dispatched action requires its own resource resolution")
			require.NoError(t, resumed.requestClose(t.Context()))
		})
	}
}
