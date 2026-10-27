package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestPromptDurableConfiguredPostToolHook(t *testing.T) {
	configureDurableCLISessionTest(t)
	t.Chdir(t.TempDir())
	marker := filepath.Join(t.TempDir(), "hook")
	server, script := newDurablePromptServer(t,
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"original","type":"function","function":{"name":"bash","arguments":"{\"command\":\"printf source\"}"}}]}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`,
	)
	defer server.Close()
	store := filepath.Join(t.TempDir(), "sessions")
	cfg := durablePromptConfig(t, server.URL, store)
	cfg.Hooks.Hooks = []config.HookConfig{{Event: "post_tool_use", Command: fmt.Sprintf("cat >> %q", marker), Timeout: "2s"}}
	out := &bytes.Buffer{}
	require.NoError(t, newPromptCmd(out).Run(t.Context(), cfg, []string{"--approve", "auto", "--no-sandbox", "run"}))
	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Contains(t, string(data), `"tool":"bash"`)
	require.Contains(t, string(data), `"duration_ms":`)
	require.Contains(t, string(data), `"exit_code":0`)
	require.Equal(t, 1, bytes.Count(data, []byte(`"event":"post_tool_use"`)))
	physicalID := physicalSessionIDFromOutput(t, out.String())
	resumed, err := newDurableCLISession(t.Context(), store, "readback", physicalID, "")
	require.NoError(t, err)
	defer resumed.requestClose(t.Context())
	snapshot, err := resumed.snapshotSelected()
	require.NoError(t, err)
	require.NotEmpty(t, snapshot.PublishedView.Messages)
	sourceReceiptSeq, hookIntentSeq := uint64(0), uint64(0)
	hookCallID := ""
	actionIDs := map[string]string{}
	proofs := map[string]bool{}
	for _, entry := range snapshot.DurableEntries {
		if entry.ActionState == session.ActionStateIntent {
			actionIDs[entry.ToolCallID] = entry.ActionID
			if strings.HasPrefix(entry.ToolCallID, "post-hook-") {
				hookIntentSeq = entry.Seq
				hookCallID = entry.ToolCallID
			}
		}
		if entry.ToolCallID == "original" && entry.ReceiptOutcome != "" {
			sourceReceiptSeq = entry.Seq
		}
		if entry.CompletionProof != "" {
			proofs[entry.ToolCallID] = true
		}
	}
	require.Len(t, actionIDs, 2)
	require.NotEmpty(t, actionIDs["original"])
	require.NotEqual(t, actionIDs["original"], actionIDs[hookCallID])
	require.True(t, proofs["original"])
	require.True(t, proofs[hookCallID])
	require.Less(t, sourceReceiptSeq, hookIntentSeq, "hook reservation must follow durable source receipt")
	bodies := script.requestBodies()
	require.Len(t, bodies, 2)
	require.Contains(t, bodies[1], "Runtime configured post-tool hook")
}

func TestPromptDurablePostToolHooksParallelFailureAndResume(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		t.Run(fmt.Sprintf("parallel=%v", parallel), func(t *testing.T) {
			configureDurableCLISessionTest(t)
			t.Chdir(t.TempDir())
			sourceMarker := filepath.Join(t.TempDir(), "source")
			hookMarker := filepath.Join(t.TempDir(), "hook")
			calls := []map[string]any{}
			for _, id := range []string{"first", "second"} {
				args, _ := json.Marshal(map[string]any{"command": fmt.Sprintf("printf x >> %q", sourceMarker)})
				calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "bash", "arguments": string(args)}})
			}
			response, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": calls}}}})
			server, script := newDurablePromptServer(t, string(response), `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
			defer server.Close()
			store := filepath.Join(t.TempDir(), "sessions")
			cfg := durablePromptConfig(t, server.URL, store)
			cfg.Tools.Parallel = &parallel
			cfg.Hooks.Hooks = []config.HookConfig{{Event: "post_tool_use", Command: fmt.Sprintf("cat >> %q; exit 7", hookMarker), Timeout: "2s"}}
			out := &bytes.Buffer{}
			require.NoError(t, newPromptCmd(out).Run(t.Context(), cfg, []string{"--approve", "auto", "--no-sandbox", "run"}))
			data, err := os.ReadFile(sourceMarker)
			require.NoError(t, err)
			require.Equal(t, "xx", string(data))
			data, err = os.ReadFile(hookMarker)
			require.NoError(t, err)
			require.Equal(t, 2, bytes.Count(data, []byte(`"event":"post_tool_use"`)))
			bodies := script.requestBodies()
			require.Len(t, bodies, 2)
			var request struct {
				Messages []struct {
					Role       string `json:"role"`
					ToolCallID string `json:"tool_call_id"`
					ToolCalls  []any  `json:"tool_calls"`
				} `json:"messages"`
			}
			require.NoError(t, json.Unmarshal([]byte(bodies[1]), &request))
			pending := 0
			for _, message := range request.Messages {
				if message.Role == "assistant" && len(message.ToolCalls) > 0 {
					require.Zero(t, pending, "source assistant tool group must close before runtime assistant")
					pending = len(message.ToolCalls)
				}
				if message.Role == "tool" {
					pending--
				}
			}
			require.Zero(t, pending)
			id := physicalSessionIDFromOutput(t, out.String())
			resume := &bytes.Buffer{}
			require.NoError(t, newPromptCmd(resume).Run(t.Context(), cfg, []string{"--resume-session", id, "--approve", "auto", "--no-sandbox", "continue"}))
			data, err = os.ReadFile(sourceMarker)
			require.NoError(t, err)
			require.Equal(t, "xx", string(data), "resume must never replay source tools")
			data, err = os.ReadFile(hookMarker)
			require.NoError(t, err)
			require.Equal(t, 2, bytes.Count(data, []byte(`"event":"post_tool_use"`)), "resume must never replay completed hooks")
		})
	}
}

func TestPromptDurablePostToolPayloadStaysOnStdin(t *testing.T) {
	configureDurableCLISessionTest(t)
	t.Chdir(t.TempDir())
	root, rootErr := os.Getwd()
	require.NoError(t, rootErr)
	injected := filepath.Join(root, "injected")
	input := filepath.Join(root, "input")
	payload := "'\"\n$(touch " + injected + ")\n" + strings.Repeat("large data ", 40000)
	require.NoError(t, os.WriteFile(input, []byte(payload), 0600))
	output := filepath.Join(root, "hook-payload")
	args, _ := json.Marshal(map[string]any{"path": input})
	response, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "read-input", "type": "function", "function": map[string]any{"name": "read", "arguments": string(args)}}}}}}})
	server, script := newDurablePromptServer(t, string(response), `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
	defer server.Close()
	cfg := durablePromptConfig(t, server.URL, filepath.Join(root, "sessions"))
	cfg.Compaction.MaxTokens = 1000000
	cfg.Hooks.Hooks = []config.HookConfig{{Event: "post_tool_use", Command: fmt.Sprintf("cat > %q", output), Timeout: "2s"}}
	require.NoError(t, newPromptCmd(&bytes.Buffer{}).Run(t.Context(), cfg, []string{"--approve", "auto", "--no-sandbox", "read"}))
	data, err := os.ReadFile(output)
	require.NoError(t, err)
	var received struct {
		Result struct {
			Output string `json:"output"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(data, &received))
	require.Contains(t, received.Result.Output, "$(touch "+injected+")")
	_, err = os.Stat(injected)
	require.True(t, os.IsNotExist(err))
	require.Contains(t, script.requestBodies()[1], "runtime hook input sha256=")
}

func TestPromptDurablePostToolTimeoutDoesNotReplaySource(t *testing.T) {
	runPostToolInterruption(t, false)
}

func TestPromptDurablePostToolCancellationDoesNotReplaySource(t *testing.T) {
	runPostToolInterruption(t, true)
}

func runPostToolInterruption(t *testing.T, cancelDuringHook bool) {
	t.Helper()
	configureDurableCLISessionTest(t)
	t.Chdir(t.TempDir())
	sourceMarker := filepath.Join(t.TempDir(), "source")
	hookMarker := filepath.Join(t.TempDir(), "hook")
	args, _ := json.Marshal(map[string]any{"command": fmt.Sprintf("printf x >> %q", sourceMarker)})
	response, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "source", "type": "function", "function": map[string]any{"name": "bash", "arguments": string(args)}}}}}}})
	server, _ := newDurablePromptServer(t, string(response), `{"choices":[{"message":{"role":"assistant","content":"after interruption"}}]}`)
	defer server.Close()
	store := filepath.Join(t.TempDir(), "sessions")
	cfg := durablePromptConfig(t, server.URL, store)
	cfg.Hooks.Hooks = []config.HookConfig{{Event: "post_tool_use", Command: fmt.Sprintf("printf hook >> %q; sleep 2", hookMarker), Timeout: "100ms"}}
	runCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	watcherDone := make(chan struct{})
	if cancelDuringHook {
		cfg.Hooks.Hooks[0].Timeout = "5s"
		go func() {
			defer close(watcherDone)
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-ticker.C:
					if _, err := os.Stat(hookMarker); err == nil {
						cancel()
						return
					}
				}
			}
		}()
	} else {
		close(watcherDone)
	}
	out := &bytes.Buffer{}
	require.Error(t, newPromptCmd(out).Run(runCtx, cfg, []string{"--approve", "auto", "--no-sandbox", "run"}))
	cancel()
	<-watcherDone
	data, err := os.ReadFile(sourceMarker)
	require.NoError(t, err)
	require.Equal(t, "x", string(data))
	id := physicalSessionIDFromOutput(t, out.String())
	resumed, err := newDurableCLISession(t.Context(), store, "readback", id, "")
	require.NoError(t, err)
	snapshot, err := resumed.snapshotSelected()
	require.NoError(t, err)
	sourceReceipts := 0
	hookReceipts := 0
	sourceProof := false
	for _, entry := range snapshot.DurableEntries {
		if entry.ToolCallID == "source" && entry.ReceiptOutcome != "" {
			sourceReceipts++
			require.Equal(t, core.ReceiptOutcomeSuccess, entry.ReceiptOutcome)
		}
		if entry.ToolCallID == "source" && entry.CompletionProof != "" {
			sourceProof = true
		}
		if strings.HasPrefix(entry.ToolCallID, "post-hook-") && entry.ReceiptOutcome != "" {
			hookReceipts++
		}
	}
	require.Equal(t, 1, sourceReceipts)
	require.True(t, sourceProof)
	require.Equal(t, 1, hookReceipts)
	require.NoError(t, resumed.requestClose(t.Context()))
	_ = newPromptCmd(&bytes.Buffer{}).Run(t.Context(), cfg, []string{"--resume-session", id, "--approve", "auto", "--no-sandbox", "continue"})
	data, err = os.ReadFile(sourceMarker)
	require.NoError(t, err)
	require.Equal(t, "x", string(data), "hook timeout/recovery must never replay source side effects")
	data, err = os.ReadFile(hookMarker)
	require.NoError(t, err)
	require.Equal(t, "hook", string(data), "interrupted hook is never automatically replayed")
}

func TestDurablePostToolHookRespectsPlanAndEmitsOwnResult(t *testing.T) {
	configureDurableCLISessionTest(t)
	t.Chdir(t.TempDir())
	root, err := os.Getwd()
	require.NoError(t, err)
	input := filepath.Join(root, "input")
	require.NoError(t, os.WriteFile(input, []byte("allowed read"), 0600))
	marker := filepath.Join(root, "forbidden-hook")
	args, _ := json.Marshal(map[string]any{"path": input})
	response, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "source", "type": "function", "function": map[string]any{"name": "read", "arguments": string(args)}}}}}}})
	server, _ := newDurablePromptServer(t, string(response), `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
	defer server.Close()
	cfg := durablePromptConfig(t, server.URL, filepath.Join(root, "sessions"))
	cfg.Git.WorkDir = root
	cfg.Sandbox.AllowedPaths = []string{root}
	cfg.Hooks.Hooks = []config.HookConfig{{Event: "post_tool_use", Command: fmt.Sprintf("printf forbidden > %q", marker), Timeout: "2s"}}
	durable, err := newDurableCLISession(t.Context(), cfg.Session.StorePath, "plan", "", "")
	require.NoError(t, err)
	assembly, err := AssembleAgent(t.Context(), cfg, cfg.Provider.Name, cfg.Provider.Model, io.Discard, WithApproveMode(ApproveAuto), WithNoSandbox(), withDurableCLISession(durable))
	require.NoError(t, err)
	defer assembly.Close(context.Background())
	require.NoError(t, assembly.PlanCtrl.Enter(t.Context(), "planning"))
	runCtx, _, err := durable.bindRun(t.Context())
	require.NoError(t, err)
	stream, err := assembly.Harness.Submit(runCtx, "read")
	require.NoError(t, err)
	events, _, runErr := collectAssemblyEvents(stream)
	require.NoError(t, runErr)
	sourceSeen, hookSeen := false, false
	for _, event := range events {
		if event.Kind == "tool_result" && event.ToolCallID == "source" {
			sourceSeen = true
			require.Equal(t, core.ReceiptOutcomeSuccess, event.ReceiptOutcome)
		}
		if event.Kind == "tool_result" && strings.HasPrefix(event.ToolCallID, "post-hook-") {
			hookSeen = true
			require.NotNil(t, event.AssistantTranscriptOrdinal)
			require.Equal(t, uint64(2), *event.AssistantTranscriptOrdinal)
			require.NotNil(t, event.CanonicalTranscriptIndex)
			require.Equal(t, uint64(3), *event.CanonicalTranscriptIndex)
			require.Equal(t, core.ReceiptOutcomeError, event.ReceiptOutcome)
		}
	}
	require.True(t, sourceSeen)
	require.True(t, hookSeen)
	_, err = os.Stat(marker)
	require.True(t, os.IsNotExist(err), "Plan must not inherit source read permission for opaque hook")
}

func TestNonDurablePostToolKnownErrorAndDenial(t *testing.T) {
	for _, mode := range []string{"auto", "deny"} {
		t.Run(mode, func(t *testing.T) {
			t.Chdir(t.TempDir())
			marker := filepath.Join(t.TempDir(), "hook")
			server, _ := newDurablePromptServer(t, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"source-error","type":"function","function":{"name":"bash","arguments":"{\"command\":\"exit 7\"}"}}]}}]}`, `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
			defer server.Close()
			cfg := newPromptConfig(t, server.URL)
			cfg.Hooks.Hooks = []config.HookConfig{{Event: "post_tool_use", Command: fmt.Sprintf("cat > %q", marker), Timeout: "2s"}}
			require.NoError(t, newPromptCmd(&bytes.Buffer{}).Run(t.Context(), cfg, []string{"--approve", mode, "--no-sandbox", "run"}))
			data, err := os.ReadFile(marker)
			if mode == "deny" {
				require.True(t, os.IsNotExist(err))
				return
			}
			require.NoError(t, err)
			require.Contains(t, string(data), `"is_error":true`)
			require.Contains(t, string(data), `"exit_code":7`)
			require.Contains(t, string(data), `"duration_ms":`)
		})
	}
}

func TestPostToolHostIDsCannotPromoteDeniedSource(t *testing.T) {
	for _, durableMode := range []bool{false, true} {
		for _, parallel := range []bool{false, true} {
			t.Run(fmt.Sprintf("durable=%v/parallel=%v", durableMode, parallel), func(t *testing.T) {
				configureDurableCLISessionTest(t)
				t.Chdir(t.TempDir())
				marker := filepath.Join(t.TempDir(), "hook")
				deniedMarker := filepath.Join(t.TempDir(), "denied-source")
				deniedCommand := fmt.Sprintf("printf forbidden > %q", deniedMarker)
				calls := []map[string]any{}
				for _, call := range []struct{ id, command string }{{"first", "printf first"}, {"post-hook-first-0", deniedCommand}} {
					args, err := json.Marshal(map[string]any{"command": call.command})
					require.NoError(t, err)
					calls = append(calls, map[string]any{"id": call.id, "type": "function", "function": map[string]any{"name": "bash", "arguments": string(args)}})
				}
				response, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": calls}}}})
				require.NoError(t, err)
				server, _ := newDurablePromptServer(t, string(response), `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
				defer server.Close()
				cfg := newPromptConfig(t, server.URL)
				cfg.Tools.Parallel = &parallel
				cfg.Hooks.Hooks = []config.HookConfig{{Event: "post_tool_use", Command: fmt.Sprintf("cat >> %q", marker), Timeout: "2s"}}
				opts := []AssembleOption{WithApproveMode(ApproveAuto), WithNoSandbox()}
				var durable *durableCLISession
				if durableMode {
					cfg.Session.StorePath = filepath.Join(t.TempDir(), "sessions")
					durable, err = newDurableCLISession(t.Context(), cfg.Session.StorePath, "crafted-source-id", "", "")
					require.NoError(t, err)
					opts = append(opts, withDurableCLISession(durable))
				}
				assembly, err := AssembleAgent(t.Context(), cfg, cfg.Provider.Name, cfg.Provider.Model, io.Discard, opts...)
				require.NoError(t, err)
				defer assembly.Close(context.Background())
				registry, ok := assembly.ToolRegistry.(*tools.MiddlewareToolRegistry)
				require.True(t, ok)
				require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
					return tools.PreparedPolicyDecision{Allowed: request.Call.ToolCall().Args["command"] != deniedCommand, Generations: request.Generations}, nil
				}))
				runCtx := t.Context()
				if durableMode {
					runCtx, _, err = durable.bindRun(runCtx)
					require.NoError(t, err)
				}
				stream, err := assembly.Harness.Submit(runCtx, "run source group")
				require.NoError(t, err)
				_, _, runErr := collectAssemblyEvents(stream)
				require.NoError(t, runErr)
				data, err := os.ReadFile(marker)
				require.NoError(t, err)
				require.Equal(t, 1, bytes.Count(data, []byte(`"event":"post_tool_use"`)), "completed hook must never promote a distinct denied source via its opaque ID")
				require.Contains(t, string(data), `"tool_call_id":"first"`)
				require.NotContains(t, string(data), `"tool_call_id":"post-hook-first-0"`)
				_, err = os.Stat(deniedMarker)
				require.True(t, os.IsNotExist(err))
			})
		}
	}
}
