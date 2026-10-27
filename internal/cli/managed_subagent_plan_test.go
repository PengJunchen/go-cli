package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestManagedChildParentPlanRevokesPreparedBashAndWrite(t *testing.T) {
	for _, tool := range []string{"bash", "write"} {
		for _, exit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/exit=%t", tool, exit), func(t *testing.T) {
				parent := core.NewDefaultPlanModeController()
				assembly := newChildPlanTestAssembly(t, parent, WithApproveMode(ApproveAuto), WithNoSandbox())
				path := filepath.Join(t.TempDir(), "must-not-exist")
				call := tools.ToolCall{ID: "prepared-child", Name: tool, Args: map[string]any{"path": path, "content": "forbidden"}}
				if tool == "bash" {
					call.Args = map[string]any{"command": fmt.Sprintf("printf wrote > %q", path)}
				}
				invocation, err := assembly.ToolRegistry.(tools.ToolCallPreparer).PrepareToolCall(t.Context(), call)
				require.NoError(t, err)
				require.NoError(t, parent.Enter(t.Context(), "revoke prepared child"))
				if exit {
					require.NoError(t, parent.Exit(t.Context(), "old child permit stays stale"))
				}
				_, err = invocation.Dispatch(t.Context())
				require.ErrorIs(t, err, tools.ErrDispatchFenceChanged)
				require.False(t, invocation.DispatchCommitted())
				_, err = os.Stat(path)
				require.True(t, os.IsNotExist(err))
				if exit {
					fresh, err := assembly.ToolRegistry.(tools.ToolCallPreparer).PrepareToolCall(t.Context(), call)
					require.NoError(t, err)
					_, err = fresh.Dispatch(t.Context())
					require.NoError(t, err)
					_, err = os.Stat(path)
					require.NoError(t, err, "fresh child authority after parent Exit must remain usable")
				}
			})
		}
	}
}

func TestManagedChildParentPlanRevokesWaitingApproval(t *testing.T) {
	parent := core.NewDefaultPlanModeController()
	requests := make(chan approval.InteractiveRequest, 1)
	assembly := newChildPlanTestAssembly(t, parent, WithApproveMode(ApproveAsk), WithApprovalChannel(requests))
	path := filepath.Join(t.TempDir(), "denied-after-approval")
	result := make(chan error, 1)
	go func() {
		_, err := assembly.ToolRegistry.(tools.ToolCallPreparer).PrepareToolCall(t.Context(), tools.ToolCall{ID: "waiting-child", Name: "write", Args: map[string]any{"path": path, "content": "no"}})
		result <- err
	}()
	var request approval.InteractiveRequest
	select {
	case request = <-requests:
	case <-time.After(5 * time.Second):
		t.Fatal("child approval did not start")
	}
	require.NoError(t, parent.Enter(t.Context(), "revoke while user decides"))
	require.NoError(t, parent.Exit(t.Context(), "the prior proposal remains stale"))
	request.ResponseCh <- approval.InteractiveResponse{Decision: approval.InteractiveAllow}
	require.ErrorIs(t, <-result, tools.ErrDispatchFenceChanged)
	_, err := os.Stat(path)
	require.True(t, os.IsNotExist(err))
}

func TestManagedChildLongBashDoesNotHoldParentPlanBoundary(t *testing.T) {
	parent := core.NewDefaultPlanModeController()
	assembly := newChildPlanTestAssembly(t, parent, WithApproveMode(ApproveAuto), WithNoSandbox())
	started := filepath.Join(t.TempDir(), "started")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	invocation, err := assembly.ToolRegistry.(tools.ToolCallPreparer).PrepareToolCall(ctx, tools.ToolCall{ID: "live-child", Name: "bash", Args: map[string]any{"command": fmt.Sprintf("printf started > %q; sleep 30", started)}})
	require.NoError(t, err)
	dispatched := make(chan error, 1)
	go func() { _, err := invocation.Dispatch(ctx); dispatched <- err }()
	deadline := time.After(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("child Bash did not start")
		case <-time.After(time.Millisecond):
		}
	}
	entered := make(chan error, 1)
	go func() { entered <- parent.Enter(t.Context(), "must not wait for child process") }()
	select {
	case err := <-entered:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("parent Plan transition waited for child process")
	}
	cancel()
	select {
	case <-dispatched:
	case <-time.After(5 * time.Second):
		t.Fatal("child process did not join")
	}
}

func newChildPlanTestAssembly(t *testing.T, parent core.PlanModeController, options ...AssembleOption) *AgentAssembly {
	t.Helper()
	cfg := newAssembleTestConfig(t)
	options = append(options, func(c *assembleConfig) { c.parentPlanCtrl = parent })
	assembly, err := AssembleAgent(context.Background(), cfg, "openai", "test-model", io.Discard, options...)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	return assembly
}
