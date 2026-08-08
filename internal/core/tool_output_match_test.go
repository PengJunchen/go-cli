package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// TestToolOutputMatchByID verifies that matchToolResultsByID correctly
// associates results with their originating calls by ToolCallID, even when
// results arrive in a different order than the calls.
func TestToolOutputMatchByID(t *testing.T) {
	calls := []llm.ToolCall{
		{ID: "call-1", Name: "tool_a"},
		{ID: "call-2", Name: "tool_b"},
		{ID: "call-3", Name: "tool_c"},
	}

	// Results arrive in reverse order (simulating parallel out-of-order
	// completion).
	shuffled := []ParallelToolResult{
		{ID: "call-3", Name: "tool_c", Output: "result_c"},
		{ID: "call-1", Name: "tool_a", Output: "result_a"},
		{ID: "call-2", Name: "tool_b", Output: "result_b"},
	}

	matched, err := matchToolResultsByID(calls, shuffled)
	require.NoError(t, err)
	require.Len(t, matched, 3)

	// After matching, results are in call order.
	assert.Equal(t, "call-1", matched[0].ID)
	assert.Equal(t, "result_a", matched[0].Output)
	assert.Equal(t, "call-2", matched[1].ID)
	assert.Equal(t, "result_b", matched[1].Output)
	assert.Equal(t, "call-3", matched[2].ID)
	assert.Equal(t, "result_c", matched[2].Output)
}

// TestToolOutputParallelNoMismatch verifies that executing tool calls in
// parallel and then matching by ToolCallID produces no mismatched results,
// regardless of completion order.
func TestToolOutputParallelNoMismatch(t *testing.T) {
	toolSrv := mock.NewMockToolServer()

	// Register tools with varying delays so completion order is
	// non-deterministic.
	for _, name := range []string{"slow", "fast", "medium"} {
		name := name
		delay := 50 * time.Millisecond
		switch name {
		case "fast":
			delay = 5 * time.Millisecond
		case "medium":
			delay = 25 * time.Millisecond
		}
		err := toolSrv.Register(context.Background(), &testToolDef{
			name: name,
			handler: func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
				time.Sleep(delay)
				return &tools.ToolResult{Output: name + "_out"}, nil
			},
		})
		require.NoError(t, err)
	}

	calls := []llm.ToolCall{
		{ID: "id-slow", Name: "slow"},
		{ID: "id-fast", Name: "fast"},
		{ID: "id-medium", Name: "medium"},
	}

	results, execErr := executeToolsParallel(context.Background(), toolSrv, calls, nil)
	require.NoError(t, execErr)

	matched, err := matchToolResultsByID(calls, results)
	require.NoError(t, err)
	require.Len(t, matched, 3)

	// Every result must be matched to the correct call by ID and output.
	for i, tc := range calls {
		assert.Equal(t, tc.ID, matched[i].ID, "result at index %d should have ID %s", i, tc.ID)
		assert.Equal(t, tc.Name, matched[i].Name)
		assert.Equal(t, tc.Name+"_out", matched[i].Output)
		assert.NoError(t, matched[i].Err)
	}
}

// TestToolOutputUnknownIDReturnsError verifies that matchToolResultsByID
// returns an error when a result's ToolCallID is not among the input calls.
func TestToolOutputUnknownIDReturnsError(t *testing.T) {
	calls := []llm.ToolCall{
		{ID: "call-1", Name: "tool_a"},
		{ID: "call-2", Name: "tool_b"},
	}

	results := []ParallelToolResult{
		{ID: "call-1", Name: "tool_a", Output: "ok"},
		{ID: "unknown-id", Name: "tool_b", Output: "mismatch"},
	}

	matched, err := matchToolResultsByID(calls, results)
	require.Error(t, err)
	assert.ErrorIs(t, err, errUnknownToolCallID)
	assert.Nil(t, matched)
}

// TestToolOutputSingleCallWorks verifies that matchToolResultsByID handles a
// single tool call correctly.
func TestToolOutputSingleCallWorks(t *testing.T) {
	calls := []llm.ToolCall{
		{ID: "only-call", Name: "solo"},
	}
	results := []ParallelToolResult{
		{ID: "only-call", Name: "solo", Output: "solo_result"},
	}

	matched, err := matchToolResultsByID(calls, results)
	require.NoError(t, err)
	require.Len(t, matched, 1)
	assert.Equal(t, "only-call", matched[0].ID)
	assert.Equal(t, "solo_result", matched[0].Output)
}

// TestToolOutputMatchByIDConcurrent verifies matchToolResultsByID is safe
// under concurrent invocation with -race.
func TestToolOutputMatchByIDConcurrent(t *testing.T) {
	calls := []llm.ToolCall{
		{ID: "a", Name: "ta"},
		{ID: "b", Name: "tb"},
		{ID: "c", Name: "tc"},
	}
	results := []ParallelToolResult{
		{ID: "c", Name: "tc", Output: "c"},
		{ID: "a", Name: "ta", Output: "a"},
		{ID: "b", Name: "tb", Output: "b"},
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			matched, err := matchToolResultsByID(calls, results)
			assert.NoError(t, err)
			assert.Len(t, matched, 3)
		}()
	}
	wg.Wait()
}
