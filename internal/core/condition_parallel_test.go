package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// ---------------------------------------------------------------------------
// ConditionEvaluator tests
// ---------------------------------------------------------------------------

func TestConditionEvaluator_TrueFires(t *testing.T) {
	m := NewDefaultSystemReminderManager()
	m.AddReminder(SystemReminder{
		ID:        "r1",
		Content:   "hello",
		Interval:  0, // one-time
		Evaluator: ConditionFunc(func(ctx context.Context) bool { return true }),
	})

	due := m.CheckAndCollect(context.Background())
	assert.Len(t, due, 1)
	assert.Equal(t, "hello", due[0])
}

func TestConditionEvaluator_FalseBlocks(t *testing.T) {
	m := NewDefaultSystemReminderManager()
	m.AddReminder(SystemReminder{
		ID:        "r1",
		Content:   "hello",
		Interval:  0, // one-time
		Evaluator: ConditionFunc(func(ctx context.Context) bool { return false }),
	})

	due := m.CheckAndCollect(context.Background())
	assert.Empty(t, due)

	// Should still be eligible next time since lastFired was not updated.
	due = m.CheckAndCollect(context.Background())
	assert.Empty(t, due)
}

func TestConditionEvaluator_FalseDoesNotResetLastFired(t *testing.T) {
	m := NewDefaultSystemReminderManager()
	count := 0
	m.AddReminder(SystemReminder{
		ID:       "r1",
		Content:  "hello",
		Interval: 0, // one-time
		Evaluator: ConditionFunc(func(ctx context.Context) bool {
			count++
			return count >= 3 // false, false, true
		}),
	})

	// First check: count=1, returns false, should not fire.
	due := m.CheckAndCollect(context.Background())
	assert.Empty(t, due)

	// Second check: count=2, returns false, should not fire.
	due = m.CheckAndCollect(context.Background())
	assert.Empty(t, due)

	// Third check: count=3, returns true, should fire.
	// This proves lastFired was not updated by the previous false evaluations.
	due = m.CheckAndCollect(context.Background())
	assert.Len(t, due, 1)
	assert.Equal(t, "hello", due[0])

	// Fourth check: already fired (one-time), should not fire again.
	due = m.CheckAndCollect(context.Background())
	assert.Empty(t, due)
}

func TestConditionEvaluator_NilUnchanged(t *testing.T) {
	m := NewDefaultSystemReminderManager()
	m.AddReminder(SystemReminder{
		ID:       "r1",
		Content:  "hello",
		Interval: 0, // one-time
		// Evaluator is nil — behavior unchanged (only Interval matters).
	})

	due := m.CheckAndCollect(context.Background())
	assert.Len(t, due, 1)

	// One-time reminder already fired — should not fire again.
	due = m.CheckAndCollect(context.Background())
	assert.Empty(t, due)
}

func TestConditionEvaluator_Stateful(t *testing.T) {
	m := NewDefaultSystemReminderManager()

	var mu sync.Mutex
	counter := 0
	m.AddReminder(SystemReminder{
		ID:       "r1",
		Content:  "hello",
		Interval: 0,
		Evaluator: ConditionFunc(func(ctx context.Context) bool {
			mu.Lock()
			defer mu.Unlock()
			counter++
			return counter >= 2
		}),
	})

	// First check: counter=1, returns false.
	due := m.CheckAndCollect(context.Background())
	assert.Empty(t, due)

	// Second check: counter=2, returns true.
	due = m.CheckAndCollect(context.Background())
	assert.Len(t, due, 1)
}

func TestConditionEvaluator_ConcurrentSafe(t *testing.T) {
	m := NewDefaultSystemReminderManager()
	m.AddReminder(SystemReminder{
		ID:        "r1",
		Content:   "hello",
		Interval:  10 * time.Millisecond,
		Evaluator: ConditionFunc(func(ctx context.Context) bool { return true }),
	})

	var wg sync.WaitGroup
	// Concurrent CheckAndCollect calls.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.CheckAndCollect(context.Background())
		}()
	}
	// Concurrent AddReminder calls.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(j int) {
			defer wg.Done()
			m.AddReminder(SystemReminder{
				ID:      fmt.Sprintf("concurrent_%d", j),
				Content: "concurrent",
			})
		}(i)
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// executeToolsParallel context-aware cancellation tests
// ---------------------------------------------------------------------------

func TestExecuteToolsParallel_Canceled_QuickReturn(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	toolSrv := mock.NewMockToolServer()
	err := toolSrv.Register(context.Background(), &testToolDef{
		name: "blocking",
		handler: func(ctx context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	require.NoError(t, err)

	calls := []llm.ToolCall{
		{ID: "1", Name: "blocking"},
		{ID: "2", Name: "blocking"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	results, err := executeToolsParallel(ctx, toolSrv, calls, nil)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
	assert.Less(t, elapsed, 1*time.Second, "should return quickly after cancel")
	require.Len(t, results, 2)

	for _, r := range results {
		assert.Error(t, r.Err)
	}
}

func TestExecuteToolsParallel_PartialResults(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	toolSrv := mock.NewMockToolServer()

	// Fast tool that completes immediately.
	err := toolSrv.Register(context.Background(), &testToolDef{
		name: "fast",
		handler: func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
			return &tools.ToolResult{Output: "fast_result"}, nil
		},
	})
	require.NoError(t, err)

	// Blocking tool that waits for ctx cancellation.
	err = toolSrv.Register(context.Background(), &testToolDef{
		name: "blocking",
		handler: func(ctx context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	require.NoError(t, err)

	calls := []llm.ToolCall{
		{ID: "1", Name: "fast"},
		{ID: "2", Name: "blocking"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	results, err := executeToolsParallel(ctx, toolSrv, calls, nil)
	require.Error(t, err)
	require.Len(t, results, 2)

	// Fast tool should have completed successfully.
	assert.Equal(t, "1", results[0].ID)
	assert.Equal(t, "fast_result", results[0].Output)
	assert.NoError(t, results[0].Err)

	// Blocking tool should be canceled.
	assert.Equal(t, "2", results[1].ID)
	assert.Error(t, results[1].Err)
	assert.Empty(t, results[1].Output)
}

func TestExecuteToolsParallel_NoLeak(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	toolSrv := mock.NewMockToolServer()
	err := toolSrv.Register(context.Background(), &testToolDef{
		name: "blocking",
		handler: func(ctx context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	require.NoError(t, err)

	calls := []llm.ToolCall{
		{ID: "1", Name: "blocking"},
		{ID: "2", Name: "blocking"},
		{ID: "3", Name: "blocking"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, _ = executeToolsParallel(ctx, toolSrv, calls, nil)
	// verify.AssertNoGoroutineLeak will check for leaks when the test exits.
}

func TestExecuteToolsParallel_Normal_NoError(t *testing.T) {
	toolSrv := mock.NewMockToolServer()
	err := toolSrv.Register(context.Background(), &testToolDef{
		name: "simple",
		handler: func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
			return &tools.ToolResult{Output: "ok"}, nil
		},
	})
	require.NoError(t, err)

	calls := []llm.ToolCall{{ID: "1", Name: "simple"}}
	results, err := executeToolsParallel(context.Background(), toolSrv, calls, nil)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "ok", results[0].Output)
	assert.NoError(t, results[0].Err)
}

func TestExecuteToolsParallel_EmptyCalls(t *testing.T) {
	toolSrv := mock.NewMockToolServer()
	results, err := executeToolsParallel(context.Background(), toolSrv, nil, nil)
	assert.Nil(t, results)
	assert.NoError(t, err)
}
