package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/capability"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func durableApprovalToolResponse(t *testing.T, id, name string, args map[string]any) string {
	t.Helper()
	arguments, err := json.Marshal(args)
	require.NoError(t, err)
	response, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
		"role": "assistant", "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(arguments)}}},
	}}}})
	require.NoError(t, err)
	return string(response)
}

func ownDurableApprovalAuthority(t *testing.T, root string) {
	t.Helper()
	id, err := session.WorkspaceIDForCanonicalRoot(root)
	require.NoError(t, err)
	configDir, err := os.UserConfigDir()
	require.NoError(t, err)
	directory := filepath.Join(configDir, "go-cli", "workspace-quarantine")
	for _, suffix := range []string{".jsonl", ".lock"} {
		_, err := os.Lstat(filepath.Join(directory, id+suffix))
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	t.Cleanup(func() {
		paths := []string{filepath.Join(directory, id+".jsonl"), filepath.Join(directory, id+".lock")}
		for _, suffix := range []string{".jsonl.torn-*.tail", ".jsonl.init-*"} {
			matches, err := filepath.Glob(filepath.Join(directory, id+suffix))
			require.NoError(t, err)
			paths = append(paths, matches...)
		}
		for _, path := range paths {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Errorf("remove own temporary authority %s: %v", path, err)
			}
		}
	})
}

func TestDurableApprovalDecisionsPublishAndPermitNextRun(t *testing.T) {
	for _, test := range []struct {
		name     string
		mode     ApproveMode
		hook     bool
		cancel   bool
		deadline bool
	}{
		{name: "ask-deny", mode: ApproveAsk},
		{name: "auto", mode: ApproveAuto},
		{name: "deny", mode: ApproveDeny},
		{name: "hook-deny", mode: ApproveAuto, hook: true},
		{name: "ask-cancel-late-allow", mode: ApproveAsk, cancel: true},
		{name: "ask-deadline-late-allow", mode: ApproveAsk, deadline: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, verified, _, _ := newPersistedDynamicTrustFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			root := verified.CanonicalRoot()
			ownDurableApprovalAuthority(t, root)
			input, output := filepath.Join(root, "input.txt"), filepath.Join(root, "write.txt")
			require.NoError(t, os.WriteFile(input, []byte("next read is live"), 0600))
			responses := []string{durableApprovalToolResponse(t, "write-once", "write", map[string]any{"path": output, "content": "approved bytes"})}
			if !test.cancel && !test.deadline {
				responses = append(responses, `{"choices":[{"message":{"role":"assistant","content":"first decision completed"}}]}`)
			}
			responses = append(responses, durableApprovalToolResponse(t, "next-read", "read", map[string]any{"path": input}),
				`{"choices":[{"message":{"role":"assistant","content":"next run completed"}}]}`)
			provider, script := newDurablePromptServer(t, responses...)
			defer provider.Close()
			durable, err := newDurableCLISession(ctx, filepath.Join(root, "sessions"), "approval-recovery", "", "")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, durable.requestClose(context.Background())) })
			channel := make(chan approval.InteractiveRequest, 1)
			cfg := newAssembleTestConfig(t)
			cfg.Git.WorkDir, cfg.Provider.BaseURL, cfg.Runtime.Dynamic = root, provider.URL, true
			cfg.Memory.StorePath = filepath.Join(root, "memories.jsonl")
			assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(test.mode), WithApprovalChannel(channel), withDurableCLISession(durable))
			require.NoError(t, err)
			t.Cleanup(assembly.Cleanup)
			assembly.LoopAgent.WithToolSearchThreshold(0)
			borrowed, err := fixture.manager.Acquire(ctx)
			require.NoError(t, err)
			plan := *borrowed.Value()
			borrowed.Release()
			plan.registry = assembly.ToolRegistry
			if test.hook {
				hook := core.NewShellHookWithStartGuard("deny-write", core.EventPreToolUse,
					`payload=$(cat); case "$payload" in *'"tool":"write"'*) exit 7;; esac`, time.Second, verified.CommitIfCurrent)
				manager := core.NewHookManager(hook)
				plan.hooks, plan.interceptor = core.NewHookChain(manager), manager.PreToolUseContextInterceptor()
			}
			snapshot, err := capability.NewSnapshot(capability.Input{Prompt: "approval recovery", TrustEpoch: verified.TrustGeneration()})
			require.NoError(t, err)
			require.NoError(t, assembly.dynamicRuntime.manager.Reload(ctx, func(context.Context, *capability.Scope) (capability.Snapshot, *runtimePlan, error) {
				return snapshot, &plan, nil
			}))
			var physicalID string
			for index, prompt := range []string{"write once and report its decision", "read the input in a fresh run"} {
				parent := ctx
				cancelRun := func() {}
				if index == 0 && test.cancel {
					parent, cancelRun = context.WithCancel(ctx)
				} else if index == 0 && test.deadline {
					parent, cancelRun = context.WithTimeout(ctx, 5*time.Second)
				}
				defer cancelRun()
				runCtx, sink, err := durable.bindRun(parent)
				require.NoError(t, err)
				require.NoError(t, hydrateDurableRunHistory(assembly.Agent, sink))
				stream, err := assembly.Harness.Submit(runCtx, prompt)
				require.NoError(t, err)
				if index == 0 && test.mode == ApproveAsk {
					select {
					case request := <-channel:
						require.Equal(t, "write", request.ToolName)
						if test.cancel || test.deadline {
							if test.cancel {
								cancelRun()
							} else {
								<-parent.Done()
							}
							<-request.Context.Done()
							request.ResponseCh <- approval.InteractiveResponse{Decision: approval.InteractiveAllow}
						} else {
							request.ResponseCh <- approval.InteractiveResponse{Decision: approval.InteractiveDeny}
						}
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
				var receipts []core.AgentEvent
				for event := range stream.Events() {
					if event.Kind == "tool_result" {
						receipts = append(receipts, event)
					}
				}
				result, runErr := stream.Result()
				if index == 0 && (test.cancel || test.deadline) {
					require.ErrorIs(t, runErr, parent.Err())
				} else {
					require.NoError(t, runErr)
					require.Contains(t, result.Content, "completed")
				}
				statusSource, ok := stream.(core.RunStatusSource)
				require.True(t, ok)
				waiter, ok := statusSource.RunStatusHandle().(core.RunStatusWaiter)
				require.True(t, ok)
				status, publication, err := waiter.WaitForCompletion(ctx)
				require.NoError(t, err)
				require.Equal(t, core.RunExecutionQuiescent, status.ExecutionState)
				require.NotEqual(t, core.RunOutcomeUnknown, status.Outcome)
				require.Equal(t, core.DurablePublicationSynced, publication.State)
				if index != 0 || !test.cancel && !test.deadline {
					require.Len(t, receipts, 1, "a known denial must reach the UI as a terminal tool receipt")
				}
				sink.CloseRunCommit()
				published, err := durable.snapshotSelected()
				require.NoError(t, err)
				if index == 0 {
					physicalID = published.SessionID
					denials := 0
					for _, entry := range published.DurableEntries {
						if entry.ToolCallID == "write-once" && entry.ActionState == "" && entry.Type == session.EntryTypeTool && (test.mode != ApproveAuto || test.hook) {
							denials++
							require.Equal(t, core.DispatchStateNotDispatched, entry.DispatchState)
							require.Empty(t, entry.ActionID, "a preparation or hook denial must not borrow an executor completion proof")
						}
					}
					if test.mode != ApproveAuto || test.hook {
						require.Equal(t, 1, denials, "even a canceled stream retains the exact durable not-dispatched receipt")
					}
				}
				require.Equal(t, physicalID, published.SessionID)
				if index == 0 && (test.mode != ApproveAuto || test.hook) {
					if !test.cancel && !test.deadline {
						require.True(t, receipts[0].IsError)
					}
					require.NoFileExists(t, output)
				} else if test.mode != ApproveDeny {
					require.Equal(t, core.ReceiptOutcomeSuccess, receipts[0].ReceiptOutcome)
				}
			}
			require.Len(t, script.requestBodies(), len(responses))
			require.Empty(t, channel)
		})
	}
}

func TestDynamicPolicyFenceKeepsRealRevocationAndPlanBoundaries(t *testing.T) {
	for _, domain := range []tools.DispatchFenceDomain{tools.DispatchFenceApproval, tools.DispatchFencePlan} {
		fixture, _, _, _ := newPersistedDynamicTrustFixture(t)
		ctx, lease, err := fixture.AcquireRun(context.Background())
		require.NoError(t, err)
		guard := ctx.Value(runtimePlanKey{}).(*runtimePlan)
		fence, err := tools.DispatchFenceFromRegistry(guard.registry)
		require.NoError(t, err)
		borrowed, err := fixture.manager.Acquire(ctx)
		require.NoError(t, err)
		runGuard := &runtimeCapabilityGuard{lease: borrowed, bindings: borrowed.Value().bindings, plan: borrowed.Value(), policyFence: fence, policy: fence.Snapshot()}
		invocation, err := runGuard.BeginCall(ctx, "already-running")
		require.NoError(t, err)
		_, err = fence.Publish(domain, nil, func() {})
		require.NoError(t, err)
		require.Error(t, core.CheckRunGeneration(ctx), "actual permission or Plan changes still invalidate the pinned run")
		require.Error(t, invocation.Complete(), "an in-flight result remains rejected after actual policy changes")
		invocation.Release()
		borrowed.Release()
		lease.Release()
	}
}
