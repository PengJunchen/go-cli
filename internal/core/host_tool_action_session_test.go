package core_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestHostToolActionPersistsRealGitWithoutModelAndRunsHooks(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, ".config"))
	ctx := context.Background()
	repo := filepath.Join(root, "repo")
	require.NoError(t, os.Mkdir(repo, 0o700))
	command := exec.Command("git", "init", "-b", "main", repo)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	canonical, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	workspaceID, err := session.WorkspaceIDForCanonicalRoot(canonical)
	require.NoError(t, err)
	_, prepared, sink := prepareRunCommitSession(t, root, workspaceID, "host action")
	defer func() { require.NoError(t, prepared.Close()) }()
	registry := tools.NewMiddlewareToolRegistry(tools.NewDefaultToolRegistry())
	require.NoError(t, tools.RegisterDefaults(ctx, registry, tools.WithRegisteredGitTool(tools.NewDefaultGitTool(repo)), tools.WithRegisteredBuiltinWhitelist([]string{"git_create_branch", "bash"}), tools.WithRegisteredBashSandbox(tools.AllowAllSandbox{})))
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	// An unborn repository cannot create another branch. Use a real initial commit.
	for _, args := range [][]string{{"config", "user.email", "test@example.invalid"}, {"config", "user.name", "Test"}, {"commit", "--allow-empty", "-m", "initial"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, runErr := cmd.CombinedOutput()
		require.NoError(t, runErr, "%s", out)
	}
	source := core.NewLoopAgent(core.WithTools(registry)) // deliberately no model
	hookMarker := filepath.Join(root, "hook-ran")
	var preCalls, postCalls atomic.Int32
	source.SetContextToolInterceptor(func(context.Context, string, string, map[string]any) error { preCalls.Add(1); return nil })
	source.SetPostToolActions(func(call llm.ToolCall, result *tools.ToolResult) ([]core.PostToolAction, error) {
		postCalls.Add(1)
		return []core.PostToolAction{{Call: llm.ToolCall{Name: "bash", Args: map[string]any{"command": fmt.Sprintf("printf hook > %q", hookMarker)}}, Timeout: time.Second}}, nil
	})
	runner := core.NewEinoTurnRunner(source)
	runner.SetRunSlotGuard(core.NewDefaultRunSlotGuard())
	runCtx := core.WithRunCommit(ctx, sink, core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID})
	result, err := runner.RunHostToolAction(runCtx, source, core.HostToolActionRequest{Description: "/session fork feature --git feature", Call: tools.ToolCall{Name: "git_create_branch", Args: map[string]any{"name": "feature"}}})
	require.NoError(t, err)
	require.Equal(t, core.RunOutcomeSuccess, result.Outcome)
	require.Equal(t, core.RunExecutionQuiescent, result.ExecutionState)
	require.Equal(t, core.DurablePublicationSynced, result.DurablePublication.State)
	require.EqualValues(t, 1, preCalls.Load())
	require.EqualValues(t, 1, postCalls.Load())
	hookBytes, err := os.ReadFile(hookMarker)
	require.NoError(t, err)
	require.Equal(t, "hook", string(hookBytes))
	cmd := exec.Command("git", "show-ref", "--verify", "refs/heads/feature")
	cmd.Dir = repo
	require.NoError(t, cmd.Run())
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	pending, err := session.InspectPendingActions(snapshot)
	require.NoError(t, err)
	require.Empty(t, pending)
	var receipt, terminal bool
	for _, row := range snapshot.DurableEntries {
		if row.ToolName == "git_create_branch" && row.ReceiptOutcome == core.ReceiptOutcomeSuccess {
			receipt = true
		}
		if row.CheckpointKind == session.CheckpointRunTerminal {
			terminal = true
		}
	}
	require.True(t, receipt)
	require.True(t, terminal)
}

func TestHostToolActionFailureAndUnknownContracts(t *testing.T) {
	for _, mode := range []string{"policy-deny", "pre-hook-deny", "whitelist-deny", "tool-error", "unknown", "busy"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, ".config"))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			canonical, err := filepath.EvalSymlinks(root)
			require.NoError(t, err)
			workspaceID, err := session.WorkspaceIDForCanonicalRoot(canonical)
			require.NoError(t, err)
			_, prepared, sink := prepareRunCommitSession(t, root, workspaceID, "host failure")
			defer func() { require.NoError(t, prepared.Close()) }()
			marker := filepath.Join(root, "effect")
			body := "printf called >> '" + marker + "'; exit 3"
			timeout := time.Duration(0)
			if mode == "unknown" {
				body = "printf called >> '" + marker + "'; /bin/sleep 3"
				timeout = 30 * time.Millisecond
			}
			registry := tools.NewMiddlewareToolRegistry(tools.NewDefaultToolRegistry())
			require.NoError(t, tools.RegisterReviewedTool(ctx, registry, tools.NewCustomCommandTool("host_action", "", []string{"/bin/sh", "-c", body}, nil, nil, timeout, root), nil))
			require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
				return tools.PreparedPolicyDecision{Allowed: mode != "policy-deny", Generations: request.Generations}, nil
			}))
			require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
			opts := []core.LoopOption{core.WithTools(registry)}
			if mode == "whitelist-deny" {
				opts = append(opts, core.WithAllowedToolNames([]string{"read"}))
			}
			source := core.NewLoopAgent(opts...)
			if mode == "pre-hook-deny" {
				source.SetContextToolInterceptor(func(context.Context, string, string, map[string]any) error { return errors.New("rejected") })
			}
			guard := core.NewDefaultRunSlotGuard()
			runner := core.NewEinoTurnRunner(source)
			runner.SetRunSlotGuard(guard)
			if mode == "busy" {
				claim, claimErr := guard.ClaimRun(ctx)
				require.NoError(t, claimErr)
				defer guard.Release(claim)
			}
			runCtx := core.WithRunCommit(ctx, sink, core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID})
			result, err := runner.RunHostToolAction(runCtx, source, core.HostToolActionRequest{Description: "explicit administrative action", Call: tools.ToolCall{Name: "host_action", Args: map[string]any{}}})
			require.Error(t, err)
			require.NotEqual(t, core.RunOutcomeSuccess, result.Outcome)
			snapshot, snapshotErr := prepared.Snapshot()
			require.NoError(t, snapshotErr)
			pending, pendingErr := session.InspectPendingActions(snapshot)
			require.NoError(t, pendingErr)
			if mode == "unknown" {
				require.Equal(t, core.RunOutcomeUnknown, result.Outcome)
				require.Len(t, pending, 1)
				for _, row := range snapshot.DurableEntries {
					if row.ToolName == "host_action" {
						require.Empty(t, row.CompletionProof)
					}
				}
			} else {
				require.Empty(t, pending)
			}
			data, readErr := os.ReadFile(marker)
			if mode == "tool-error" || mode == "unknown" {
				require.NoError(t, readErr)
				require.Equal(t, "called", string(data))
			} else {
				require.ErrorIs(t, readErr, os.ErrNotExist)
			}
		})
	}
}

func TestHostToolActionUsesOriginalRunnerLifecycleStreamAndTurnIDs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, ".config"))
	canonical, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	workspaceID, err := session.WorkspaceIDForCanonicalRoot(canonical)
	require.NoError(t, err)
	_, prepared, sink := prepareRunCommitSession(t, root, workspaceID, "host lifecycle")
	defer func() { require.NoError(t, prepared.Close()) }()
	registry := tools.NewMiddlewareToolRegistry(tools.NewDefaultToolRegistry())
	marker := filepath.Join(root, "started")
	require.NoError(t, tools.RegisterReviewedTool(t.Context(), registry, tools.NewCustomCommandTool("host_block", "", []string{"/bin/sh", "-c", "printf started > '" + marker + "'; /bin/sleep 10"}, nil, nil, 0, root), nil))
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	source := core.NewLoopAgent(core.WithTools(registry))
	runner := core.NewEinoTurnRunner(source)
	// An ordinary turn consumes the same identity sequence. Its model is
	// intentionally absent; host dispatch must bypass this attached Agent.
	runner.SetAgent(core.NewAgentImpl("ordinary", source))
	guard := core.NewDefaultRunSlotGuard()
	runner.SetRunSlotGuard(guard)
	_, err = runner.RunTurn(t.Context(), core.Submission{Type: core.SubmissionUserMessage, Content: "ordinary before host"})
	require.Error(t, err)
	first, err := runner.Get(t.Context(), "turn-1")
	require.NoError(t, err)
	require.Equal(t, "ordinary before host", first.Submission.Content)
	stream := core.NewEventStream(64)
	runner.SetStream(stream)
	callerCtx, callerCancel := context.WithCancel(t.Context())
	runCtx := core.WithRunCommit(callerCtx, sink, core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID})
	done := make(chan struct{})
	var result core.Result
	var runErr error
	go func() {
		defer close(done)
		result, runErr = runner.RunHostToolAction(runCtx, source, core.HostToolActionRequest{Description: "explicit lifecycle action", Call: tools.ToolCall{Name: "host_block", Args: map[string]any{}}})
	}()
	defer func() {
		callerCancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("host action failed to join")
		}
	}()
	require.Eventually(t, func() bool { _, err := os.Stat(marker); return err == nil }, 2*time.Second, time.Millisecond)
	id := runner.RunningTurnID()
	require.Equal(t, "turn-2", id)
	active, err := runner.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, core.TurnRunning, active.Status)
	require.NotNil(t, active.Result.StatusHandle)
	require.NoError(t, runner.Cancel(t.Context(), id))
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("original runner Cancel did not stop host action")
	}
	require.NoError(t, callerCtx.Err(), "only the original runner Cancel should stop this action")
	require.Error(t, runErr)
	waiter, ok := result.StatusHandle.(core.RunStatusWaiter)
	require.True(t, ok)
	joinCtx, cancelJoin := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancelJoin()
	status, _, err := waiter.WaitForCompletion(joinCtx)
	require.NoError(t, err)
	require.Equal(t, core.RunExecutionQuiescent, status.ExecutionState)
	finished, err := runner.Get(t.Context(), id)
	require.NoError(t, err)
	require.True(t, finished.Canceled)
	require.Equal(t, result.RunID, finished.RunID)
	require.Empty(t, runner.RunningTurnID())
	stream.Close() // direct runner callers own the stream drain lifecycle
	streamedCall := false
	for event := range stream.Events() {
		if event.Kind == "tool_call" && event.Content == "host_block" {
			streamedCall = true
		}
	}
	require.True(t, streamedCall, "the configured original stream must receive host events")
	runner.SetStream(nil)
	_, err = runner.RunTurn(t.Context(), core.Submission{Type: core.SubmissionUserMessage, Content: "ordinary after host"})
	require.Error(t, err)
	third, err := runner.Get(t.Context(), "turn-3")
	require.NoError(t, err)
	require.Equal(t, "ordinary after host", third.Submission.Content)
	firstAgain, err := runner.Get(t.Context(), "turn-1")
	require.NoError(t, err)
	require.Equal(t, first.Submission.Content, firstAgain.Submission.Content)
	select {
	case <-guard.CloseAdmission():
	case <-joinCtx.Done():
		t.Fatal("host wrapper retained the shared admission claim after join")
	}
}
