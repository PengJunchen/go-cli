package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestPlanModeToolInterceptor_AllowsWhenInactive(t *testing.T) {
	ctrl := NewDefaultPlanModeController()
	interceptor := NewPlanModeToolInterceptor(ctrl)

	err := interceptor("write", "call-1", nil)
	require.NoError(t, err, "should allow when plan mode inactive")
}

func TestPlanModeToolInterceptor_BlocksWriteToolsWhenActive(t *testing.T) {
	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	interceptor := NewPlanModeToolInterceptor(ctrl)

	err := interceptor("write", "call-1", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blocked")
	assert.Contains(t, err.Error(), "write")
}

func TestPlanModeToolInterceptor_AllowsControlToolsWhenActive(t *testing.T) {
	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	interceptor := NewPlanModeToolInterceptor(ctrl)

	for _, name := range []string{
		"tool_search", "ask_user", "goal_get", "goal_list", "task_get", "task_list",
		"enter_plan_mode", "exit_plan_mode",
	} {
		err := interceptor(name, "call-1", nil)
		require.NoError(t, err, "%s should remain available as an explicit Plan control", name)
	}
}

func TestPlanModeToolInterceptor_FailsClosedForUnknownAndExternalTools(t *testing.T) {
	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	interceptor := NewPlanModeToolInterceptor(ctrl)

	for _, name := range []string{
		"read", "grep", "find", "ls", "lsp_query", "git_status", "git_diff", "web_fetch", "web_search",
		"git_commit", "git_pull", "dispatch_subagent", "mcp__server__write", "custom_side_effect", "extension_tool", "unknown_tool",
	} {
		err := interceptor(name, "call-1", nil)
		require.Error(t, err, "%s must be blocked in plan mode", name)
		assert.Contains(t, err.Error(), "blocked")
	}
}

func TestPlanModeToolWrapper_FailsClosedForUnknownAndExternalTools(t *testing.T) {
	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	executions := 0
	wrapped := NewPlanModeToolWrapper(ctrl)(func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
		executions++
		return &tools.ToolResult{}, nil
	})

	for _, name := range []string{"git_commit", "dispatch_subagent", "mcp__server__write", "custom_side_effect", "extension_tool", "unknown_tool"} {
		result, err := wrapped(context.Background(), tools.ToolCall{Name: name})
		require.ErrorIs(t, err, ErrPlanModeBlocked, "%s must be blocked", name)
		require.NotNil(t, result)
		assert.Equal(t, true, result.Metadata["plan_mode_blocked"])
	}
	assert.Zero(t, executions, "blocked tools must not reach the executor")
}

func TestPlanModeToolWrapperBlocksNetworkTools(t *testing.T) {
	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	executions := 0
	wrapped := NewPlanModeToolWrapper(ctrl)(func(_ context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		executions++
		return &tools.ToolResult{Output: call.Name}, nil
	})

	for _, name := range []string{"web_fetch", "web_search"} {
		result, err := wrapped(context.Background(), tools.ToolCall{Name: name})
		require.ErrorIs(t, err, ErrPlanModeBlocked, "%s is a network effect, not a local read", name)
		require.NotNil(t, result)
		assert.Equal(t, true, result.Metadata["plan_mode_blocked"])
	}
	assert.Zero(t, executions)
}

func TestPlanModeToolInterceptor_NilControllerAllowsAll(t *testing.T) {
	interceptor := NewPlanModeToolInterceptor(nil)

	err := interceptor("write", "call-1", nil)
	require.NoError(t, err, "nil controller should allow all tools")
}

// TestPlanModeMiddleware_RemovedOrRefactored verifies that the old
// PlanModeMiddleware type has been replaced by NewPlanModeToolInterceptor.
// The interceptor satisfies the ToolInterceptor type and blocks write tools
// when plan mode is active.
func TestPlanModeMiddleware_RemovedOrRefactored(t *testing.T) {
	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))

	// NewPlanModeToolInterceptor returns a valid ToolInterceptor.
	var _ ToolInterceptor = NewPlanModeToolInterceptor(ctrl)

	interceptor := NewPlanModeToolInterceptor(ctrl)
	err := interceptor("mutation", "call-1", nil)
	require.Error(t, err, "write tool should be blocked in plan mode")
	assert.Contains(t, err.Error(), "mutation")
}
