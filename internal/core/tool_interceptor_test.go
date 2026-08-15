package core

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// trackingTool records whether Execute was called. It is race-safe for use
// with `go test -race`.
type trackingTool struct {
	name     string
	executed atomic.Bool
}

func (t *trackingTool) Name() string        { return t.name }
func (t *trackingTool) Description() string { return "tracking tool" }
func (t *trackingTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	t.executed.Store(true)
	return &tools.ToolResult{Output: "executed"}, nil
}

var _ tools.ToolDefinition = (*trackingTool)(nil)

// AC-1: Registered PreToolCall interceptor can synchronously cancel a tool
// call before execution.
func TestToolInterceptor_CancelsBeforeExecution(t *testing.T) {
	t.Cleanup(ClearToolInterceptors)

	tool := &trackingTool{name: "danger"}
	toolSrv := scriptedRegistry(tool)

	model := &scriptedChatModel{seq: []*llm.Message{
		{
			Role:    llm.RoleAssistant,
			Content: "calling danger",
			ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "danger", Args: map[string]any{}},
			},
		},
		{Role: llm.RoleAssistant, Content: "done"},
	}}
	loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv))

	RegisterToolInterceptor(func(toolName, toolCallID string, args map[string]any) error {
		if toolName == "danger" {
			return errors.New("blocked by policy")
		}
		return nil
	})

	events, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)

	// Tool must NOT have been executed.
	assert.False(t, tool.executed.Load(), "tool should not execute when interceptor cancels")

	// A tool_cancelled event must be present.
	cancelled := findEvents(events, "tool_cancelled")
	require.Len(t, cancelled, 1)
	assert.Equal(t, "danger", cancelled[0])

	// No tool_result event for the cancelled call.
	results := findEvents(events, "tool_result")
	assert.Empty(t, results)
}

// AC-1 (negative): When the interceptor allows the call, the tool executes.
func TestToolInterceptor_AllowsExecution(t *testing.T) {
	t.Cleanup(ClearToolInterceptors)

	tool := &trackingTool{name: "safe"}
	toolSrv := scriptedRegistry(tool)

	model := &scriptedChatModel{seq: []*llm.Message{
		{
			Role:    llm.RoleAssistant,
			Content: "calling safe",
			ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "safe", Args: map[string]any{}},
			},
		},
		{Role: llm.RoleAssistant, Content: "done"},
	}}
	loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv))

	RegisterToolInterceptor(func(toolName, toolCallID string, args map[string]any) error {
		return nil
	})

	events, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)

	// Tool must have been executed.
	assert.True(t, tool.executed.Load(), "tool should execute when interceptor allows")

	// A tool_result event must be present.
	results := findEvents(events, "tool_result")
	require.Len(t, results, 1)
	assert.Equal(t, "executed", results[0])
}

// AC-2: Event serves only as notification — interception works without any
// EventStream consumer. The loop's Run is called with no stream argument, so
// sendEvent only appends to the local slice; no external consumer could call
// Cancel(). Yet the interceptor still blocks the tool.
func TestToolInterceptor_WorksWithoutEventStream(t *testing.T) {
	t.Cleanup(ClearToolInterceptors)

	tool := &trackingTool{name: "blocked"}
	toolSrv := scriptedRegistry(tool)

	model := &scriptedChatModel{seq: []*llm.Message{
		{
			Role:    llm.RoleAssistant,
			Content: "calling",
			ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "blocked", Args: map[string]any{}},
			},
		},
		{Role: llm.RoleAssistant, Content: "done"},
	}}
	loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv))

	RegisterToolInterceptor(func(toolName, toolCallID string, args map[string]any) error {
		return errors.New("denied")
	})

	// Run without any EventStream — interception must still work.
	events, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)

	assert.False(t, tool.executed.Load(), "tool should not execute even without event stream")
	cancelled := findEvents(events, "tool_cancelled")
	require.Len(t, cancelled, 1)
}

// AC-3: End-to-end timing test — the interceptor runs synchronously before
// executeTool. The tool must not execute until the interceptor returns nil.
func TestToolInterceptor_TimingInterceptorBeforeExecute(t *testing.T) {
	t.Cleanup(ClearToolInterceptors)

	tool := &trackingTool{name: "timed"}
	toolSrv := scriptedRegistry(tool)

	model := &scriptedChatModel{seq: []*llm.Message{
		{
			Role:    llm.RoleAssistant,
			Content: "calling",
			ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "timed", Args: map[string]any{}},
			},
		},
		{Role: llm.RoleAssistant, Content: "done"},
	}}
	loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv))

	var interceptorCalled atomic.Bool
	var toolRanBeforeInterceptor atomic.Bool

	RegisterToolInterceptor(func(toolName, toolCallID string, args map[string]any) error {
		// If the tool already executed before the interceptor returns,
		// the synchronous-before-execute guarantee is violated.
		if tool.executed.Load() {
			toolRanBeforeInterceptor.Store(true)
		}
		interceptorCalled.Store(true)
		return nil
	})

	_, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)

	assert.True(t, interceptorCalled.Load(), "interceptor must have been called")
	assert.False(t, toolRanBeforeInterceptor.Load(), "tool must not execute before interceptor returns")
	assert.True(t, tool.executed.Load(), "tool should execute after interceptor allows")
}

// AC-1 via SetToolInterceptor method on *LoopAgent (the OO API).
func TestLoopAgent_SetToolInterceptor(t *testing.T) {
	t.Cleanup(ClearToolInterceptors)

	tool := &trackingTool{name: "blocked"}
	toolSrv := scriptedRegistry(tool)

	model := &scriptedChatModel{seq: []*llm.Message{
		{
			Role:    llm.RoleAssistant,
			Content: "calling",
			ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "blocked", Args: map[string]any{}},
			},
		},
		{Role: llm.RoleAssistant, Content: "done"},
	}}
	loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv))
	loop.SetToolInterceptor(func(toolName, toolCallID string, args map[string]any) error {
		return errors.New("blocked")
	})

	events, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)

	assert.False(t, tool.executed.Load(), "tool should not execute")
	require.Len(t, findEvents(events, "tool_cancelled"), 1)
}

// Verify interceptors run exactly once per event (idempotency).
func TestPreToolCallEvent_InterceptorsRunOnce(t *testing.T) {
	t.Cleanup(ClearToolInterceptors)

	var callCount atomic.Int64
	RegisterToolInterceptor(func(toolName, toolCallID string, args map[string]any) error {
		callCount.Add(1)
		return nil
	})

	ev := &PreToolCallEvent{
		ToolName:   "test",
		ToolCallID: "id1",
		Args:       map[string]any{},
	}

	_ = ev.IsCancelled()
	_ = ev.IsCancelled()
	_ = ev.IsCancelled()

	assert.Equal(t, int64(1), callCount.Load(), "interceptor should run exactly once")
}

// Cancel() before IsCancelled() must skip interceptors entirely.
func TestPreToolCallEvent_CancelSkipsInterceptors(t *testing.T) {
	t.Cleanup(ClearToolInterceptors)

	var interceptorCalled atomic.Bool
	RegisterToolInterceptor(func(toolName, toolCallID string, args map[string]any) error {
		interceptorCalled.Store(true)
		return nil
	})

	ev := &PreToolCallEvent{
		ToolName:   "test",
		ToolCallID: "id2",
		Args:       map[string]any{},
	}

	ev.Cancel()
	assert.True(t, ev.IsCancelled(), "should be cancelled after Cancel()")
	assert.False(t, interceptorCalled.Load(), "interceptor should not run when already cancelled")
}

// Multiple interceptors: the first error cancels, remaining are skipped.
func TestToolInterceptor_MultipleFirstErrorCancels(t *testing.T) {
	t.Cleanup(ClearToolInterceptors)

	var secondCalled atomic.Bool
	RegisterToolInterceptor(func(toolName, toolCallID string, args map[string]any) error {
		return errors.New("first blocks")
	})
	RegisterToolInterceptor(func(toolName, toolCallID string, args map[string]any) error {
		secondCalled.Store(true)
		return nil
	})

	ev := &PreToolCallEvent{
		ToolName:   "test",
		ToolCallID: "id3",
		Args:       map[string]any{},
	}

	assert.True(t, ev.IsCancelled(), "should be cancelled by first interceptor")
	assert.False(t, secondCalled.Load(), "second interceptor should not run after first errors")
}
