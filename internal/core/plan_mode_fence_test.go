package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestPlanModeTransitionsPublishSharedFenceGeneration(t *testing.T) {
	fence := tools.NewDispatchFence()
	controller := NewDefaultPlanModeController(fence)
	require.Same(t, fence, controller.DispatchFence())

	initial := fence.Snapshot()
	require.NoError(t, controller.Enter(context.Background(), "planning"))
	entered := fence.Snapshot()
	assert.Equal(t, initial.Plan+1, entered.Plan)
	assert.Equal(t, initial.Registry, entered.Registry)
	assert.Equal(t, initial.Approval, entered.Approval)
	assert.True(t, controller.IsActive())
	assert.Equal(t, "planning", controller.Reason())

	require.NoError(t, controller.Exit(context.Background(), "finished"))
	exited := fence.Snapshot()
	assert.Equal(t, entered.Plan+1, exited.Plan)
	assert.Equal(t, entered.Registry, exited.Registry)
	assert.Equal(t, entered.Approval, exited.Approval)
	assert.False(t, controller.IsActive())
	assert.Empty(t, controller.Reason())
}

func TestPlanModeTransitionInvalidatesPreparedGenerationSnapshot(t *testing.T) {
	fence := tools.NewDispatchFence()
	controller := NewDefaultPlanModeController(fence)
	prepared := fence.Snapshot()

	require.NoError(t, controller.Enter(context.Background(), "planning"))

	assert.NotEqual(t, prepared, fence.Snapshot(), "entering Plan Mode must stale an outstanding prepared policy snapshot")
}
