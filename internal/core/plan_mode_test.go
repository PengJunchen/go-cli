package core

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultPlanModeController_SatisfiesInterface(t *testing.T) {
	var _ PlanModeController = (*DefaultPlanModeController)(nil)
}

func TestDefaultPlanModeController_IsActive_FalseByDefault(t *testing.T) {
	c := NewDefaultPlanModeController()
	assert.False(t, c.IsActive())
}

func TestDefaultPlanModeController_Enter_ActivatesPlanMode(t *testing.T) {
	c := NewDefaultPlanModeController()
	err := c.Enter(context.Background(), "user requested planning")
	require.NoError(t, err)
	assert.True(t, c.IsActive())
	assert.Equal(t, "user requested planning", c.Reason())
}

func TestDefaultPlanModeController_Exit_DeactivatesPlanMode(t *testing.T) {
	c := NewDefaultPlanModeController()
	require.NoError(t, c.Enter(context.Background(), "planning"))
	require.True(t, c.IsActive())

	err := c.Exit(context.Background(), "plan complete")
	require.NoError(t, err)
	assert.False(t, c.IsActive())
	assert.Empty(t, c.Reason())
}

func TestDefaultPlanModeController_ShouldBlockWrite_WhenInactive(t *testing.T) {
	c := NewDefaultPlanModeController()
	for _, name := range []string{"write", "edit", "bash", "mutation", "read", "ls"} {
		assert.False(t, c.ShouldBlockWrite(name), "should not block %q when inactive", name)
	}
}

func TestDefaultPlanModeController_ShouldBlockWrite_WhenActive(t *testing.T) {
	c := NewDefaultPlanModeController()
	require.NoError(t, c.Enter(context.Background(), "planning"))

	writeTools := []string{"write", "edit", "bash", "mutation"}
	for _, name := range writeTools {
		assert.True(t, c.ShouldBlockWrite(name), "should block %q when active", name)
	}
}

func TestDefaultPlanModeController_ShouldNotBlockReadTools_WhenActive(t *testing.T) {
	c := NewDefaultPlanModeController()
	require.NoError(t, c.Enter(context.Background(), "planning"))

	readTools := []string{"read", "ls", "find", "grep", "search"}
	for _, name := range readTools {
		assert.False(t, c.ShouldBlockWrite(name), "should not block read tool %q when active", name)
	}
}

func TestDefaultPlanModeController_ShouldBlockWrite_UnknownTool(t *testing.T) {
	c := NewDefaultPlanModeController()
	require.NoError(t, c.Enter(context.Background(), "planning"))
	assert.False(t, c.ShouldBlockWrite("nonexistent_tool"))
}

func TestDefaultPlanModeController_ConcurrentAccess(t *testing.T) {
	c := NewDefaultPlanModeController()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Enter(context.Background(), "concurrent") //nolint:errcheck
			_ = c.IsActive()                                //nolint:errcheck
			_ = c.ShouldBlockWrite("write")                 //nolint:errcheck
			_ = c.Exit(context.Background(), "done")        //nolint:errcheck
		}()
	}
	wg.Wait()
	assert.False(t, c.IsActive())
}
