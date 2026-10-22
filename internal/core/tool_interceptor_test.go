package core

import (
	"context"
	"errors"
	"fmt"
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

	result, err := loop.RunCanonical(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)
	events := result.Events

	// Tool must NOT have been executed.
	assert.False(t, tool.executed.Load(), "tool should not execute when interceptor cancels")

	// A tool_canceled event must be present.
	cancelled := findEvents(events, "tool_canceled")
	require.Len(t, cancelled, 1)
	assert.Equal(t, "danger", cancelled[0])

	// Cancellation must finish the proposal with one known interrupted result,
	// rather than leave event consumers waiting for a tool that never ran.
	var canceledEvents, results []AgentEvent
	for _, event := range events {
		switch event.Kind {
		case "tool_canceled":
			canceledEvents = append(canceledEvents, event)
		case "tool_result":
			results = append(results, event)
		}
	}
	require.Len(t, canceledEvents, 1)
	assert.Equal(t, "c1", canceledEvents[0].ToolCallID)
	assert.Equal(t, ReceiptOutcomeInterrupted, canceledEvents[0].ReceiptOutcome)
	require.Len(t, results, 1)
	assert.Equal(t, "c1", results[0].ToolCallID)
	assert.Equal(t, "Tool call canceled by interceptor", results[0].Content)
	assert.True(t, results[0].IsError)
	assert.Equal(t, ReceiptOutcomeInterrupted, results[0].ReceiptOutcome)
	assert.Empty(t, findEvents(events, "tool_call"), "cancellation must precede dispatch")

	// The canonical receipt retains the exact tool identity and terminal truth.
	var receipts []AgentMessage
	for _, entry := range result.Transcript {
		if entry.Message.Role == string(llm.RoleTool) {
			receipts = append(receipts, entry.Message)
		}
	}
	require.Len(t, receipts, 1)
	assert.Equal(t, "c1", receipts[0].ToolCallID)
	assert.Equal(t, "danger", receipts[0].ToolName)
	assert.Equal(t, results[0].Content, receipts[0].Content)
	assert.Equal(t, ReceiptOutcomeInterrupted, receipts[0].ReceiptOutcome)
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
	cancelled := findEvents(events, "tool_canceled")
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
	require.Len(t, findEvents(events, "tool_canceled"), 1)
}

func TestLoopAgentInterceptorIsScopedToInstance(t *testing.T) {
	t.Cleanup(ClearToolInterceptors)

	toolA := &trackingTool{name: "mutate"}
	toolB := &trackingTool{name: "mutate"}
	modelA := &scriptedChatModel{seq: []*llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "a-call", Name: "mutate", Args: map[string]any{}}}},
		{Role: llm.RoleAssistant, Content: "done A"},
	}}
	modelB := &scriptedChatModel{seq: []*llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "b-call", Name: "mutate", Args: map[string]any{}}}},
		{Role: llm.RoleAssistant, Content: "done B"},
	}}
	loopA := NewLoopAgent(WithLLM(modelA), WithTools(scriptedRegistry(toolA)))
	loopB := NewLoopAgent(WithLLM(modelB), WithTools(scriptedRegistry(toolB)))
	loopA.SetToolInterceptor(func(string, string, map[string]any) error {
		return errors.New("assembly A blocks its mutation")
	})

	_, err := loopB.Run(context.Background(), Submission{Content: "run B"})
	require.NoError(t, err)
	assert.True(t, toolB.executed.Load(), "loop A's interceptor must not cancel loop B's tool call")
	assert.False(t, toolA.executed.Load(), "setting loop A's interceptor must not execute A's tool")
}

func TestWithToolInterceptorBlocksToolExecution(t *testing.T) {
	tool := &trackingTool{name: "blocked"}
	model := &scriptedChatModel{seq: []*llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call", Name: "blocked", Args: map[string]any{}}}},
		{Role: llm.RoleAssistant, Content: "done"},
	}}
	loop := NewLoopAgent(
		WithLLM(model),
		WithTools(scriptedRegistry(tool)),
		WithToolInterceptor(func(string, string, map[string]any) error {
			return errors.New("blocked by loop option")
		}),
	)

	events, err := loop.Run(context.Background(), Submission{Content: "run"})
	require.NoError(t, err)
	assert.False(t, tool.executed.Load())
	require.Len(t, findEvents(events, "tool_canceled"), 1)
}

func TestContextToolInterceptorReceivesRunContext(t *testing.T) {
	type contextKey struct{}
	tool := &trackingTool{name: "context-aware"}
	model := &scriptedChatModel{seq: []*llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call", Name: "context-aware", Args: map[string]any{}}}},
		{Role: llm.RoleAssistant, Content: "done"},
	}}
	loop := NewLoopAgent(WithLLM(model), WithTools(scriptedRegistry(tool)))
	var observed any
	loop.SetContextToolInterceptor(func(ctx context.Context, _, _ string, _ map[string]any) error {
		observed = ctx.Value(contextKey{})
		return nil
	})

	_, err := loop.Run(context.WithValue(context.Background(), contextKey{}, "run-context"), Submission{Content: "run"})
	require.NoError(t, err)
	require.Equal(t, "run-context", observed)
	require.True(t, tool.executed.Load())
}

func TestToolInterceptionReceiptsExposeOnlyTypedSafeMessages(t *testing.T) {
	for _, execution := range []struct {
		name  string
		mode  ExecutionMode
		calls int
	}{
		{name: "sequential", mode: ExecutionModeSequential, calls: 1},
		{name: "parallel", mode: ExecutionModeParallel, calls: 2},
	} {
		for _, denial := range []struct {
			name     string
			err      error
			wantText string
		}{
			{name: "typed-safe", err: testSafeToolInterceptionDenial{}, wantText: "safe policy denial"},
			{name: "arbitrary", err: errors.New("private interceptor details"), wantText: toolInterceptorCanceledOutput},
		} {
			t.Run(execution.name+"/"+denial.name, func(t *testing.T) {
				calls := make([]llm.ToolCall, execution.calls)
				for index := range calls {
					calls[index] = llm.ToolCall{ID: fmt.Sprintf("call-%d", index), Name: "blocked", Args: map[string]any{}}
				}
				model := &scriptedChatModel{seq: []*llm.Message{
					{Role: llm.RoleAssistant, ToolCalls: calls},
					{Role: llm.RoleAssistant, Content: "done"},
				}}
				loop := NewLoopAgent(WithLLM(model), WithTools(scriptedRegistry(&trackingTool{name: "blocked"})), WithExecutionMode(execution.mode))
				loop.SetContextToolInterceptor(func(context.Context, string, string, map[string]any) error {
					return denial.err
				})

				_, err := loop.Run(context.Background(), Submission{Content: "run"})
				require.NoError(t, err)

				model.mu.Lock()
				requests := append([][]llm.Message(nil), model.requests...)
				model.mu.Unlock()
				require.GreaterOrEqual(t, len(requests), 2)
				var receipts []string
				for _, message := range requests[1] {
					if message.Role == llm.RoleTool {
						receipts = append(receipts, message.Content)
					}
				}
				require.Len(t, receipts, execution.calls)
				for _, receipt := range receipts {
					assert.Equal(t, denial.wantText, receipt)
					assert.NotContains(t, receipt, "private interceptor details")
				}
			})
		}
	}
}

func TestPreToolCallEventRetainsContextDenialWithoutExposingArbitraryMessages(t *testing.T) {
	secret := errors.New("private interceptor details")
	event := &PreToolCallEvent{
		ctx: context.Background(),
		contextInterceptors: []ContextToolInterceptor{func(context.Context, string, string, map[string]any) error {
			return secret
		}},
	}
	require.True(t, event.IsCancelled())
	require.ErrorIs(t, event.CancellationError(), secret)
	_, safe := SafeToolInterceptionMessage(event.CancellationError())
	require.False(t, safe, "arbitrary interceptor errors must not become user-facing tool receipts")

	message, safe := SafeToolInterceptionMessage(testSafeToolInterceptionDenial{})
	require.True(t, safe)
	require.Equal(t, "safe policy denial", message)
}

type testSafeToolInterceptionDenial struct{}

func (testSafeToolInterceptionDenial) Error() string { return "internal error details" }

func (testSafeToolInterceptionDenial) SafeToolInterceptionMessage() string {
	return "safe policy denial"
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
