package production

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCostTrackerRecordSubagent(t *testing.T) {
	tracker := NewCostTracker(nil)

	cost, err := tracker.RecordSubagent("task-1", "gpt-4o", 1000, 500)
	require.NoError(t, err)
	assert.Greater(t, cost, 0.0)

	// Record another call for the same task.
	cost2, err := tracker.RecordSubagent("task-1", "gpt-4o", 500, 200)
	require.NoError(t, err)
	assert.Greater(t, cost2, 0.0)

	summary := tracker.SubagentCosts["task-1"]
	assert.Equal(t, 2, summary.Calls)
	assert.Equal(t, 1500, summary.TokensIn)
	assert.Equal(t, 700, summary.TokensOut)
	assert.InDelta(t, cost+cost2, summary.Cost, 0.0001)
}

func TestCostTrackerRecordSubagentSeparateFromMain(t *testing.T) {
	tracker := NewCostTracker(nil)

	// Record a main session call.
	_, err := tracker.Record("gpt-4o", 1000, 500)
	require.NoError(t, err)

	// Record a subagent call.
	_, err = tracker.RecordSubagent("task-1", "gpt-4o", 2000, 1000)
	require.NoError(t, err)

	// Main and subagent costs should be tracked separately.
	assert.Equal(t, 1, tracker.Calls())
	assert.Equal(t, 1, tracker.SubagentCalls())

	mainTotal := tracker.Total()
	subTotal := tracker.SubagentTotal()
	assert.Greater(t, mainTotal, 0.0)
	assert.Greater(t, subTotal, 0.0)
	assert.InDelta(t, mainTotal+subTotal, mainTotal+subTotal, 0.0001)
	assert.NotEqual(t, mainTotal, subTotal, "main and subagent costs should differ (different token counts)")
}

func TestCostTrackerSubagentTotalAggregatesMultipleTasks(t *testing.T) {
	tracker := NewCostTracker(nil)

	_, err := tracker.RecordSubagent("task-1", "gpt-4o", 1000, 500)
	require.NoError(t, err)
	_, err = tracker.RecordSubagent("task-2", "gpt-4o", 2000, 1000)
	require.NoError(t, err)

	total := tracker.SubagentTotal()
	assert.Greater(t, total, 0.0)
	assert.Equal(t, 2, tracker.SubagentCalls())

	s1 := tracker.SubagentCosts["task-1"]
	s2 := tracker.SubagentCosts["task-2"]
	assert.InDelta(t, s1.Cost+s2.Cost, total, 0.0001)
}

func TestCostTrackerSubagentCallsZeroWhenEmpty(t *testing.T) {
	tracker := NewCostTracker(nil)
	assert.Equal(t, 0, tracker.SubagentCalls())
	assert.Equal(t, 0.0, tracker.SubagentTotal())
}

func TestCostTrackerRecordSubagentUnknownModelError(t *testing.T) {
	tracker := NewCostTracker(nil)
	_, err := tracker.RecordSubagent("task-1", "unknown-model", 100, 50)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no pricing tier")
}

func TestNewCostTrackerInitializesSubagentCosts(t *testing.T) {
	tracker := NewCostTracker(nil)
	assert.NotNil(t, tracker.SubagentCosts)
	assert.Empty(t, tracker.SubagentCosts)
}
