package core

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// mockStreamingTool implements both tools.ToolDefinition and
// tools.StreamingBashTool so tests can verify which execution path the loop
// takes. It records whether ExecuteStreaming or Execute was invoked and
// optionally pushes canned lines through the StreamSink.
type mockStreamingTool struct {
	mu              sync.Mutex
	name            string
	streamingCalled bool
	executeCalled   bool
	streamingCalls  int
	executeCalls    int
	output          string
	sinkLines       []string // lines pushed through the sink when streaming
	sinkStream      string   // stream tag for sink lines ("stdout"/"stderr")
	err             error
}

func (t *mockStreamingTool) Name() string        { return t.name }
func (t *mockStreamingTool) Description() string { return "mock streaming tool" }

func (t *mockStreamingTool) Execute(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
	t.mu.Lock()
	t.executeCalled = true
	t.executeCalls++
	t.mu.Unlock()
	if t.err != nil {
		return nil, t.err
	}
	return &tools.ToolResult{Output: t.output}, nil
}

func (t *mockStreamingTool) ExecuteStreaming(_ context.Context, call tools.ToolCall, sink tools.StreamSink) (*tools.ToolResult, error) {
	t.mu.Lock()
	t.streamingCalled = true
	t.streamingCalls++
	t.mu.Unlock()
	if sink != nil {
		stream := t.sinkStream
		if stream == "" {
			stream = "stdout"
		}
		for _, line := range t.sinkLines {
			_ = sink.Send(line, call.ID, stream)
		}
	}
	if t.err != nil {
		return nil, t.err
	}
	return &tools.ToolResult{Output: t.output}, nil
}

var _ tools.StreamingBashTool = (*mockStreamingTool)(nil)

// collectingEventStream is a minimal EventStream that records every Send call
// into a slice. It is thread-safe for use in parallel execution tests.
type collectingEventStream struct {
	mu     sync.Mutex
	events []AgentEvent
}

func (c *collectingEventStream) Send(ev AgentEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
	return nil
}

func (c *collectingEventStream) Events() <-chan AgentEvent { return nil }
func (c *collectingEventStream) Close()                    {}
func (c *collectingEventStream) Result() (AgentMessage, error) {
	return AgentMessage{}, nil
}
func (c *collectingEventStream) Err() error { return nil }

func (c *collectingEventStream) collected() []AgentEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]AgentEvent, len(c.events))
	copy(cp, c.events)
	return cp
}

var _ EventStream = (*collectingEventStream)(nil)

// TestExecuteToolStreamingDetected verifies that when a tool implements
// StreamingBashTool and an EventStream is provided, executeTool dispatches
// via ExecuteStreaming rather than Execute.
func TestExecuteToolStreamingDetected(t *testing.T) {
	tool := &mockStreamingTool{name: "stream_tool", output: "result"}
	tr := scriptedRegistry(tool)

	loop := NewLoopAgent(WithTools(tr))
	es := &collectingEventStream{}

	out, _, err := loop.executeTool(context.Background(), tools.ToolCall{
		ID:   "tc1",
		Name: "stream_tool",
		Args: map[string]any{},
	}, es)
	require.NoError(t, err)
	assert.Equal(t, "result", out)

	assert.True(t, tool.streamingCalled, "ExecuteStreaming should be called")
	assert.False(t, tool.executeCalled, "Execute should NOT be called when streaming path is taken")
}

// TestExecuteToolNonStreamingUsesExecute verifies that a plain ToolDefinition
// (one that does not implement StreamingBashTool) still goes through Execute
// even when an EventStream is provided.
func TestExecuteToolNonStreamingUsesExecute(t *testing.T) {
	// scriptedTool is a plain ToolDefinition (no StreamingBashTool).
	tool := scriptedTool{name: "plain", res: &tools.ToolResult{Output: "plain-out"}}
	tr := scriptedRegistry(tool)

	loop := NewLoopAgent(WithTools(tr))
	es := &collectingEventStream{}

	out, _, err := loop.executeTool(context.Background(), tools.ToolCall{
		ID:   "tc1",
		Name: "plain",
		Args: map[string]any{},
	}, es)
	require.NoError(t, err)
	assert.Equal(t, "plain-out", out)

	// No tool_output events should have been sent.
	for _, ev := range es.collected() {
		assert.NotEqual(t, "tool_output", ev.Kind)
	}
}

// TestExecuteToolStreamingSendsToolOutputEvents verifies that lines pushed
// through the StreamSink appear as "tool_output" events on the EventStream
// with the correct ToolCallID and Stream tags.
func TestExecuteToolStreamingSendsToolOutputEvents(t *testing.T) {
	tool := &mockStreamingTool{
		name:       "stream_tool",
		output:     "accumulated",
		sinkLines:  []string{"line1", "line2", "line3"},
		sinkStream: "stdout",
	}
	tr := scriptedRegistry(tool)

	loop := NewLoopAgent(WithTools(tr))
	es := &collectingEventStream{}

	out, _, err := loop.executeTool(context.Background(), tools.ToolCall{
		ID:   "call-42",
		Name: "stream_tool",
		Args: map[string]any{},
	}, es)
	require.NoError(t, err)
	assert.Equal(t, "accumulated", out)

	toolOutputs := filterEvents(es.collected(), "tool_output")
	require.Len(t, toolOutputs, 3, "three sink lines should produce three tool_output events")
	assert.Equal(t, "line1", toolOutputs[0].Content)
	assert.Equal(t, "call-42", toolOutputs[0].ToolCallID)
	assert.Equal(t, "stdout", toolOutputs[0].Stream)

	assert.Equal(t, "line2", toolOutputs[1].Content)
	assert.Equal(t, "call-42", toolOutputs[1].ToolCallID)

	assert.Equal(t, "line3", toolOutputs[2].Content)
	assert.Equal(t, "call-42", toolOutputs[2].ToolCallID)
}

// TestExecuteToolNilEventStreamFallsBack verifies that when the EventStream is
// nil, executeTool falls back to the regular Execute path even for tools that
// implement StreamingBashTool.
func TestExecuteToolNilEventStreamFallsBack(t *testing.T) {
	tool := &mockStreamingTool{name: "stream_tool", output: "fallback-result"}
	tr := scriptedRegistry(tool)

	loop := NewLoopAgent(WithTools(tr))

	// Pass nil EventStream — should use Execute, not ExecuteStreaming.
	out, _, err := loop.executeTool(context.Background(), tools.ToolCall{
		ID:   "tc1",
		Name: "stream_tool",
		Args: map[string]any{},
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, "fallback-result", out)

	assert.True(t, tool.executeCalled, "Execute should be called when es is nil")
	assert.False(t, tool.streamingCalled, "ExecuteStreaming should NOT be called when es is nil")
}

// TestExecuteToolsParallelStreaming verifies that streaming tools running in
// parallel each have ExecuteStreaming invoked and that all tool_output events
// are forwarded to the shared EventStream.
func TestExecuteToolsParallelStreaming(t *testing.T) {
	toolA := &mockStreamingTool{
		name:       "parallel_a",
		output:     "out_a",
		sinkLines:  []string{"a1", "a2"},
		sinkStream: "stdout",
	}
	toolB := &mockStreamingTool{
		name:       "parallel_b",
		output:     "out_b",
		sinkLines:  []string{"b1"},
		sinkStream: "stderr",
	}
	tr := scriptedRegistry(toolA, toolB)

	es := &collectingEventStream{}
	calls := []llm.ToolCall{
		{ID: "pa", Name: "parallel_a"},
		{ID: "pb", Name: "parallel_b"},
	}

	results, _ := executeToolsParallel(context.Background(), tr, calls, es)
	require.Len(t, results, 2)

	// Results preserve input order.
	assert.Equal(t, "pa", results[0].ID)
	assert.Equal(t, "out_a", results[0].Output)
	assert.NoError(t, results[0].Err)

	assert.Equal(t, "pb", results[1].ID)
	assert.Equal(t, "out_b", results[1].Output)
	assert.NoError(t, results[1].Err)

	// Both tools used the streaming path.
	assert.True(t, toolA.streamingCalled, "toolA should use ExecuteStreaming")
	assert.False(t, toolA.executeCalled, "toolA should NOT use Execute")
	assert.True(t, toolB.streamingCalled, "toolB should use ExecuteStreaming")
	assert.False(t, toolB.executeCalled, "toolB should NOT use Execute")

	// All 3 sink lines (2 + 1) should appear as tool_output events.
	toolOutputs := filterEvents(es.collected(), "tool_output")
	assert.Len(t, toolOutputs, 3, "all sink lines should be forwarded")

	// Verify each event carries the correct ToolCallID.
	ids := make(map[string]int)
	for _, ev := range toolOutputs {
		ids[ev.ToolCallID]++
	}
	assert.Equal(t, 2, ids["pa"], "toolA produced 2 lines")
	assert.Equal(t, 1, ids["pb"], "toolB produced 1 line")
}

// TestLoopSequentialStreamingIntegration verifies the end-to-end flow: a
// streaming tool invoked through the full Run loop in sequential mode pushes
// tool_output events to the EventStream while still feeding the accumulated
// output back to the model.
func TestLoopSequentialStreamingIntegration(t *testing.T) {
	tool := &mockStreamingTool{
		name:       "stream_tool",
		output:     "done-output",
		sinkLines:  []string{"streaming-line-1", "streaming-line-2"},
		sinkStream: "stdout",
	}
	tr := scriptedRegistry(tool)

	model := &scriptedChatModel{seq: []*llm.Message{
		{
			Role:    llm.RoleAssistant,
			Content: "calling streaming tool",
			ToolCalls: []llm.ToolCall{
				{ID: "sc1", Name: "stream_tool", Args: map[string]any{}},
			},
		},
		{Role: llm.RoleAssistant, Content: "finished"},
	}}

	loop := NewLoopAgent(WithLLM(model), WithTools(tr))
	es := &collectingEventStream{}

	events, err := loop.Run(context.Background(), Submission{Content: "run"}, es)
	require.NoError(t, err)

	// The loop should have invoked ExecuteStreaming.
	assert.True(t, tool.streamingCalled, "ExecuteStreaming should be called")

	// tool_output events go directly to the EventStream via the sink,
	// bypassing sendEvent, so they appear in es but not in the returned
	// events slice.
	esOutputs := filterEvents(es.collected(), "tool_output")
	require.Len(t, esOutputs, 2, "two streaming lines should produce two tool_output events")
	assert.Equal(t, "streaming-line-1", esOutputs[0].Content)
	assert.Equal(t, "sc1", esOutputs[0].ToolCallID)
	assert.Equal(t, "stdout", esOutputs[0].Stream)
	assert.Equal(t, "streaming-line-2", esOutputs[1].Content)

	// The tool_result event should carry the accumulated output.
	toolResults := findEvents(events, "tool_result")
	require.Len(t, toolResults, 1)
	assert.Equal(t, "done-output", toolResults[0])

	// The final message should be present.
	messages := findEvents(events, "message")
	require.Len(t, messages, 2)
	assert.Equal(t, "finished", messages[1])
}

// TestLoopParallelStreamingIntegration verifies the end-to-end flow with
// parallel execution mode: multiple streaming tools push output concurrently
// and all tool_output events reach the EventStream.
func TestLoopParallelStreamingIntegration(t *testing.T) {
	toolA := &mockStreamingTool{
		name:       "p_stream_a",
		output:     "a-out",
		sinkLines:  []string{"a-line"},
		sinkStream: "stdout",
	}
	toolB := &mockStreamingTool{
		name:       "p_stream_b",
		output:     "b-out",
		sinkLines:  []string{"b-line"},
		sinkStream: "stderr",
	}
	tr := scriptedRegistry(toolA, toolB)

	model := &scriptedChatModel{seq: []*llm.Message{
		{
			Role:    llm.RoleAssistant,
			Content: "running parallel",
			ToolCalls: []llm.ToolCall{
				{ID: "pa", Name: "p_stream_a", Args: map[string]any{}},
				{ID: "pb", Name: "p_stream_b", Args: map[string]any{}},
			},
		},
		{Role: llm.RoleAssistant, Content: "all done"},
	}}

	loop := NewLoopAgent(
		WithLLM(model),
		WithTools(tr),
		WithExecutionMode(ExecutionModeParallel),
	)
	es := &collectingEventStream{}

	events, err := loop.Run(context.Background(), Submission{Content: "run"}, es)
	require.NoError(t, err)

	// Both tools should have used the streaming path.
	assert.True(t, toolA.streamingCalled, "toolA should use ExecuteStreaming")
	assert.True(t, toolB.streamingCalled, "toolB should use ExecuteStreaming")

	// tool_output events go directly to the EventStream via the sink.
	esOutputs := filterEvents(es.collected(), "tool_output")
	require.Len(t, esOutputs, 2)

	// Verify each carries the correct ToolCallID and stream tag.
	byID := make(map[string]AgentEvent)
	for _, ev := range esOutputs {
		byID[ev.ToolCallID] = ev
	}
	assert.Contains(t, byID, "pa")
	assert.Equal(t, "a-line", byID["pa"].Content)
	assert.Equal(t, "stdout", byID["pa"].Stream)

	assert.Contains(t, byID, "pb")
	assert.Equal(t, "b-line", byID["pb"].Content)
	assert.Equal(t, "stderr", byID["pb"].Stream)

	// The tool_result events should carry the accumulated outputs.
	toolResults := findEvents(events, "tool_result")
	require.Len(t, toolResults, 2)
}

// filterEvents returns all events whose Kind equals kind.
func filterEvents(events []AgentEvent, kind string) []AgentEvent {
	var out []AgentEvent
	for _, ev := range events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}
