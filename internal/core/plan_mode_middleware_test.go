package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestPlanModeToolInterceptor_AllowsReadToolsWhenActive(t *testing.T) {
	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	interceptor := NewPlanModeToolInterceptor(ctrl)

	err := interceptor("read", "call-1", nil)
	require.NoError(t, err, "read tools should not be blocked")
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
