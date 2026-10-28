package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReviewedGitMaintenanceRunsInForegroundWithoutChangingRepositoryConfig(t *testing.T) {
	git := setupGitRepo(t)
	for _, key := range []string{"maintenance.auto", "maintenance.autoDetach", "gc.autoDetach"} {
		runGitInDir(t, git.cwd, "config", "--local", key, "true")
	}
	configPath := filepath.Join(git.cwd, ".git", "config")
	before, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(git.cwd, ".git", "hooks", "post-commit"), []byte("#!/bin/sh\nprintf hook-ran > .git/reviewed-hook\n"), 0o700))
	tracePath := filepath.Join(t.TempDir(), "trace.jsonl")
	t.Setenv("GIT_TRACE2_EVENT", tracePath)
	writeFileInDir(t, git.cwd, "README.md", "# Foreground maintenance\n")
	ctx := context.Background()
	registry := NewMiddlewareToolRegistry(NewDefaultToolRegistry())
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	require.NoError(t, RegisterReviewedTool(ctx, registry, NewGitCommitTool(git), nil))
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "git_commit", Args: map[string]any{"message": "foreground", "add_all": true}})
	require.NoError(t, err)
	_, err = invocation.Dispatch(ctx)
	require.NoError(t, err)
	require.Equal(t, CompletionInlinePipelineJoined, invocation.CompletionSignal().Proof())
	hook, err := os.ReadFile(filepath.Join(git.cwd, ".git", "reviewed-hook"))
	require.NoError(t, err)
	require.Equal(t, "hook-ran", string(hook))
	after, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, before, after, "command-scoped maintenance settings must not rewrite repository configuration")
	assertGitMaintenanceJoinedInForeground(t, tracePath)
}

func assertGitMaintenanceJoinedInForeground(t *testing.T, tracePath string) {
	t.Helper()
	trace, err := os.ReadFile(tracePath)
	require.NoError(t, err)
	type traceEvent struct {
		Event     string   `json:"event"`
		SID       string   `json:"sid"`
		ChildID   int      `json:"child_id"`
		Code      int      `json:"code"`
		Argv      []string `json:"argv"`
		Category  string   `json:"category"`
		Label     string   `json:"label"`
		Hierarchy string   `json:"hierarchy"`
	}
	type childIdentity struct {
		sid string
		id  int
	}
	maintenance := map[childIdentity]bool{}
	exits := map[childIdentity]int{}
	hierarchies := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(trace)), "\n") {
		var event traceEvent
		require.NoError(t, json.Unmarshal([]byte(line), &event))
		if event.Event == "cmd_name" {
			hierarchies[event.SID] = event.Hierarchy
		}
		if event.Event == "child_start" && strings.Contains(strings.Join(event.Argv, " "), "maintenance run --auto") {
			require.Contains(t, event.Argv, "--no-detach", "automatic maintenance must remain inside the command owner: %s", hierarchies[event.SID])
			require.NotContains(t, event.Argv, "--detach")
			maintenance[childIdentity{event.SID, event.ChildID}] = true
		}
		if event.Event == "child_exit" {
			exits[childIdentity{event.SID, event.ChildID}] = event.Code
		}
		require.False(t, event.Category == "maintenance" && event.Label == "detach", "maintenance must not enter its daemonization region")
	}
	require.NotEmpty(t, maintenance, "real Git automatic maintenance must actually run")
	for child := range maintenance {
		code, joined := exits[child]
		require.True(t, joined, "Git must wait for each automatic maintenance child")
		require.Zero(t, code)
	}
}

func TestReviewedGitBackgroundHookStillHasUnknownOutcome(t *testing.T) {
	git := setupGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(git.cwd, ".git", "hooks", "post-commit"), []byte("#!/bin/sh\n/bin/sleep 4 &\nprintf hook-ran > .git/reviewed-hook\n"), 0o700))
	writeFileInDir(t, git.cwd, "README.md", "# Background hook\n")
	ctx := context.Background()
	registry := NewMiddlewareToolRegistry(NewDefaultToolRegistry())
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	require.NoError(t, RegisterReviewedTool(ctx, registry, NewGitCommitTool(git), nil))
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "git_commit", Args: map[string]any{"message": "hook side effect", "add_all": true}})
	require.NoError(t, err)
	_, err = invocation.Dispatch(ctx)
	require.Error(t, err)
	require.True(t, unresolvedExecutionError(err))
	require.Empty(t, invocation.CompletionSignal().Proof())
	hook, readErr := os.ReadFile(filepath.Join(git.cwd, ".git", "reviewed-hook"))
	require.NoError(t, readErr)
	require.Equal(t, "hook-ran", string(hook))
	require.Equal(t, "hook side effect", strings.TrimSpace(runGitInDir(t, git.cwd, "log", "-1", "--format=%s")))
}
