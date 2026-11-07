package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/session"
)

func mixedDelegationResponse(t *testing.T, order []string, root string) string {
	t.Helper()
	calls := make([]any, 0, len(order))
	for _, id := range order {
		name := "read"
		args := map[string]any{"path": filepath.Join(root, id+".txt")}
		if strings.HasPrefix(id, "delegate") {
			name = "dispatch_subagent"
			args = map[string]any{"prompt": "find the two files", "role": "researcher", "tools": []string{"find"}}
		}
		encoded, err := json.Marshal(args)
		require.NoError(t, err)
		calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(encoded)}})
	}
	response, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": calls}}}})
	require.NoError(t, err)
	return string(response)
}

func TestDurableMixedDelegateAndReadersPreserveOrderAndComplete(t *testing.T) {
	for _, order := range [][]string{{"delegate", "read-one", "read-two"}, {"read-one", "delegate", "read-two"}, {"read-one", "delegate-one", "read-two", "delegate-two"}} {
		t.Run(strings.Join(order, "-"), func(t *testing.T) {
			configureDurableCLISessionTest(t)
			root, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			t.Chdir(root)
			for _, name := range []string{"read-one", "read-two"} {
				require.NoError(t, os.WriteFile(filepath.Join(root, name+".txt"), []byte(name+" fixture\n"), 0o600))
			}
			responses := []string{mixedDelegationResponse(t, order, root)}
			childCalls := make(map[string]bool)
			for _, id := range order {
				if strings.HasPrefix(id, "delegate") {
					callID := "child-find-" + id
					childCalls[callID] = true
					responses = append(responses, sequentialCLIResponse(t, callID, "find", map[string]any{"path": ".", "pattern": "read-*.txt"}), `{"choices":[{"message":{"role":"assistant","content":"child search complete"}}]}`)
				}
			}
			responses = append(responses, `{"choices":[{"message":{"role":"assistant","content":"parent complete"}}]}`, `{"choices":[{"message":{"role":"assistant","content":"parent resumed"}}]}`)
			server, script := newDurablePromptServer(t, responses...)
			defer server.Close()
			store := filepath.Join(t.TempDir(), "store")
			cfg := durablePromptConfig(t, server.URL, store)
			cfg.Sandbox.AllowedPaths = []string{root}
			var out bytes.Buffer
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			require.NoError(t, newPromptCmd(&out).Run(ctx, cfg, []string{"-approve=auto", "parent mixed batch"}))
			require.Contains(t, out.String(), "parent complete")
			require.Len(t, script.requestBodies(), 2+2*len(childCalls), "each child actually searches before the parent final reply")
			require.Contains(t, script.requestBodies()[0], "Before claiming completion, verify the requested deliverables and relevant checks. If a verified blocker prevents further authorized progress, state the blocker and unfinished work accurately instead of claiming success.")
			require.Contains(t, script.requestBodies()[0], "Perform branch, commit, or publish steps only when explicitly requested or authorized by the task, and verify their completion before reporting them as done.")
			parentID := physicalSessionIDFromOutput(t, out.String())
			files, err := filepath.Glob(filepath.Join(store, "sessions", "*.jsonl"))
			require.NoError(t, err)
			require.Len(t, files, 1+len(childCalls))
			var ids []string
			for _, file := range files {
				ids = append(ids, strings.TrimSuffix(filepath.Base(file), ".jsonl"))
			}
			for _, id := range ids {
				resumed, openErr := newDurableCLISession(ctx, store, "inspect", id, "")
				require.NoError(t, openErr)
				snapshot, snapshotErr := resumed.snapshotSelected()
				require.NoError(t, snapshotErr)
				pending, pendingErr := session.InspectPendingActions(snapshot)
				require.NoError(t, pendingErr)
				require.Empty(t, pending)
				var received []string
				for _, row := range snapshot.DurableEntries {
					if row.Type == session.EntryTypeTool && row.ActionState == "" && row.ReceiptOutcome != "" {
						require.Equal(t, "success", row.ReceiptOutcome, "%s %s: %s", id, row.ToolCallID, row.Content)
						received = append(received, row.ToolCallID)
						if childCalls[row.ToolCallID] {
							require.Contains(t, row.Content, "read-one.txt")
						}
					}
				}
				if id == parentID {
					require.Equal(t, order, received, "persisted receipts retain assistant call order")
				} else {
					require.Len(t, received, 1)
					require.True(t, childCalls[received[0]], "each physical child has one distinct search receipt")
					delete(childCalls, received[0])
				}
				require.NoError(t, resumed.requestClose(ctx))
			}
			type marker struct {
				Seq       int    `json:"seq"`
				Kind      string `json:"kind"`
				SessionID string `json:"session_id"`
				Actions   []struct {
					Action struct {
						ActionID   string `json:"action_id"`
						ToolCallID string `json:"tool_call_id"`
					} `json:"action"`
				} `json:"actions"`
				Resolution struct {
					ActionID string `json:"action_id"`
				} `json:"resolution"`
			}
			require.Empty(t, childCalls)
			configDir, err := os.UserConfigDir()
			require.NoError(t, err)
			indexes, err := filepath.Glob(filepath.Join(configDir, "go-cli", "workspace-quarantine", "*.jsonl"))
			require.NoError(t, err)
			require.Len(t, indexes, 1)
			raw, err := os.ReadFile(indexes[0])
			require.NoError(t, err)
			dispatch, resolution, actionID := map[string]int{}, map[string]int{}, map[string]string{}
			for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
				var row marker
				require.NoError(t, json.Unmarshal(line, &row))
				if row.Kind == "dispatch_ready" && row.SessionID == parentID {
					for _, action := range row.Actions {
						require.Zero(t, dispatch[action.Action.ToolCallID], "each parent action dispatched once")
						dispatch[action.Action.ToolCallID], actionID[action.Action.ToolCallID] = row.Seq, action.Action.ActionID
					}
				}
				if row.Kind == "execution_resolution" {
					resolution[row.Resolution.ActionID] = row.Seq
				}
			}
			for _, id := range order {
				require.Positive(t, dispatch[id], id)
				require.Greater(t, resolution[actionID[id]], dispatch[id], id)
			}
			for i := 1; i < len(order); i++ {
				if strings.HasPrefix(order[i-1], "delegate") != strings.HasPrefix(order[i], "delegate") {
					require.Greater(t, dispatch[order[i]], resolution[actionID[order[i-1]]], "next segment cannot reserve before the prior segment resolves")
				}
			}
			out.Reset()
			require.NoError(t, newPromptCmd(&out).Run(ctx, cfg, []string{"-approve=auto", "--resume-session", parentID, "resume without replay"}))
			require.Contains(t, out.String(), "parent resumed")
			require.Len(t, script.requestBodies(), len(responses))
			after, err := os.ReadFile(indexes[0])
			require.NoError(t, err)
			require.Equal(t, raw, after, "ordinary resume must not replay tool actions")
		})
	}
}
