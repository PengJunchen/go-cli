package core

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// testToolDef is a custom ToolDefinition that delegates to a handler function,
// allowing tests to control the tool's behavior (sleep, error, etc.).
type testToolDef struct {
	name        string
	description string
	handler     func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error)
}

func (d *testToolDef) Name() string        { return d.name }
func (d *testToolDef) Description() string { return d.description }
func (d *testToolDef) Execute(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	return d.handler(ctx, call)
}

var _ tools.ToolDefinition = (*testToolDef)(nil)

func TestExecuteToolsParallel(t *testing.T) {
	toolSrv := mock.NewMockToolServer()

	// Register tools that sleep briefly to verify concurrency.
	for _, name := range []string{"tool_a", "tool_b", "tool_c"} {
		name := name
		err := toolSrv.Register(context.Background(), &testToolDef{
			name: name,
			handler: func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
				time.Sleep(50 * time.Millisecond)
				return &tools.ToolResult{Output: name + "_result"}, nil
			},
		})
		require.NoError(t, err)
	}

	calls := []llm.ToolCall{
		{ID: "1", Name: "tool_a"},
		{ID: "2", Name: "tool_b"},
		{ID: "3", Name: "tool_c"},
	}

	start := time.Now()
	results := executeToolsParallel(context.Background(), toolSrv, calls)
	elapsed := time.Since(start)

	require.Len(t, results, 3)

	// Results should be in input order.
	assert.Equal(t, "1", results[0].ID)
	assert.Equal(t, "tool_a", results[0].Name)
	assert.Equal(t, "tool_a_result", results[0].Output)
	assert.NoError(t, results[0].Err)

	assert.Equal(t, "2", results[1].ID)
	assert.Equal(t, "tool_b", results[1].Name)
	assert.Equal(t, "tool_b_result", results[1].Output)
	assert.NoError(t, results[1].Err)

	assert.Equal(t, "3", results[2].ID)
	assert.Equal(t, "tool_c", results[2].Name)
	assert.Equal(t, "tool_c_result", results[2].Output)
	assert.NoError(t, results[2].Err)

	// If sequential, total would be >= 150ms. Parallel should be ~50ms.
	assert.Less(t, elapsed, 120*time.Millisecond,
		"parallel execution should be faster than sequential")
}

func TestExecuteToolsParallelEmpty(t *testing.T) {
	toolSrv := mock.NewMockToolServer()
	results := executeToolsParallel(context.Background(), toolSrv, nil)
	assert.Empty(t, results)
}

func TestExecuteToolsParallelWithError(t *testing.T) {
	toolSrv := mock.NewMockToolServer()

	err := toolSrv.Register(context.Background(), &testToolDef{
		name: "good",
		handler: func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
			return &tools.ToolResult{Output: "ok"}, nil
		},
	})
	require.NoError(t, err)
	// "bad" is intentionally not registered.

	calls := []llm.ToolCall{
		{ID: "1", Name: "good"},
		{ID: "2", Name: "bad"},
	}

	results := executeToolsParallel(context.Background(), toolSrv, calls)
	require.Len(t, results, 2)

	assert.NoError(t, results[0].Err)
	assert.Equal(t, "ok", results[0].Output)

	assert.Error(t, results[1].Err)
	assert.Empty(t, results[1].Output)
}

func TestExecuteToolsParallelSingleCall(t *testing.T) {
	toolSrv := mock.NewMockToolServer()

	err := toolSrv.Register(context.Background(), &testToolDef{
		name: "only",
		handler: func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
			return &tools.ToolResult{Output: "single"}, nil
		},
	})
	require.NoError(t, err)

	calls := []llm.ToolCall{{ID: "1", Name: "only"}}
	results := executeToolsParallel(context.Background(), toolSrv, calls)

	require.Len(t, results, 1)
	assert.Equal(t, "single", results[0].Output)
	assert.NoError(t, results[0].Err)
}

func TestExecutionModeParallelLoop(t *testing.T) {
	toolSrv := mock.NewMockToolServer()

	var mu sync.Mutex
	executionTimes := make([]time.Duration, 0, 2)

	for _, name := range []string{"t1", "t2"} {
		name := name
		err := toolSrv.Register(context.Background(), &testToolDef{
			name: name,
			handler: func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
				start := time.Now()
				time.Sleep(50 * time.Millisecond)
				mu.Lock()
				executionTimes = append(executionTimes, time.Since(start))
				mu.Unlock()
				return &tools.ToolResult{Output: name + "_out"}, nil
			},
		})
		require.NoError(t, err)
	}

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"T", "parallel-loop",
		mock.ConversationTurn{
			AssistantContent: "running tools",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "c1", Name: "t1"},
				{ID: "c2", Name: "t2"},
			},
		},
		mock.ConversationTurn{AssistantContent: "done"},
	))

	loop := NewLoopAgent(
		WithLLM(model),
		WithTools(toolSrv),
		WithExecutionMode(ExecutionModeParallel),
	)

	events, err := loop.Run(context.Background(), Submission{Content: "run"})
	require.NoError(t, err)

	// Should have 2 tool_call events and 2 tool_result events.
	toolCalls := findEvents(events, "tool_call")
	assert.Len(t, toolCalls, 2)

	toolResults := findEvents(events, "tool_result")
	assert.Len(t, toolResults, 2)
}

func TestExecutionModeSequentialDefault(t *testing.T) {
	toolSrv := mock.NewMockToolServer()

	for _, name := range []string{"s1", "s2"} {
		name := name
		err := toolSrv.Register(context.Background(), &testToolDef{
			name: name,
			handler: func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
				return &tools.ToolResult{Output: name + "_out"}, nil
			},
		})
		require.NoError(t, err)
	}

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"T", "sequential-default",
		mock.ConversationTurn{
			AssistantContent: "running tools",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "c1", Name: "s1"},
				{ID: "c2", Name: "s2"},
			},
		},
		mock.ConversationTurn{AssistantContent: "done"},
	))

	// No WithExecutionMode -> default is sequential.
	loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv))

	events, err := loop.Run(context.Background(), Submission{Content: "run"})
	require.NoError(t, err)

	toolCalls := findEvents(events, "tool_call")
	assert.Len(t, toolCalls, 2)

	toolResults := findEvents(events, "tool_result")
	assert.Len(t, toolResults, 2)
}

func TestExecuteSingleToolNoRegistry(t *testing.T) {
	_, err := executeSingleTool(context.Background(), nil, tools.ToolCall{Name: "x"})
	assert.Error(t, err)
	assert.Equal(t, errNoTools, err)
}

func TestExecuteToolsParallelConcurrency(t *testing.T) {
	toolSrv := mock.NewMockToolServer()

	var mu sync.Mutex
	activeCount := 0
	maxActive := 0

	for i := 0; i < 5; i++ {
		err := toolSrv.Register(context.Background(), &testToolDef{
			name: fmt.Sprintf("concurrent_%d", i),
			handler: func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
				mu.Lock()
				activeCount++
				if activeCount > maxActive {
					maxActive = activeCount
				}
				mu.Unlock()

				time.Sleep(30 * time.Millisecond)

				mu.Lock()
				activeCount--
				mu.Unlock()

				return &tools.ToolResult{Output: "done"}, nil
			},
		})
		require.NoError(t, err)
	}

	calls := []llm.ToolCall{
		{ID: "1", Name: "concurrent_0"},
		{ID: "2", Name: "concurrent_1"},
		{ID: "3", Name: "concurrent_2"},
		{ID: "4", Name: "concurrent_3"},
		{ID: "5", Name: "concurrent_4"},
	}

	results := executeToolsParallel(context.Background(), toolSrv, calls)
	require.Len(t, results, 5)

	for _, r := range results {
		assert.NoError(t, r.Err)
	}

	// At least 2 tools should have been active simultaneously.
	assert.GreaterOrEqual(t, maxActive, 2,
		"at least 2 tools should run concurrently")
}
