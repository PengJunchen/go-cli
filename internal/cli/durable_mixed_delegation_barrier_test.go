package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func barrierBatchResponse(t *testing.T, root string, order []string) string {
	t.Helper()
	calls := make([]any, 0, len(order))
	for _, id := range order {
		var name string
		var args map[string]any
		switch id {
		case "bash":
			name, args = "bash", map[string]any{"command": "sleep 1; printf 'ordinary joined\\n'", "timeout_ms": 5000}
		case "read":
			name, args = "read", map[string]any{"path": filepath.Join(root, "fixture.txt")}
		case "missing-one", "missing-two":
			name, args = id+"-tool", map[string]any{}
		case "delegate-one":
			name, args = "dispatch_subagent", map[string]any{"prompt": "barrier child one", "role": "researcher", "tools": []string{"find"}}
		case "delegate-two":
			name, args = "dispatch_subagent", map[string]any{"prompt": "barrier child two", "role": "researcher", "tools": []string{"grep"}}
		default:
			t.Fatalf("unknown test call %q", id)
		}
		encoded, err := json.Marshal(args)
		require.NoError(t, err)
		calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(encoded)}})
	}
	response, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": calls}}}})
	require.NoError(t, err)
	return string(response)
}

func barrierRoleServer(t *testing.T, root string, order []string) (*httptest.Server, func() map[string]int) {
	t.Helper()
	parent := barrierBatchResponse(t, root, order)
	find := sequentialCLIResponse(t, "child-find", "find", map[string]any{"path": ".", "pattern": "fixture.txt"})
	grep := sequentialCLIResponse(t, "child-grep", "grep", map[string]any{"path": ".", "pattern": "barrier-canary"})
	var mu sync.Mutex
	counts := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read request", http.StatusBadRequest)
			return
		}
		var request struct {
			Messages []struct {
				Role       string `json:"role"`
				Content    any    `json:"content"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			http.Error(w, "decode request", http.StatusBadRequest)
			return
		}
		role, hasReceipt := "parent", false
		for _, message := range request.Messages {
			text, _ := message.Content.(string)
			if message.Role == "user" && strings.Contains(text, "barrier child one") {
				role = "one"
			}
			if message.Role == "user" && strings.Contains(text, "barrier child two") {
				role = "two"
			}
		}
		for _, message := range request.Messages {
			if message.Role == "tool" && ((role == "one" && message.ToolCallID == "child-find") || (role == "two" && message.ToolCallID == "child-grep") || (role == "parent" && message.ToolCallID == order[len(order)-1])) {
				hasReceipt = true
			}
		}
		mu.Lock()
		counts[role]++
		mu.Unlock()
		response := parent
		switch role {
		case "one":
			response = find
		case "two":
			response = grep
		}
		if hasReceipt {
			response = `{"choices":[{"message":{"role":"assistant","content":"finished"}}]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response)
	}))
	return server, func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		copyCounts := make(map[string]int, len(counts))
		for key, count := range counts {
			copyCounts[key] = count
		}
		return copyCounts
	}
}

func TestDurableMixedDelegationsWaitForPriorResolution(t *testing.T) {
	for _, order := range [][]string{
		{"bash", "delegate-one", "delegate-two"},
		{"delegate-one", "bash", "delegate-two"},
		{"read", "bash", "delegate-one"},
		{"delegate-one", "delegate-two"},
		{"missing-one", "read", "delegate-one", "missing-two"},
	} {
		t.Run(strings.Join(order, "-"), func(t *testing.T) {
			configureDurableCLISessionTest(t)
			root, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			t.Chdir(root)
			require.NoError(t, os.WriteFile(filepath.Join(root, "fixture.txt"), []byte("barrier-canary\n"), 0o600))
			server, counts := barrierRoleServer(t, root, order)
			defer server.Close()
			store := filepath.Join(t.TempDir(), "store")
			cfg := durablePromptConfig(t, server.URL, store)
			cfg.Sandbox.AllowedPaths = []string{root}
			var out bytes.Buffer
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			require.NoError(t, newPromptCmd(&out).Run(ctx, cfg, []string{"-approve=auto", "run the barrier fixture"}))
			require.Contains(t, out.String(), "finished")
			parentID := physicalSessionIDFromOutput(t, out.String())
			configDir, err := os.UserConfigDir()
			require.NoError(t, err)
			indexes, err := filepath.Glob(filepath.Join(configDir, "go-cli", "workspace-quarantine", "*.jsonl"))
			require.NoError(t, err)
			require.Len(t, indexes, 1)
			raw, err := os.ReadFile(indexes[0])
			require.NoError(t, err)
			files, err := filepath.Glob(filepath.Join(store, "sessions", "*.jsonl"))
			require.NoError(t, err)
			childReceipts := make(map[string]string)
			childReceiptCount := make(map[string]int)
			for _, file := range files {
				data, readErr := os.ReadFile(file)
				require.NoError(t, readErr)
				for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
					var row struct {
						ReceiptOutcome string `json:"receipt_outcome"`
						ToolCallID     string `json:"tool_call_id"`
						Content        string `json:"content"`
					}
					require.NoError(t, json.Unmarshal(line, &row))
					if (row.ToolCallID == "child-find" || row.ToolCallID == "child-grep") && row.ReceiptOutcome != "" {
						childReceipts[row.ToolCallID] = row.ReceiptOutcome + ": " + row.Content
						childReceiptCount[row.ToolCallID]++
					}
				}
			}
			for _, pair := range [][2]string{{"delegate-one", "child-find"}, {"delegate-two", "child-grep"}} {
				for _, id := range order {
					if id == pair[0] {
						require.Contains(t, childReceipts[pair[1]], "success:", "child search must actually execute: %s", childReceipts[pair[1]])
						require.Equal(t, 1, childReceiptCount[pair[1]], "each physical child has exactly one search receipt")
					}
				}
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
			dispatch, resolution, actionID := map[string]int{}, map[string]int{}, map[string]string{}
			for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
				var row marker
				require.NoError(t, json.Unmarshal(line, &row))
				if row.Kind == "dispatch_ready" {
					for _, action := range row.Actions {
						id := action.Action.ToolCallID
						require.Zero(t, dispatch[id], "each parent or child action is dispatched once")
						dispatch[id], actionID[id] = row.Seq, action.Action.ActionID
					}
				}
				if row.Kind == "execution_resolution" {
					resolution[row.Resolution.ActionID] = row.Seq
				}
			}
			for _, id := range order {
				if strings.HasPrefix(id, "missing-") {
					require.Zero(t, dispatch[id], "a known preparation rejection cannot reserve a workspace action")
					continue
				}
				require.Positive(t, dispatch[id], "parent action %s was reserved", id)
				require.Greater(t, resolution[actionID[id]], dispatch[id], "parent action %s durably resolved", id)
			}
			for i := 1; i < len(order); i++ {
				if !strings.HasPrefix(order[i-1], "missing-") && !strings.HasPrefix(order[i], "missing-") &&
					(strings.HasPrefix(order[i-1], "delegate") || strings.HasPrefix(order[i], "delegate")) {
					require.Greater(t, dispatch[order[i]], resolution[actionID[order[i-1]]], "a later phase cannot reserve before prior durable resolution")
				}
			}
			for _, pair := range [][2]string{{"delegate-one", "child-find"}, {"delegate-two", "child-grep"}} {
				if dispatch[pair[0]] == 0 {
					continue
				}
				require.Greater(t, dispatch[pair[1]], dispatch[pair[0]], "the real child search must be dispatched")
				require.Greater(t, resolution[actionID[pair[1]]], dispatch[pair[1]])
				require.Greater(t, resolution[actionID[pair[0]]], resolution[actionID[pair[1]]])
			}
			var parentReceipts []string
			for _, file := range files {
				data, readErr := os.ReadFile(file)
				require.NoError(t, readErr)
				for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
					var row struct {
						Type           string `json:"type"`
						ActionState    string `json:"action_state"`
						ReceiptOutcome string `json:"receipt_outcome"`
						ToolCallID     string `json:"tool_call_id"`
						Content        string `json:"content"`
					}
					require.NoError(t, json.Unmarshal(line, &row))
					if row.Type == "tool" && row.ActionState == "" && row.ReceiptOutcome != "" {
						expected := "success"
						if strings.HasPrefix(row.ToolCallID, "missing-") {
							expected = "error"
						}
						require.Equal(t, expected, row.ReceiptOutcome, "%s: %s", row.ToolCallID, row.Content)
						if strings.TrimSuffix(filepath.Base(file), ".jsonl") == parentID {
							parentReceipts = append(parentReceipts, row.ToolCallID)
						}
					}
				}
			}
			require.Equal(t, order, parentReceipts, "canonical receipts follow assistant call order")
			for _, id := range order {
				if id == "delegate-one" {
					require.Equal(t, 2, counts()["one"])
				}
				if id == "delegate-two" {
					require.Equal(t, 2, counts()["two"])
				}
			}
			if strings.Join(order, ",") == "bash,delegate-one,delegate-two" {
				beforeCounts := counts()
				out.Reset()
				require.NoError(t, newPromptCmd(&out).Run(ctx, cfg, []string{"-approve=auto", "--resume-session", parentID, "resume without replay"}))
				after, readErr := os.ReadFile(indexes[0])
				require.NoError(t, readErr)
				require.Equal(t, raw, after, "resume must not reserve or replay prior actions")
				require.Equal(t, beforeCounts["one"], counts()["one"])
				require.Equal(t, beforeCounts["two"], counts()["two"])
			}
		})
	}
}
