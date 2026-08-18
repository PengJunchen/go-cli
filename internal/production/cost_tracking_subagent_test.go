package production

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// findSubagentRecord returns the record for taskID from a snapshot, or false.
func findSubagentRecord(records []SubagentCostRecord, taskID string) (SubagentCostRecord, bool) {
	for _, r := range records {
		if r.TaskID == taskID {
			return r, true
		}
	}
	return SubagentCostRecord{}, false
}

func TestCostTrackerRecordSubagent(t *testing.T) {
	tracker := NewCostTracker(nil)

	cost, err := tracker.RecordSubagent("task-1", "gpt-4o", 1000, 500)
	require.NoError(t, err)
	assert.Greater(t, cost, 0.0)

	// Record another call for the same task.
	cost2, err := tracker.RecordSubagent("task-1", "gpt-4o", 500, 200)
	require.NoError(t, err)
	assert.Greater(t, cost2, 0.0)

	summary, ok := findSubagentRecord(tracker.SubagentCostSnapshot(), "task-1")
	require.True(t, ok)
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

	records := tracker.SubagentCostSnapshot()
	s1, ok1 := findSubagentRecord(records, "task-1")
	s2, ok2 := findSubagentRecord(records, "task-2")
	require.True(t, ok1)
	require.True(t, ok2)
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
	assert.Empty(t, tracker.SubagentCostSnapshot())
}

// TestConcurrentRecordAndSnapshotRace runs many concurrent RecordSubagent and
// SubagentCostSnapshot goroutines. Run with -race; no data race should be
// reported.
func TestConcurrentRecordAndSnapshotRace(t *testing.T) {
	tracker := NewCostTracker(nil)

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n * 2)

	// Writers: RecordSubagent across a small set of task IDs to encourage
	// contention on shared map keys.
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			taskID := fmt.Sprintf("task-%d", i%10)
			_, _ = tracker.RecordSubagent(taskID, "gpt-4o", 10, 5)
		}(i)
	}
	// Readers: SubagentCostSnapshot concurrently with the writers.
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = tracker.SubagentCostSnapshot()
		}()
	}
	wg.Wait()

	// After all goroutines complete, the snapshot should account for every
	// recorded call.
	records := tracker.SubagentCostSnapshot()
	totalCalls := 0
	for _, r := range records {
		totalCalls += r.Calls
	}
	assert.Equal(t, n, totalCalls, "all recorded calls should be present in the snapshot")
}

// TestSnapshotIsDefensiveCopy verifies that mutating the returned slice or its
// elements does not affect the tracker's internal state.
func TestSnapshotIsDefensiveCopy(t *testing.T) {
	tracker := NewCostTracker(nil)
	_, err := tracker.RecordSubagent("task-1", "gpt-4o", 1000, 500)
	require.NoError(t, err)

	snap := tracker.SubagentCostSnapshot()
	require.Len(t, snap, 1)

	// Mutate the snapshot elements and append to the slice.
	snap[0].Cost = 9999.99
	snap[0].Calls = 9999
	snap[0].TaskID = "tampered"
	snap = append(snap, SubagentCostRecord{TaskID: "extra"})

	// The tracker's data must be unaffected.
	fresh := tracker.SubagentCostSnapshot()
	require.Len(t, fresh, 1)
	rec, ok := findSubagentRecord(fresh, "task-1")
	require.True(t, ok)
	assert.Equal(t, 1, rec.Calls)
	assert.NotEqual(t, 9999.99, rec.Cost)
	assert.NotEqual(t, "tampered", fresh[0].TaskID)
}

// TestCorrectDataAfterMultipleRecords records 5 sub-agent entries and verifies
// the snapshot contains the correct data for each.
func TestCorrectDataAfterMultipleRecords(t *testing.T) {
	tracker := NewCostTracker(nil)

	costs := make(map[string]float64)
	for i := 0; i < 5; i++ {
		taskID := fmt.Sprintf("task-%d", i)
		cost, err := tracker.RecordSubagent(taskID, "gpt-4o", 100*(i+1), 50*(i+1))
		require.NoError(t, err)
		costs[taskID] = cost
	}

	records := tracker.SubagentCostSnapshot()
	assert.Len(t, records, 5)

	for _, r := range records {
		expected, ok := costs[r.TaskID]
		require.True(t, ok, "unexpected task %q in snapshot", r.TaskID)
		assert.InDelta(t, expected, r.Cost, 0.0001)
		assert.Equal(t, 1, r.Calls)
	}
}
