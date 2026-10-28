package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReviewedRuntimeCustomPreservesArgumentsAndCompletion(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry()
	registry := NewMiddlewareToolRegistry(NewDeferredToolRegistryAdapter(base))
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	command := []string{"/bin/sh", "-c", `printf '%s\n' "$@"`, "custom"}
	tool := NewCustomCommandTool("custom_exact", "", command, []string{"static"}, nil, 0, t.TempDir())
	command[0] = "/not-the-captured-command"
	require.NoError(t, RegisterReviewedTool(ctx, registry, tool, nil))
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: tool.Name(), Args: map[string]any{"input": "a b\n$(echo unsafe);'"}})
	require.NoError(t, err)
	supported, err := PreparedCompletionSupported(ctx, invocation, nil)
	require.NoError(t, err)
	require.True(t, supported)
	result, err := invocation.Dispatch(ctx)
	require.NoError(t, err)
	require.Equal(t, "static\na b\n$(echo unsafe);'\n", result.Output)
	require.Equal(t, CompletionInlinePipelineJoined, invocation.CompletionSignal().Proof())
	require.Equal(t, []TrustedToolEffect{EffectUnknown}, invocation.EffectSet().Values())
	// A generic replacement cannot retain the reviewed registration contract.
	require.NoError(t, registry.Register(ctx, tool))
	replaced, err := registry.PrepareToolCall(ctx, ToolCall{Name: tool.Name(), Args: map[string]any{}})
	require.NoError(t, err)
	require.Nil(t, replaced.CompletionSignal().Done())
}

func TestReviewedRuntimeStateRejectsCallbacksAndExternalStores(t *testing.T) {
	ctx := context.Background()
	registry := NewMiddlewareToolRegistry(NewDefaultToolRegistry())
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	store := NewTaskStore()
	create := NewTaskCreateTool(store)
	require.NoError(t, RegisterReviewedTool(ctx, registry, create, nil))
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: create.Name(), Args: map[string]any{"title": "real task"}})
	require.NoError(t, err)
	_, err = invocation.Dispatch(ctx)
	require.NoError(t, err)
	require.Len(t, store.List(), 1)
	require.Equal(t, CompletionInlinePipelineJoined, invocation.CompletionSignal().Proof())
	callback := NewTodoWriteTool(NewTodoStore(), WithTodoWriteChangeHandler(func(TodoSnapshot) error { return nil }))
	require.Error(t, RegisterReviewedTool(ctx, registry, callback, nil))
	fileStore, err := NewDefaultGoalStore(filepath.Join(t.TempDir(), "goals.jsonl"))
	require.NoError(t, err)
	require.Error(t, RegisterReviewedTool(ctx, registry, NewGoalCreateTool(fileStore), nil))
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = create.Execute(canceled, ToolCall{Args: map[string]any{"title": "must not exist"}})
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, store.List(), 1)
}

func TestReviewedRuntimeGitUnknownMergeStartsNoProbe(t *testing.T) {
	bin := t.TempDir()
	trace := filepath.Join(t.TempDir(), "commands")
	installFakeGit(t, bin, `printf '%s\n' "$*" >> '`+trace+`'
case "$*" in
  *rev-parse*) exit 0 ;;
  *merge*) /bin/sleep 4 ;;
esac
`)
	ctx, cancel := context.WithCancel(context.Background())
	observerDone := make(chan struct{})
	go func() {
		defer close(observerDone)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				data, _ := os.ReadFile(trace)
				if strings.Contains(string(data), "merge ") {
					cancel()
					return
				}
			}
		}
	}()
	defer cancel()
	_, err := NewDefaultGitTool(t.TempDir()).Merge(ctx, "branch")
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled))
	data, err := os.ReadFile(trace)
	require.NoError(t, err)
	require.NotContains(t, string(data), "--diff-filter=U")
	<-observerDone
}

func TestReviewedRuntimeAllGitToolsExecuteAgainstLocalRepository(t *testing.T) {
	ctx := context.Background()
	git := setupGitRepo(t)
	mainBranch := currentBranch(t, git.cwd)
	bare := t.TempDir()
	runGitInDir(t, bare, "init", "--bare")
	// A server's configuration is independent of the client's command-scoped
	// settings. This owned local remote must also keep its maintenance joined.
	for _, key := range []string{"maintenance.autoDetach", "gc.autoDetach"} {
		runGitInDir(t, bare, "config", "--local", key, "false")
		require.Equal(t, "false", strings.TrimSpace(runGitInDir(t, bare, "config", "--local", "--bool", "--get", key)))
	}
	runGitInDir(t, git.cwd, "remote", "add", "origin", bare)
	tracePath := filepath.Join(t.TempDir(), "trace.jsonl")
	t.Setenv("GIT_TRACE2_EVENT", tracePath)
	base := NewDefaultToolRegistry()
	require.NoError(t, RegisterDefaults(ctx, base, WithRegisteredGitTool(git)))
	registry := NewMiddlewareToolRegistry(base)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	seen := map[string]bool{}
	invoke := func(name string, args map[string]any) *ToolResult {
		t.Helper()
		invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: name, Args: args})
		require.NoError(t, err, name)
		supported, err := PreparedCompletionSupported(ctx, invocation, nil)
		require.NoError(t, err)
		require.True(t, supported, name)
		result, err := invocation.Dispatch(ctx)
		require.NoError(t, err, name)
		require.NotNil(t, result)
		require.Equal(t, CompletionInlinePipelineJoined, invocation.CompletionSignal().Proof(), name)
		require.Equal(t, []TrustedToolEffect{EffectUnknown}, invocation.EffectSet().Values(), name)
		seen[name] = true
		return result
	}
	writeFileInDir(t, git.cwd, "README.md", "# Changed\n")
	invoke("git_diff", map[string]any{})
	invoke("git_status", map[string]any{})
	invoke("git_commit", map[string]any{"message": "changed", "add_all": true})
	invoke("git_log", map[string]any{})
	invoke("git_branch", map[string]any{})
	invoke("git_blame", map[string]any{"file": "README.md"})
	invoke("git_remote", map[string]any{})
	invoke("git_push", map[string]any{"remote": "origin", "branch": mainBranch})
	invoke("git_fetch", map[string]any{"remote": "origin"})
	invoke("git_pull", map[string]any{"remote": "origin", "branch": mainBranch})
	invoke("git_create_branch", map[string]any{"name": "reviewed-feature"})
	invoke("git_checkout", map[string]any{"branch": "reviewed-feature"})
	writeFileInDir(t, git.cwd, "README.md", "# Stashed\n")
	invoke("git_stash", map[string]any{})
	invoke("git_stash_pop", map[string]any{})
	invoke("git_reset", map[string]any{"mode": "hard"})
	writeFileInDir(t, git.cwd, "feature.txt", "feature\n")
	invoke("git_commit", map[string]any{"message": "feature", "add_all": true})
	invoke("git_checkout", map[string]any{"branch": mainBranch})
	invoke("git_merge", map[string]any{"branch": "reviewed-feature"})
	invoke("git_revert", map[string]any{"commit": "HEAD"})
	require.Len(t, seen, 17)
	assertGitMaintenanceJoinedInForeground(t, tracePath)
}

func TestReviewedRuntimeGitChecksFenceBetweenActualCommands(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	started := filepath.Join(root, "started")
	release := filepath.Join(root, "release")
	trace := filepath.Join(root, "trace")
	installFakeGit(t, t.TempDir(), `printf '%s\n' "$*" >> '`+trace+`'
case "$*" in
  *rev-parse*) printf ready > '`+started+`'; while [ ! -f '`+release+`' ]; do /bin/sleep .01; done ;;
  *) exit 0 ;;
esac
`)
	registry := NewMiddlewareToolRegistry(NewDefaultToolRegistry())
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	require.NoError(t, RegisterReviewedTool(ctx, registry, NewGitCommitTool(NewDefaultGitTool(root)), nil))
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "git_commit", Args: map[string]any{"message": "blocked", "add_all": true}})
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { _, runErr := invocation.Dispatch(ctx); done <- runErr }()
	require.Eventually(t, func() bool { _, statErr := os.Stat(started); return statErr == nil }, time.Second, time.Millisecond)
	require.NoError(t, registry.Register(ctx, NewCustomCommandTool("invalidate", "", []string{"/bin/true"}, nil, nil, 0, "")))
	require.NoError(t, os.WriteFile(release, nil, 0o600))
	require.ErrorIs(t, <-done, ErrDispatchFenceChanged)
	data, err := os.ReadFile(trace)
	require.NoError(t, err)
	require.NotContains(t, string(data), "add -A")
	require.NotContains(t, string(data), "commit -m")
}

func TestReviewedRuntimeGitRejectsSubstitutedAndNilDependencies(t *testing.T) {
	ctx := context.Background()
	registry := NewDefaultToolRegistry()
	for _, definition := range []ToolDefinition{NewGitStatusTool(nil), NewGitStatusTool((*DefaultGitTool)(nil)), NewGitDiffTool(&lifecycleTestGitTool{})} {
		require.Error(t, RegisterReviewedTool(ctx, registry, definition, nil))
	}
}

func TestReviewedRuntimePRSuccessAndLostReply(t *testing.T) {
	for _, mode := range []string{"success", "malformed", "lost"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			marker := filepath.Join(root, "effect")
			body := "#!/bin/sh\nprintf called >> '" + marker + "'\n"
			switch mode {
			case "success":
				body += "printf 'https://github.example/owner/repo/pull/42\\n'\n"
			case "malformed":
				body += "printf 'no valid result\\n'\n"
			case "lost":
				body += "exit 1\n"
			}
			require.NoError(t, os.WriteFile(filepath.Join(root, "gh"), []byte(body), 0o700))
			t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
			ctx := context.Background()
			registry := NewMiddlewareToolRegistry(NewDefaultToolRegistry())
			configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
			require.NoError(t, RegisterReviewedTool(ctx, registry, NewGitPRCreateTool(root), nil))
			invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "git_pr_create", Args: map[string]any{"title": "a\n$(literal)", "base": "main", "head": "feature"}})
			require.NoError(t, err)
			_, err = invocation.Dispatch(ctx)
			if mode == "success" {
				require.NoError(t, err)
				require.Equal(t, CompletionInlinePipelineJoined, invocation.CompletionSignal().Proof())
			} else {
				require.Error(t, err)
				require.True(t, unresolvedExecutionError(err))
				require.Empty(t, invocation.CompletionSignal().Proof())
			}
			data, err := os.ReadFile(marker)
			require.NoError(t, err)
			require.Equal(t, "called", string(data))
		})
	}
}

func TestReviewedRuntimeGoalSnapshotsDoNotShareMutableState(t *testing.T) {
	ctx := context.Background()
	store, err := NewDefaultGoalStore("")
	require.NoError(t, err)
	goal, err := store.Create(ctx, "goal", "", "")
	require.NoError(t, err)
	require.NoError(t, store.AddTask(ctx, goal.ID, "task-1"))
	require.NoError(t, store.Update(ctx, goal.ID, WithGoalStatus(GoalStatusCompleted)))
	copy, err := store.Get(ctx, goal.ID)
	require.NoError(t, err)
	copy.TaskIDs[0] = "forged"
	*copy.CompletedAt = time.Time{}
	items, err := store.List(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"task-1"}, items[0].TaskIDs)
	require.False(t, items[0].CompletedAt.IsZero())
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = store.Create(canceled, "canceled", "", "")
	require.ErrorIs(t, err, context.Canceled)
}

func TestReviewedRuntimeOrdinaryPlatformFallbackCompatibility(t *testing.T) {
	ctx := context.Background()
	cmd := exec.Command("/bin/sh", "-c", `printf '%s\n' "$CUSTOM_VALUE"; pwd; printf stderr >&2`)
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"CUSTOM_VALUE=kept"}
	output, lifecycle, err := runUnmanagedCommand(ctx, cmd, 4096)
	require.NoError(t, err)
	require.Contains(t, output, "kept\n")
	require.Contains(t, output, cmd.Dir)
	require.Contains(t, output, "stderr")
	require.False(t, lifecycle.containmentProven, "legacy platform fallback cannot mint owned-process evidence")
	cmd = exec.Command("/bin/sh", "-c", `printf 123456789; printf abcdef >&2`)
	output, _, err = runUnmanagedCommand(ctx, cmd, 5)
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(output, "\n[output truncated]"))
	require.Len(t, strings.TrimSuffix(output, "\n[output truncated]"), 5)
}

func TestReviewedRuntimeArtifactCloseWaitsForActualOwner(t *testing.T) {
	root := t.TempDir()
	started, release := filepath.Join(root, "started"), filepath.Join(root, "release")
	script := `printf ready > '` + started + `'; while [ ! -f '` + release + `' ]; do /bin/sleep .01; done; printf complete`
	tool := NewCustomCommandTool("owned", "", []string{"/bin/sh", "-c", script}, nil, nil, 0, root)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := tool.Execute(ctx, ToolCall{}); finished <- err }()
	require.Eventually(t, func() bool { _, err := os.Stat(started); return err == nil }, time.Second, time.Millisecond)
	closeCtx, stopClose := context.WithTimeout(ctx, 10*time.Millisecond)
	require.ErrorIs(t, tool.Close(closeCtx), context.DeadlineExceeded)
	stopClose()
	require.NoError(t, os.WriteFile(release, nil, 0o600))
	require.NoError(t, <-finished)
	require.NoError(t, tool.Close(ctx))
	_, err := tool.Execute(ctx, ToolCall{})
	require.Error(t, err, "closed artifact owner rejects later execution")
}

func TestReviewedRuntimeArtifactCloseDistinguishesPendingAndLegacyJoins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	tool := NewCustomCommandTool("pending", "", []string{"/bin/true"}, nil, nil, 0, "")
	require.NoError(t, tool.beginExecution())
	joined := make(chan struct{})
	tool.finishExecution(managedLocalResult{executionDone: joined}, context.Canceled)
	closeCtx, stop := context.WithCancel(ctx)
	stop()
	require.Error(t, tool.Close(closeCtx))
	close(joined)
	require.NoError(t, tool.Close(ctx), "the exact later owner join permits artifact cleanup, without changing a durable proof")
	legacy := NewCustomCommandTool("legacy", "", []string{"/bin/true"}, nil, nil, 0, "")
	require.NoError(t, legacy.beginExecution())
	_, lifecycle, err := runUnmanagedCommand(ctx, exec.Command("/bin/sh", "-c", "exit 0"), 10)
	require.NoError(t, err)
	legacy.finishExecution(lifecycle, err)
	require.NoError(t, legacy.Close(ctx), "normal legacy leader and stdio join is sufficient for its ordinary artifact cleanup")
	require.False(t, lifecycle.containmentProven)
}

func TestReviewedRuntimeOrdinaryFallbackRechecksFinalStartFence(t *testing.T) {
	for name, domain := range map[string]DispatchFenceDomain{"registry": DispatchFenceRegistry, "plan": DispatchFencePlan} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			registry := NewMiddlewareToolRegistry(NewDefaultToolRegistry())
			configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
			root := t.TempDir()
			marker := filepath.Join(root, "effect")
			require.NoError(t, RegisterReviewedTool(ctx, registry, NewCustomCommandTool("ordinary_fallback", "", []string{"/bin/sh"}, nil, nil, 0, root), nil))
			invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "ordinary_fallback", Args: map[string]any{}})
			require.NoError(t, err)
			executeCtx := invocation.(*preparedInvocation).withEffectStartGuard(ctx)
			_, err = registry.DispatchFence().Publish(domain, nil, func() {})
			require.NoError(t, err)
			command := exec.Command("/bin/sh", "-c", "printf ran > '"+marker+"'")
			command.Dir = root
			_, lifecycle, err := runUnmanagedCommand(executeCtx, command, 1024)
			require.ErrorIs(t, err, ErrDispatchFenceChanged)
			_, err = os.Stat(marker)
			require.ErrorIs(t, err, os.ErrNotExist)
			require.False(t, lifecycle.containmentProven, "ordinary rejection cannot grant durable process proof")
		})
	}
}

func TestReviewedRuntimeOrdinaryFallbackReleasesStartFenceBeforeWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	registry := NewMiddlewareToolRegistry(NewDefaultToolRegistry())
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	root := t.TempDir()
	require.NoError(t, RegisterReviewedTool(ctx, registry, NewCustomCommandTool("ordinary_fallback", "", []string{"/bin/sh"}, nil, nil, 0, root), nil))
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "ordinary_fallback", Args: map[string]any{}})
	require.NoError(t, err)
	executeCtx := invocation.(*preparedInvocation).withEffectStartGuard(ctx)
	marker, release := filepath.Join(root, "started"), filepath.Join(root, "release")
	command := exec.Command("/bin/sh", "-c", "printf started > '"+marker+"'; while [ ! -f '"+release+"' ]; do sleep 0.01; done")
	done := make(chan struct{})
	var lifecycle managedLocalResult
	var runErr error
	go func() {
		defer close(done)
		_, lifecycle, runErr = runUnmanagedCommand(executeCtx, command, 1024)
	}()
	defer func() {
		_ = os.WriteFile(release, nil, 0o600)
		<-done
	}()
	require.Eventually(t, func() bool { _, err := os.Stat(marker); return err == nil }, time.Second, time.Millisecond)
	published := make(chan error, 1)
	go func() {
		_, err := registry.DispatchFence().Publish(DispatchFencePlan, nil, func() {})
		published <- err
	}()
	select {
	case err := <-published:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("ordinary fallback retained authority guard while waiting for the child")
	}
	require.NoError(t, os.WriteFile(release, nil, 0o600))
	<-done
	require.NoError(t, runErr)
	require.True(t, lifecycle.ordinaryPipelineJoined)
	require.False(t, lifecycle.containmentProven)
}
