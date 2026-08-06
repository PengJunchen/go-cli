package tools

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePlanModeController is a test double for the PlanModeController interface.
type fakePlanModeController struct {
	mu          sync.Mutex
	active      bool
	enterReason string
	exitSummary string
	enterCalls  int
	exitCalls   int
	enterErr    error
	exitErr     error
}

func (f *fakePlanModeController) Enter(_ context.Context, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enterCalls++
	f.enterReason = reason
	if f.enterErr != nil {
		return f.enterErr
	}
	f.active = true
	return nil
}

func (f *fakePlanModeController) Exit(_ context.Context, summary string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exitCalls++
	f.exitSummary = summary
	if f.exitErr != nil {
		return f.exitErr
	}
	f.active = false
	return nil
}

func (f *fakePlanModeController) IsActive() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active
}

func TestEnterPlanModeTool_SatisfiesInterface(t *testing.T) {
	var _ ToolDefinition = (*EnterPlanModeTool)(nil)
}

func TestEnterPlanModeTool_NameAndDescription(t *testing.T) {
	tool := NewEnterPlanModeTool(&fakePlanModeController{})
	assert.Equal(t, "enter_plan_mode", tool.Name())
	assert.NotEmpty(t, tool.Description())
}

func TestEnterPlanModeTool_Execute_ActivatesPlanMode(t *testing.T) {
	fc := &fakePlanModeController{}
	tool := NewEnterPlanModeTool(fc)

	result, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-1",
		Name: "enter_plan_mode",
		Args: map[string]any{"reason": "need to investigate"},
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, "call-1", result.ToolCallID)
	assert.Contains(t, result.Output, "plan mode activated")
	assert.Contains(t, result.Output, "need to investigate")
	assert.True(t, fc.IsActive())
	assert.Equal(t, "need to investigate", fc.enterReason)
	assert.Equal(t, 1, fc.enterCalls)
}

func TestEnterPlanModeTool_Execute_NoReason(t *testing.T) {
	fc := &fakePlanModeController{}
	tool := NewEnterPlanModeTool(fc)

	result, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-2",
		Name: "enter_plan_mode",
		Args: map[string]any{},
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, fc.IsActive())
	assert.Empty(t, fc.enterReason)
}

func TestEnterPlanModeTool_Execute_PropagatesError(t *testing.T) {
	fc := &fakePlanModeController{enterErr: assert.AnError}
	tool := NewEnterPlanModeTool(fc)

	result, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-3",
		Name: "enter_plan_mode",
		Args: map[string]any{"reason": "fail"},
	})
	require.Error(t, err)
	assert.Nil(t, result)
	assert.ErrorIs(t, err, assert.AnError)
}

func TestExitPlanModeTool_SatisfiesInterface(t *testing.T) {
	var _ ToolDefinition = (*ExitPlanModeTool)(nil)
}

func TestExitPlanModeTool_NameAndDescription(t *testing.T) {
	tool := NewExitPlanModeTool(&fakePlanModeController{})
	assert.Equal(t, "exit_plan_mode", tool.Name())
	assert.NotEmpty(t, tool.Description())
}

func TestExitPlanModeTool_Execute_DeactivatesPlanMode(t *testing.T) {
	fc := &fakePlanModeController{active: true}
	tool := NewExitPlanModeTool(fc)

	result, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-4",
		Name: "exit_plan_mode",
		Args: map[string]any{"summary": "plan is ready"},
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, "call-4", result.ToolCallID)
	assert.Contains(t, result.Output, "plan mode deactivated")
	assert.Contains(t, result.Output, "plan is ready")
	assert.False(t, fc.IsActive())
	assert.Equal(t, "plan is ready", fc.exitSummary)
	assert.Equal(t, 1, fc.exitCalls)
}

func TestExitPlanModeTool_Execute_PropagatesError(t *testing.T) {
	fc := &fakePlanModeController{active: true, exitErr: assert.AnError}
	tool := NewExitPlanModeTool(fc)

	result, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-5",
		Name: "exit_plan_mode",
		Args: map[string]any{"summary": "fail"},
	})
	require.Error(t, err)
	assert.Nil(t, result)
	assert.ErrorIs(t, err, assert.AnError)
}
