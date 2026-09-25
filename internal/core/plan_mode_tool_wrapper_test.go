package core

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// mockExecutor returns a next executor function that increments callCount
// and returns a simple ToolResult.
func mockExecutor(callCount *atomic.Int32) func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	return func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		callCount.Add(1)
		return &tools.ToolResult{Output: "ok"}, nil
	}
}

func TestPlanModeToolWrapper_BlocksWriteWhenActive(t *testing.T) {
	var callCount atomic.Int32
	next := mockExecutor(&callCount)

	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	wrapped := NewPlanModeToolWrapper(ctrl)(next)

	result, err := wrapped(context.Background(), tools.ToolCall{Name: "write"})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrPlanModeBlocked)
	assert.Equal(t, int32(0), callCount.Load(), "next should NOT be called when write is blocked")
	require.NotNil(t, result)
	assert.Equal(t, true, result.Metadata["plan_mode_blocked"])
}

func TestPlanModeToolWrapper_BlocksEditWhenActive(t *testing.T) {
	var callCount atomic.Int32
	next := mockExecutor(&callCount)

	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	wrapped := NewPlanModeToolWrapper(ctrl)(next)

	result, err := wrapped(context.Background(), tools.ToolCall{Name: "edit"})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrPlanModeBlocked)
	assert.Equal(t, int32(0), callCount.Load(), "next should NOT be called when edit is blocked")
	require.NotNil(t, result)
	assert.Equal(t, true, result.Metadata["plan_mode_blocked"])
}

func TestPlanModeToolWrapper_BlocksBashWhenActive(t *testing.T) {
	var callCount atomic.Int32
	next := mockExecutor(&callCount)

	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	wrapped := NewPlanModeToolWrapper(ctrl)(next)

	result, err := wrapped(context.Background(), tools.ToolCall{Name: "bash"})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrPlanModeBlocked)
	assert.Equal(t, int32(0), callCount.Load(), "next should NOT be called when bash is blocked")
	require.NotNil(t, result)
	assert.Equal(t, true, result.Metadata["plan_mode_blocked"])
}

func TestPlanModeToolWrapper_BlocksUnverifiedReadWhenActive(t *testing.T) {
	var callCount atomic.Int32
	next := mockExecutor(&callCount)

	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	wrapped := NewPlanModeToolWrapper(ctrl)(next)

	result, err := wrapped(context.Background(), tools.ToolCall{Name: "read"})
	require.ErrorIs(t, err, ErrPlanModeBlocked)
	assert.Equal(t, int32(0), callCount.Load(), "unverified reads must not bypass prepared effect policy")
	require.NotNil(t, result)
	assert.Equal(t, true, result.Metadata["plan_mode_blocked"])
}

func TestPlanModeToolWrapper_BlocksUnverifiedGrepWhenActive(t *testing.T) {
	var callCount atomic.Int32
	next := mockExecutor(&callCount)

	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	wrapped := NewPlanModeToolWrapper(ctrl)(next)

	result, err := wrapped(context.Background(), tools.ToolCall{Name: "grep"})
	require.ErrorIs(t, err, ErrPlanModeBlocked)
	assert.Equal(t, int32(0), callCount.Load(), "unverified search tools must not bypass prepared effect policy")
	require.NotNil(t, result)
	assert.Equal(t, true, result.Metadata["plan_mode_blocked"])
}

func TestPlanModeToolWrapper_PassthroughWhenInactive(t *testing.T) {
	var callCount atomic.Int32
	next := mockExecutor(&callCount)

	ctrl := NewDefaultPlanModeController()
	// plan mode is NOT active
	wrapped := NewPlanModeToolWrapper(ctrl)(next)

	result, err := wrapped(context.Background(), tools.ToolCall{Name: "write"})
	require.NoError(t, err)
	assert.Equal(t, int32(1), callCount.Load(), "next should be called when plan mode inactive")
	require.NotNil(t, result)
	assert.Equal(t, "ok", result.Output)
}

func TestPlanModeToolWrapper_NilController(t *testing.T) {
	var callCount atomic.Int32
	next := mockExecutor(&callCount)

	// nil controller = zero-config fallback
	wrapped := NewPlanModeToolWrapper(nil)(next)

	result, err := wrapped(context.Background(), tools.ToolCall{Name: "write"})
	require.NoError(t, err)
	assert.Equal(t, int32(1), callCount.Load(), "next should be called when controller is nil")
	require.NotNil(t, result)
	assert.Equal(t, "ok", result.Output)
}

func TestPlanModeToolWrapper_ReturnsReadableError(t *testing.T) {
	var callCount atomic.Int32
	next := mockExecutor(&callCount)

	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	wrapped := NewPlanModeToolWrapper(ctrl)(next)

	result, err := wrapped(context.Background(), tools.ToolCall{Name: "write"})
	require.Error(t, err)
	require.NotNil(t, result)
	// Output should contain "plan mode" and the tool name.
	assert.Contains(t, result.Output, "plan mode")
	assert.Contains(t, result.Output, "write")
	// Metadata should mark the block.
	assert.Equal(t, true, result.Metadata["plan_mode_blocked"])
	// errors.Is should work.
	require.ErrorIs(t, err, ErrPlanModeBlocked)
}

func TestPlanModeToolWrapper_NilNext(t *testing.T) {
	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))

	// next=nil should not panic.
	wrapped := NewPlanModeToolWrapper(ctrl)(nil)

	// A blocked tool still returns a result + error (doesn't need next).
	result, err := wrapped(context.Background(), tools.ToolCall{Name: "write"})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrPlanModeBlocked)
	require.NotNil(t, result)
	assert.Equal(t, true, result.Metadata["plan_mode_blocked"])

	// An unverified read is blocked before a nil next executor could run.
	result2, err2 := wrapped(context.Background(), tools.ToolCall{Name: "read"})
	assert.ErrorIs(t, err2, ErrPlanModeBlocked)
	assert.NotNil(t, result2)
}

// planModeStubToolDef is a minimal ToolDefinition for the integration test.
type planModeStubToolDef struct {
	name       string
	executions int
}

func (s *planModeStubToolDef) Name() string        { return s.name }
func (s *planModeStubToolDef) Description() string { return "stub" }

func (s *planModeStubToolDef) Execute(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
	s.executions++
	return &tools.ToolResult{Output: "executed"}, nil
}

// TestPlanModePreparedPolicy_BlocksMutatorThroughRegistry uses the prepared
// policy boundary and the production controller/fence methods. The legacy
// executor wrapper remains covered by its direct tests and is rejected by the
// middleware-registry fail-closed tests in internal/tools. The direct tests
// retain its readable denial result; the prepared policy rejects before a
// result is produced and therefore asserts zero real executions instead.
func TestPlanModePreparedPolicy_BlocksMutatorThroughRegistry(t *testing.T) {
	inner := tools.NewDefaultToolRegistry()
	tool := &planModeStubToolDef{name: "custom_write"}
	require.NoError(t, inner.Register(context.Background(), tool))

	reg := tools.NewMiddlewareToolRegistry(inner)
	ctrl := NewDefaultPlanModeController(reg.DispatchFence())
	require.Same(t, reg.DispatchFence(), ctrl.DispatchFence(), "Plan Mode and the prepared registry must share one fence")
	require.NoError(t, reg.SetPreparedToolStages(tools.PreparedToolStages{}))
	require.NoError(t, reg.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		decision := tools.PreparedPolicyDecision{Generations: request.Generations}
		if request.Fence != ctrl.DispatchFence() {
			return decision, tools.ErrDispatchFenceUnavailable
		}
		if request.Fence.Snapshot() != request.Generations {
			return decision, tools.ErrDispatchFenceChanged
		}
		if ctrl.ShouldBlockWrite(request.Call.ToolCall().Name) {
			return decision, ErrPlanModeBlocked
		}
		decision.Allowed = true
		return decision, nil
	}))

	def, err := reg.Get(context.Background(), "custom_write")
	require.NoError(t, err)
	require.NotNil(t, def)

	result, err := def.Execute(context.Background(), tools.ToolCall{Name: "custom_write"})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "executed", result.Output)
	assert.Equal(t, 1, tool.executions, "an inactive Plan Mode allows the real prepared executor")

	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	result, err = def.Execute(context.Background(), tools.ToolCall{Name: "custom_write"})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrPlanModeBlocked)
	assert.Nil(t, result, "prepared policy denial occurs before the executor returns a result")
	assert.Equal(t, 1, tool.executions, "active Plan Mode must block the write before real execution")
}
