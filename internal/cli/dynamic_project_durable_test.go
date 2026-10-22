package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/capability"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestDynamicProjectSkillHookProviderAndDurableCompletionRemainLive(t *testing.T) {
	fixture, verified, _, _ := newPersistedDynamicTrustFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := verified.CanonicalRoot()
	input := filepath.Join(root, "input.txt")
	require.NoError(t, os.WriteFile(input, []byte("trusted read body"), 0600))
	toolResponse := func(id, name string, args map[string]any) string {
		arguments, err := json.Marshal(args)
		require.NoError(t, err)
		response, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(arguments)}}}}}}})
		require.NoError(t, err)
		return string(response)
	}
	responses := []string{}
	for _, suffix := range []string{"one", "two"} {
		responses = append(responses,
			toolResponse("skill-"+suffix, "skill_load", map[string]any{"name": "private"}),
			toolResponse("read-"+suffix, "read", map[string]any{"path": input}),
			`{"choices":[{"message":{"role":"assistant","content":"trusted durable run completed"}}]}`)
	}
	provider, script := newDurablePromptServer(t, responses...)
	defer provider.Close()
	// The global quarantine authority is intentionally not relocatable. This
	// test owns one fresh temporary-workspace ID and removes only those files
	// after all session handles close, without changing HOME or existing keys.
	workspaceID, err := session.WorkspaceIDForCanonicalRoot(root)
	require.NoError(t, err)
	configDir, err := os.UserConfigDir()
	require.NoError(t, err)
	authorityDir := filepath.Join(configDir, "go-cli", "workspace-quarantine")
	for _, suffix := range []string{".jsonl", ".lock"} {
		_, err = os.Lstat(filepath.Join(authorityDir, workspaceID+suffix))
		require.ErrorIs(t, err, os.ErrNotExist, "test must never borrow an existing authority ID")
	}
	t.Cleanup(func() {
		paths := []string{filepath.Join(authorityDir, workspaceID+".jsonl"), filepath.Join(authorityDir, workspaceID+".lock")}
		for _, pattern := range []string{workspaceID + ".jsonl.torn-*.tail", workspaceID + ".jsonl.init-*"} {
			matches, globErr := filepath.Glob(filepath.Join(authorityDir, pattern))
			require.NoError(t, globErr)
			paths = append(paths, matches...)
		}
		for _, path := range paths {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Errorf("remove owned test authority %s: %v", path, err)
			}
		}
		t.Logf("removed own temporary workspace authority %s", workspaceID)
	})
	durable, err := newDurableCLISession(ctx, filepath.Join(root, "sessions"), "dynamic-trust-success", "", "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, durable.requestClose(context.Background())) })
	cfg := newAssembleTestConfig(t)
	cfg.Provider.BaseURL = provider.URL
	cfg.Git.WorkDir = root
	cfg.Runtime.Dynamic = true
	cfg.Memory.StorePath = filepath.Join(root, "memories.jsonl")
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto), withDurableCLISession(durable))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	registrar, ok := assembly.ToolRegistry.(tools.TrustedInlineControlRegistrar)
	require.True(t, ok)
	for _, load := range []bool{false, true} {
		require.NoError(t, registrar.RegisterTrustedInlineControl(ctx, &skillCatalogTool{load: load}))
	}
	assembly.LoopAgent.WithToolSearchThreshold(0)
	borrowed, err := fixture.manager.Acquire(ctx)
	require.NoError(t, err)
	plan := *borrowed.Value()
	borrowed.Release()
	plan.registry = assembly.ToolRegistry
	hook := core.NewShellHookWithStartGuard("verified-project-hook", core.EventPreToolUse,
		"cat >/dev/null; printf 'checked\\n' >> .go-cli/hook-allowed.log", time.Second, verified.CommitIfCurrent)
	hookManager := core.NewHookManager(hook)
	plan.hooks = core.NewHookChain(hookManager)
	plan.interceptor = hookManager.PreToolUseContextInterceptor()
	snapshot, err := capability.NewSnapshot(capability.Input{Prompt: "trusted project skill/hook fixture", TrustEpoch: verified.TrustGeneration()})
	require.NoError(t, err)
	require.NoError(t, assembly.dynamicRuntime.manager.Reload(ctx, func(context.Context, *capability.Scope) (capability.Snapshot, *runtimePlan, error) {
		return snapshot, &plan, nil
	}))
	var physicalID string
	for _, message := range []string{"first trusted run", "second trusted run"} {
		runCtx, sink, err := durable.bindRun(ctx)
		require.NoError(t, err)
		require.NoError(t, hydrateDurableRunHistory(assembly.Agent, sink))
		stream, err := assembly.Harness.Submit(runCtx, message)
		require.NoError(t, err)
		for range stream.Events() {
		}
		result, runErr := stream.Result()
		sink.CloseRunCommit()
		require.NoError(t, runErr)
		require.Contains(t, result.Content, "trusted durable run completed")
		published, snapshotErr := durable.snapshotSelected()
		require.NoError(t, snapshotErr)
		if physicalID == "" {
			physicalID = published.SessionID
		}
		require.Equal(t, physicalID, published.SessionID)
	}
	require.Len(t, script.requestBodies(), 6, "both complete runs must pass actual resolver dispatch and session publication")
	hookEvents, err := os.ReadFile(filepath.Join(root, ".go-cli", "hook-allowed.log"))
	require.NoError(t, err)
	require.Equal(t, 4, strings.Count(string(hookEvents), "checked"))
}
