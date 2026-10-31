package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestPromptDurableReviewedToolsLocalAndBareRemoteJourney(t *testing.T) {
	configureDurableCLISessionTest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Chdir(root)
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		output, runErr := cmd.CombinedOutput()
		require.NoError(t, runErr, "%s", output)
	}
	git(root, "init", "-b", "main")
	git(root, "config", "user.name", "Local Test")
	git(root, "config", "user.email", "test@example.invalid")
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("initial\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".go-cli/\n"), 0o600))
	git(root, "add", "README.md", ".gitignore")
	git(root, "commit", "-m", "initial")
	bare := t.TempDir()
	git(bare, "init", "--bare")
	git(root, "remote", "add", "origin", bare)
	customDir := t.TempDir()
	custom := filepath.Join(customDir, "custom")
	marker := filepath.Join(customDir, "calls")
	require.NoError(t, os.WriteFile(custom, []byte("#!/bin/sh\nprintf called >> '"+marker+"'\nprintf '%s\\n' \"$1\" > README.md\nprintf custom-done\n"), 0o700))
	previousTrust := approval.GetTrustManager()
	manager := approval.NewDefaultTrustManager(nil)
	require.NoError(t, manager.TrustProject(ctx, root))
	approval.RegisterTrustManager(manager)
	t.Cleanup(func() { approval.RegisterTrustManager(previousTrust) })
	type action struct {
		name string
		args map[string]any
	}
	actions := []action{
		{"custom_local", map[string]any{"input": "changed"}},
		{"git_diff", map[string]any{}}, {"git_status", map[string]any{}},
		{"git_commit", map[string]any{"message": "changed", "add_all": true}},
		{"git_log", map[string]any{}}, {"git_branch", map[string]any{}},
		{"git_blame", map[string]any{"file": "README.md"}}, {"git_remote", map[string]any{}},
		{"git_push", map[string]any{"remote": "origin", "branch": "main"}},
		{"git_fetch", map[string]any{"remote": "origin"}},
		{"git_pull", map[string]any{"remote": "origin", "branch": "main"}},
		{"git_create_branch", map[string]any{"name": "reviewed-feature"}},
		{"git_checkout", map[string]any{"branch": "reviewed-feature"}},
		{"custom_local", map[string]any{"input": "stashed"}},
		{"git_stash", map[string]any{}}, {"git_stash_pop", map[string]any{}},
		{"git_reset", map[string]any{"mode": "hard"}},
		{"git_checkout", map[string]any{"branch": "main"}},
		{"git_merge", map[string]any{"branch": "reviewed-feature"}},
		{"git_revert", map[string]any{"commit": "HEAD"}},
		{"task_create", map[string]any{"title": "process task"}},
		{"task_update", map[string]any{"id": "task-1", "status": "completed"}},
		{"task_get", map[string]any{"id": "task-1"}}, {"task_list", map[string]any{}},
		{"goal_create", map[string]any{"title": "process goal"}}, {"goal_list", map[string]any{}},
		{"todo_write", map[string]any{"action": "add", "content": "process todo"}},
		{"todo_write", map[string]any{"action": "update", "id": "todo-1", "status": "completed"}},
		{"todo_write", map[string]any{"action": "list"}},
		{"todo_write", map[string]any{"action": "remove", "id": "todo-1"}},
	}
	responses := make([]string, 0, len(actions)+3)
	for index, action := range actions {
		responses = append(responses, durableApprovalToolResponse(t, strings.Repeat("x", index+1), action.name, action.args))
	}
	responses = append(responses, `{"choices":[{"message":{"role":"assistant","content":"all local actions completed"}}]}`,
		durableApprovalToolResponse(t, "fresh-task", "task_get", map[string]any{"id": "task-1"}),
		`{"choices":[{"message":{"role":"assistant","content":"restart uses fresh process stores"}}]}`)
	server, script := newDurablePromptServer(t, responses...)
	defer server.Close()
	store := filepath.Join(t.TempDir(), "sessions")
	cfg := durablePromptConfig(t, server.URL, store)
	disabledMemory := false
	cfg.Memory.Enabled = &disabledMemory
	cfg.Git.WorkDir = root
	cfg.Tools.CustomTools = []config.CustomToolConfig{{Name: "custom_local", Command: []string{custom}, WorkingDir: root}}
	var output bytes.Buffer
	require.NoError(t, newPromptCmd(&output).Run(ctx, cfg, []string{"--approve", "auto", "complete local journey"}))
	physicalID := physicalSessionIDFromOutput(t, output.String())
	require.Contains(t, output.String(), "all local actions completed")
	readback, err := newDurableCLISession(ctx, store, "inspect", physicalID, "")
	require.NoError(t, err)
	snapshot, err := readback.snapshotSelected()
	require.NoError(t, err)
	receipts := map[string]int{}
	proofs := map[string]int{}
	for _, row := range snapshot.DurableEntries {
		if row.Type == session.EntryTypeTool && row.ActionState == "" {
			require.Equal(t, string(core.ReceiptOutcomeSuccess), row.ReceiptOutcome, row.ToolName+": "+row.Content)
			require.Equal(t, core.DispatchStateDispatched, row.DispatchState)
			receipts[row.ToolName]++
		}
		if row.ActionState == session.ActionStateExecution && row.CompletionProof != "" {
			proofs[row.ToolName]++
		}
	}
	for _, action := range actions {
		require.Positive(t, receipts[action.name], action.name)
		require.Positive(t, proofs[action.name], action.name)
	}
	pending, err := session.InspectPendingActions(snapshot)
	require.NoError(t, err)
	require.Empty(t, pending)
	require.NoError(t, readback.requestClose(ctx))
	output.Reset()
	require.NoError(t, newPromptCmd(&output).Run(ctx, cfg, []string{"--approve", "auto", "--resume-session", physicalID, "inspect process state after restart"}))
	require.Contains(t, output.String(), "restart uses fresh process stores")
	bodies := script.requestBodies()
	require.Contains(t, bodies[len(bodies)-1], `task_get: task \"task-1\" not found`)
	calls, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, "calledcalled", string(calls), "resume must not replay either custom call")
}

func TestDurableReviewedCustomDeniedBeforeProcessStart(t *testing.T) {
	for _, mode := range []string{"deny", "plan", "replacement", "untrusted", "child-whitelist"} {
		t.Run(mode, func(t *testing.T) {
			configureDurableCLISessionTest(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			root, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			t.Chdir(root)
			marker := filepath.Join(root, "must-not-exist")
			command := filepath.Join(root, "custom")
			require.NoError(t, os.WriteFile(command, []byte("#!/bin/sh\nprintf effect > '"+marker+"'\n"), 0o700))
			previous := approval.GetTrustManager()
			manager := approval.NewDefaultTrustManager(nil)
			if mode != "untrusted" {
				require.NoError(t, manager.TrustProject(ctx, root))
			}
			approval.RegisterTrustManager(manager)
			t.Cleanup(func() { approval.RegisterTrustManager(previous) })
			responses := []string{durableApprovalToolResponse(t, "denied-custom", "custom_denied", map[string]any{}), `{"choices":[{"message":{"role":"assistant","content":"denial observed"}}]}`}
			if mode == "child-whitelist" {
				responses = append([]string{durableApprovalToolResponse(t, "restricted-child", "dispatch_subagent", map[string]any{"prompt": "restricted child", "tools": []string{"task_list"}, "max_turns": 3})}, responses...)
				responses = append(responses, `{"choices":[{"message":{"role":"assistant","content":"parent observed denial"}}]}`)
			}
			server, _ := newDurablePromptServer(t, responses...)
			defer server.Close()
			cfg := newAssembleTestConfig(t)
			disabledMemory := false
			cfg.Memory.Enabled = &disabledMemory
			cfg.Git.WorkDir, cfg.Provider.BaseURL = root, server.URL
			cfg.Session.StorePath = filepath.Join(t.TempDir(), "sessions")
			cfg.Tools.CustomTools = []config.CustomToolConfig{{Name: "custom_denied", Command: []string{command}, WorkingDir: root}}
			durable, err := newDurableCLISession(ctx, cfg.Session.StorePath, "deny", "", "")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, durable.requestClose(context.Background())) })
			approvalMode := ApproveAuto
			if mode == "deny" {
				approvalMode = ApproveDeny
			}
			assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(approvalMode), withDurableCLISession(durable))
			require.NoError(t, err)
			t.Cleanup(assembly.Cleanup)
			assembly.LoopAgent.WithToolSearchThreshold(0)
			if mode == "plan" {
				require.NoError(t, assembly.PlanCtrl.Enter(ctx, "read only"))
			}
			if mode == "replacement" {
				require.NoError(t, assembly.ToolRegistry.Register(ctx, tools.NewCustomCommandTool("custom_denied", "", []string{command}, nil, nil, 0, root)))
			}
			runCtx, sink, err := durable.bindRun(ctx)
			require.NoError(t, err)
			require.NoError(t, hydrateDurableRunHistory(assembly.Agent, sink))
			stream, err := assembly.Harness.Submit(runCtx, "observe denial")
			require.NoError(t, err)
			for range stream.Events() {
			}
			_, err = stream.Result()
			require.NoError(t, err)
			sink.CloseRunCommit()
			_, err = os.Stat(marker)
			require.ErrorIs(t, err, os.ErrNotExist)
			snapshot, err := durable.snapshotSelected()
			require.NoError(t, err)
			for _, row := range snapshot.DurableEntries {
				if row.ToolName == "custom_denied" && row.Type == session.EntryTypeTool && row.ActionState == "" {
					require.Equal(t, core.DispatchStateNotDispatched, row.DispatchState)
				}
			}
		})
	}
}

func TestPromptDurableCustomUnknownPersistsWithoutReplay(t *testing.T) {
	configureDurableCLISessionTest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Chdir(root)
	marker := filepath.Join(root, "effect")
	command := filepath.Join(root, "custom")
	require.NoError(t, os.WriteFile(command, []byte("#!/bin/sh\nprintf called >> '"+marker+"'\n/bin/sleep 60 >/dev/null 2>&1 &\nexit 124\n"), 0o700))
	previous := approval.GetTrustManager()
	manager := approval.NewDefaultTrustManager(nil)
	require.NoError(t, manager.TrustProject(ctx, root))
	approval.RegisterTrustManager(manager)
	t.Cleanup(func() { approval.RegisterTrustManager(previous) })
	server, script := newDurablePromptServer(t, durableApprovalToolResponse(t, "unknown-custom", "custom_unknown", map[string]any{}))
	defer server.Close()
	store := filepath.Join(t.TempDir(), "sessions")
	cfg := durablePromptConfig(t, server.URL, store)
	disabledMemory := false
	cfg.Memory.Enabled = &disabledMemory
	cfg.Git.WorkDir = root
	cfg.Tools.CustomTools = []config.CustomToolConfig{{Name: "custom_unknown", Command: []string{command}, WorkingDir: root}}
	var output bytes.Buffer
	err = newPromptCmd(&output).Run(ctx, cfg, []string{"--approve", "auto", "perform one action"})
	require.ErrorIs(t, err, core.ErrRunCommitQuiescenceUnproven)
	require.ErrorContains(t, err, "child processes remained after the shell exited")
	require.ErrorContains(t, err, "exit status 124")
	id := physicalSessionIDFromOutput(t, output.String())
	resumed, err := newDurableCLISession(ctx, store, "inspect unknown", id, "")
	require.NoError(t, err)
	defer func() { require.NoError(t, resumed.requestClose(context.Background())) }()
	snapshot, err := resumed.snapshotSelected()
	require.NoError(t, err)
	pending, err := session.InspectPendingActions(snapshot)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "custom_unknown", pending[0].ToolName)
	for _, row := range snapshot.DurableEntries {
		if row.ToolCallID == "unknown-custom" {
			require.Empty(t, row.CompletionProof)
			require.NotEqual(t, session.CheckpointExecutionResolution, row.CheckpointKind)
		}
	}
	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, "called", string(data))
	require.Len(t, script.requestBodies(), 1)
}
