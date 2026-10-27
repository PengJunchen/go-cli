package cli

import (
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

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
)

func TestPostToolTimeoutStartsAfterHumanApproval(t *testing.T) {
	for _, durableMode := range []bool{false, true} {
		for _, scenario := range []string{"quick", "execution-timeout", "plan-changed", "trust-revoked", "parent-canceled"} {
			t.Run(fmt.Sprintf("durable=%v/%s", durableMode, scenario), func(t *testing.T) {
				configureDurableCLISessionTest(t)
				root := t.TempDir()
				sourceMarker := filepath.Join(root, "source")
				hookMarker := filepath.Join(root, "hook")
				sourceCommand := fmt.Sprintf("printf x >> %q", sourceMarker)
				hookCommand := "cat >> " + shellSingleQuote(hookMarker)
				if scenario == "execution-timeout" {
					hookCommand += "; sleep 2"
				}
				args, err := json.Marshal(map[string]any{"command": sourceCommand})
				require.NoError(t, err)
				response, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "source", "type": "function", "function": map[string]any{"name": "bash", "arguments": string(args)}}}}}}})
				require.NoError(t, err)
				server, _ := newDurablePromptServer(t, string(response), `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
				defer server.Close()
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				cfg := newR4TrustedProjectHookConfig(ctx, t, root, server.URL, []r4ShellHookSpec{{event: "post_tool_use", command: hookCommand}})
				cfg.Provider.Name = "test"
				// Keep loader-minted project authority while using the explicitly short
				// execution budget in the captured hooks, rather than changing public config.
				hookPath := filepath.Join(cfg.Git.WorkDir, ".go-cli", "hooks.yaml")
				contents, err := os.ReadFile(hookPath)
				require.NoError(t, err)
				contents = []byte(strings.ReplaceAll(string(contents), "timeout: 2s", "timeout: 500ms"))
				require.NoError(t, os.WriteFile(hookPath, contents, 0600))
				manager := cfg.ProjectTrustManager()
				grantProjectShellHooks(ctx, t, cfg.Git.WorkDir, manager)
				fresh, err := config.LoadTrustedWithManager(ctx, manager)
				require.NoError(t, err)
				fresh.Provider = cfg.Provider
				fresh.Model = cfg.Model
				fresh.Git = cfg.Git
				fresh.Sandbox = cfg.Sandbox
				cfg = fresh
				approvalCh := make(chan approval.InteractiveRequest)
				opts := []AssembleOption{WithApproveMode(ApproveAsk), WithApprovalChannel(approvalCh), WithNoSandbox()}
				var durable *durableCLISession
				if durableMode {
					cfg.Session.StorePath = filepath.Join(t.TempDir(), "sessions")
					durable, err = newDurableCLISession(ctx, cfg.Session.StorePath, "slow-approval", "", "")
					require.NoError(t, err)
					opts = append(opts, withDurableCLISession(durable))
				}
				assembly, err := AssembleAgent(ctx, cfg, cfg.Provider.Name, cfg.Provider.Model, io.Discard, opts...)
				require.NoError(t, err)
				defer func() { cancel(); require.NoError(t, assembly.Close(context.Background())) }()
				runCtx := ctx
				if durableMode {
					runCtx, _, err = durable.bindRun(runCtx)
					require.NoError(t, err)
				}
				stream, err := assembly.Harness.Submit(runCtx, "run source")
				require.NoError(t, err)
				receive := func() approval.InteractiveRequest {
					t.Helper()
					select {
					case request := <-approvalCh:
						return request
					case <-ctx.Done():
						t.Fatal("approval request did not arrive")
						return approval.InteractiveRequest{}
					}
				}
				sourceRequest := receive()
				require.Equal(t, sourceCommand, sourceRequest.Args["command"])
				sourceRequest.ResponseCh <- approval.InteractiveResponse{Decision: approval.InteractiveAllow, DecisionSource: "user"}
				hookRequest := receive()
				require.Contains(t, hookRequest.Args["command"], "runtime hook input sha256=")
				// Leave room for managed-process startup and fsync under -race,
				// while making human review strictly longer than the execution budget.
				timer := time.NewTimer(750 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-ctx.Done():
					t.Fatal("parent canceled before controlled approval delay")
				}
				require.NoError(t, hookRequest.Context.Err(), "a human's approval wait must not consume the hook's 500ms execution budget")
				switch scenario {
				case "plan-changed":
					require.NoError(t, assembly.PlanCtrl.Enter(ctx, "plan entered while awaiting approval"))
				case "trust-revoked":
					require.NoError(t, manager.RevokeTrust(ctx, cfg.Git.WorkDir))
				case "parent-canceled":
					cancel()
				}
				started := time.Now()
				if scenario != "parent-canceled" {
					hookRequest.ResponseCh <- approval.InteractiveResponse{Decision: approval.InteractiveAllow, DecisionSource: "user"}
				}
				events, _, runErr := collectAssemblyEvents(stream)
				if scenario == "quick" {
					require.NoError(t, runErr)
				}
				sourceSeen, hookSeen := false, false
				for _, event := range events {
					if event.Kind != "tool_result" {
						continue
					}
					if event.ToolCallID == "source" {
						sourceSeen = true
						require.Equal(t, core.ReceiptOutcomeSuccess, event.ReceiptOutcome, event.Content)
					}
					if strings.HasPrefix(event.ToolCallID, "post-hook-") {
						hookSeen = true
						if scenario == "quick" {
							require.Equal(t, core.ReceiptOutcomeSuccess, event.ReceiptOutcome, event.Content)
						} else {
							require.NotEqual(t, core.ReceiptOutcomeSuccess, event.ReceiptOutcome)
						}
					}
				}
				require.True(t, sourceSeen)
				if scenario != "trust-revoked" {
					require.True(t, hookSeen)
				}
				data, err := os.ReadFile(sourceMarker)
				require.NoError(t, err)
				require.Equal(t, "x", string(data), "hook failure must never repeat the completed source")
				data, err = os.ReadFile(hookMarker)
				if scenario == "quick" || scenario == "execution-timeout" {
					require.NoError(t, err)
					require.Contains(t, string(data), `"event":"post_tool_use"`)
				} else {
					require.True(t, os.IsNotExist(err), "authority changes/cancellation while waiting must block hook start")
				}
				if scenario == "execution-timeout" {
					require.Less(t, time.Since(started), 1500*time.Millisecond, "the process must still be stopped by its execution budget")
				}
			})
		}
	}
}
