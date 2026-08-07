package core

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestPlanModeToolWrapper_AllowsReadWhenActive(t *testing.T) {
	var callCount atomic.Int32
	next := mockExecutor(&callCount)

	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	wrapped := NewPlanModeToolWrapper(ctrl)(next)

	result, err := wrapped(context.Background(), tools.ToolCall{Name: "read"})
	require.NoError(t, err)
	assert.Equal(t, int32(1), callCount.Load(), "next should be called for read tools")
	require.NotNil(t, result)
	assert.Equal(t, "ok", result.Output)
}

func TestPlanModeToolWrapper_AllowsGrepWhenActive(t *testing.T) {
	var callCount atomic.Int32
	next := mockExecutor(&callCount)

	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	wrapped := NewPlanModeToolWrapper(ctrl)(next)

	result, err := wrapped(context.Background(), tools.ToolCall{Name: "grep"})
	require.NoError(t, err)
	assert.Equal(t, int32(1), callCount.Load(), "next should be called for grep tools")
	require.NotNil(t, result)
	assert.Equal(t, "ok", result.Output)
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

	// A read tool with nil next returns nil, nil (no panic).
	result2, err2 := wrapped(context.Background(), tools.ToolCall{Name: "read"})
	assert.NoError(t, err2)
	assert.Nil(t, result2)
}

// planModeStubToolDef is a minimal ToolDefinition for the integration test.
type planModeStubToolDef struct {
	name string
}

func (s *planModeStubToolDef) Name() string        { return s.name }
func (s *planModeStubToolDef) Description() string { return "stub" }
func (s *planModeStubToolDef) Execute(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
	return &tools.ToolResult{Output: "executed"}, nil
}

func TestPlanModeToolWrapper_ThroughRegistry(t *testing.T) {
	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))

	inner := tools.NewDefaultToolRegistry()
	require.NoError(t, inner.Register(context.Background(), &planModeStubToolDef{name: "write"}))

	reg := tools.NewMiddlewareToolRegistry(inner, NewPlanModeToolWrapper(ctrl))

	def, err := reg.Get(context.Background(), "write")
	require.NoError(t, err)
	require.NotNil(t, def)

	result, err := def.Execute(context.Background(), tools.ToolCall{Name: "write"})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrPlanModeBlocked)
	require.NotNil(t, result)
	assert.Equal(t, true, result.Metadata["plan_mode_blocked"])
	assert.Contains(t, result.Output, "plan mode")
}
