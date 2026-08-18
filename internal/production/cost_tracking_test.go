package production

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestCostTrackerCalculateCost(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	tr := NewCostTracker(nil) // uses DefaultCostTiers

	// gpt-4o: $0.0025/1K input, $0.01/1K output
	// 1000 input + 500 output => 0.0025 + 0.005 = 0.0075
	cost, err := tr.CalculateCost("gpt-4o", 1000, 500)
	require.NoError(t, err)
	assert.InDelta(t, 0.0075, cost, 1e-9)
}

func TestCostTrackerCalculateCostUnknownModel(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	tr := NewCostTracker(nil)

	_, err := tr.CalculateCost("unknown-model", 100, 100)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown-model")
}

func TestCostTrackerRecordAccumulates(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	tr := NewCostTracker(nil)

	c1, err := tr.Record("gpt-4o", 1000, 500)
	require.NoError(t, err)
	c2, err := tr.Record("gpt-4o", 2000, 0)
	require.NoError(t, err)

	assert.InDelta(t, 0.0075, c1, 1e-9)
	assert.InDelta(t, 0.005, c2, 1e-9)
	assert.InDelta(t, 0.0125, tr.Total(), 1e-9)
	assert.Equal(t, 2, tr.Calls())
}

func TestCostTrackerRecordUnknownModel(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	tr := NewCostTracker(nil)

	_, err := tr.Record("nope", 1, 1)
	require.Error(t, err)
	assert.Equal(t, 0, tr.Calls())
	assert.InDelta(t, 0.0, tr.Total(), 1e-9)
}

func TestCostTrackerCustomTiers(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	tiers := []CostTier{
		{Model: "custom", InputPer1K: 1.0, OutputPer1K: 2.0},
	}
	tr := NewCostTracker(tiers)

	cost, err := tr.CalculateCost("custom", 1000, 500)
	require.NoError(t, err)
	assert.InDelta(t, 1.0+1.0, cost, 1e-9)

	// Default tiers should not be present.
	_, err = tr.CalculateCost("gpt-4o", 10, 10)
	require.Error(t, err)
}

func TestCostTrackerDefaultTiersPopulated(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	tr := NewCostTracker(nil)

	for _, m := range []string{"gpt-4o", "gpt-4o-mini", "claude-sonnet-4", "claude-opus-4"} {
		_, err := tr.CalculateCost(m, 100, 100)
		require.NoError(t, err, "model %s should have a tier", m)
	}
}

func TestCostTrackerConcurrentRecord(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	tr := NewCostTracker(nil)

	var wg sync.WaitGroup
	const goroutines = 8
	const per = 50
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				_, _ = tr.Record("gpt-4o-mini", 100, 100) //nolint:errcheck
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, goroutines*per, tr.Calls())
}

func TestCostTrackerInterfaceCompliance(t *testing.T) {
	var _ CostCalculator = (*CostTracker)(nil)
	var _ CostCalculator = NewCostTracker(nil)
}

func TestBudgetExceededErrorMessage(t *testing.T) {
	err := &BudgetExceededError{Spent: 1.5, Budget: 1.0}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "budget exceeded")
	assert.Contains(t, err.Error(), "1.5000")
	assert.Contains(t, err.Error(), "1.0000")
}

// TestCostTrackerBudgetZeroMeansNoLimit verifies that the default zero budget
// disables the limit: Record never returns a budget error and CheckBudget is
// always nil.
func TestCostTrackerBudgetZeroMeansNoLimit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	tracker := NewCostTracker(nil)
	for i := 0; i < 100; i++ {
		_, err := tracker.Record("gpt-4o-mini", 1000, 1000)
		require.NoError(t, err, "record %d must not error with no budget", i)
	}
	assert.NoError(t, tracker.CheckBudget(), "CheckBudget must be nil with no budget")
}

// TestCostTrackerCheckBudgetExceeded verifies that CheckBudget returns a
// *BudgetExceededError once the running total crosses the configured limit, and
// that Record surfaces the same error.
func TestCostTrackerCheckBudgetExceeded(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	tracker := NewCostTracker(nil)
	tracker.SetBudgetLimit(0.01) // $0.01 cap

	// A small call stays under budget.
	_, err := tracker.Record("gpt-4o-mini", 100, 10)
	require.NoError(t, err)
	assert.NoError(t, tracker.CheckBudget())

	// Keep recording until the budget is exceeded.
	var exceeded bool
	for i := 0; i < 50; i++ {
		_, recErr := tracker.Record("gpt-4o-mini", 1000, 1000)
		if recErr != nil {
			var bee *BudgetExceededError
			require.ErrorAs(t, recErr, &bee, "error must be *BudgetExceededError")
			assert.Equal(t, 0.01, bee.Budget)
			assert.Greater(t, bee.Spent, bee.Budget, "spent must exceed budget")
			exceeded = true
			break
		}
	}
	require.True(t, exceeded, "expected the budget to be exceeded")

	// CheckBudget must now also report the overrun.
	var bee *BudgetExceededError
	require.ErrorAs(t, tracker.CheckBudget(), &bee)
	assert.Greater(t, bee.Spent, bee.Budget)
}

// TestCostTrackerBudgetCallback verifies the BudgetCallback fires with the
// current spent total and configured budget when Record pushes the total past
// the limit.
func TestCostTrackerBudgetCallback(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	tracker := NewCostTracker(nil)
	tracker.SetBudgetLimit(0.01)

	var called bool
	var cbSpent, cbBudget float64
	tracker.SetBudgetCallback(func(spent, budget float64) {
		called = true
		cbSpent = spent
		cbBudget = budget
	})

	for i := 0; i < 50; i++ {
		_, err := tracker.Record("gpt-4o-mini", 1000, 1000)
		if err != nil {
			break
		}
	}
	require.True(t, called, "budget callback must fire when budget is exceeded")
	assert.Equal(t, 0.01, cbBudget)
	assert.Greater(t, cbSpent, cbBudget, "callback spent must exceed budget")
}

// TestCostTrackerBudgetCallbackNotFiredUnderLimit verifies the callback is not
// invoked while spending remains within budget.
func TestCostTrackerBudgetCallbackNotFiredUnderLimit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	tracker := NewCostTracker(nil)
	tracker.SetBudgetLimit(1.0) // generous cap

	var called bool
	tracker.SetBudgetCallback(func(spent, budget float64) {
		called = true
	})

	_, err := tracker.Record("gpt-4o-mini", 100, 10)
	require.NoError(t, err)
	assert.False(t, called, "callback must not fire while under budget")
	assert.NoError(t, tracker.CheckBudget())
}
