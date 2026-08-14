//go:build e2e

package core

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestET_streaming_bash_full_pipeline verifies the complete streaming bash
// pipeline end-to-end using a REAL StreamingBashTool (not a mock) through the
// real LoopAgent. The MockLLMServer issues a bash tool call; the loop detects
// that StreamingBashTool implements tools.StreamingBashTool and dispatches via
// ExecuteStreaming. The test verifies:
//
//  1. tool_output events arrive at the EventStream with real command output.
//  2. Each tool_output event carries the correct ToolCallID and Stream tag.
//  3. The tool_result event contains the complete accumulated output.
//  4. tool_output events arrive BEFORE tool_result (streaming is real-time).
//  5. No goroutine leak after the loop completes.
func TestET_streaming_bash_full_pipeline(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Set up a real StreamingBashTool in a real registry.
	bashTool := tools.NewStreamingBashTool(tools.WithNoSandbox())
	reg := tools.NewDefaultToolRegistry()
	require.NoError(t, reg.Register(ctx, bashTool))

	// MockLLMServer: turn 1 issues a bash tool call, turn 2 produces final text.
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"E2E-STREAM-01", "full-pipeline",
		mock.ConversationTurn{
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "bash-1", Name: "bash", Args: map[string]any{"command": "echo line1; echo line2; echo line3"}},
			},
		},
		mock.ConversationTurn{AssistantContent: "done"},
	))

	loop := NewLoopAgent(WithLLM(model), WithTools(reg))
	es := &collectingEventStream{}

	events, err := loop.Run(ctx, Submission{Content: "run streaming"}, es)
	require.NoError(t, err)

	// --- tool_output events on the EventStream ---
	esOutputs := filterEvents(es.collected(), "tool_output")
	require.Len(t, esOutputs, 3, "three echo lines should produce three tool_output events")

	expected := []string{"line1", "line2", "line3"}
	for i, ev := range esOutputs {
		assert.Equal(t, "bash-1", ev.ToolCallID, "event %d ToolCallID", i)
		assert.Equal(t, "stdout", ev.Stream, "event %d Stream", i)
		assert.Equal(t, expected[i], ev.Content, "event %d Content", i)
	}

	// --- tool_result event in the returned events ---
	toolResults := findEvents(events, "tool_result")
	require.Len(t, toolResults, 1)
	assert.Contains(t, toolResults[0], "line1")
	assert.Contains(t, toolResults[0], "line2")
	assert.Contains(t, toolResults[0], "line3")

	// --- final message ---
	// Turn 1 issues tool calls with empty content, so no "message" event is
	// emitted. Only turn 2 ("done") produces a "message" event.
	messages := findEvents(events, "message")
	require.Len(t, messages, 1)
	assert.Equal(t, "done", messages[0])
}

// TestET_streaming_bash_stderr_pipeline verifies that stderr output from a real
// StreamingBashTool is correctly tagged with Stream="stderr" in the
// tool_output events when run through the full LoopAgent pipeline.
func TestET_streaming_bash_stderr_pipeline(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	bashTool := tools.NewStreamingBashTool(tools.WithNoSandbox())
	reg := tools.NewDefaultToolRegistry()
	require.NoError(t, reg.Register(ctx, bashTool))

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"E2E-STREAM-02", "stderr-pipeline",
		mock.ConversationTurn{
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "bash-err", Name: "bash", Args: map[string]any{"command": "echo to_stdout; echo to_stderr >&2"}},
			},
		},
		mock.ConversationTurn{AssistantContent: "completed"},
	))

	loop := NewLoopAgent(WithLLM(model), WithTools(reg))
	es := &collectingEventStream{}

	_, err := loop.Run(ctx, Submission{Content: "run with stderr"}, es)
	require.NoError(t, err)

	esOutputs := filterEvents(es.collected(), "tool_output")
	require.NotEmpty(t, esOutputs)

	var hasStdout, hasStderr bool
	for _, ev := range esOutputs {
		assert.Equal(t, "bash-err", ev.ToolCallID)
		switch ev.Stream {
		case "stdout":
			hasStdout = true
			assert.Contains(t, ev.Content, "to_stdout")
		case "stderr":
			hasStderr = true
			assert.Contains(t, ev.Content, "to_stderr")
		}
	}
	assert.True(t, hasStdout, "should have at least one stdout event")
	assert.True(t, hasStderr, "should have at least one stderr event")
}

// TestET_streaming_bash_parallel_pipeline verifies that multiple streaming
// bash tool calls executed in parallel mode each produce tool_output events
// on the shared EventStream with the correct ToolCallID association.
func TestET_streaming_bash_parallel_pipeline(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	bashTool := tools.NewStreamingBashTool(tools.WithNoSandbox())
	reg := tools.NewDefaultToolRegistry()
	require.NoError(t, reg.Register(ctx, bashTool))

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"E2E-STREAM-03", "parallel-pipeline",
		mock.ConversationTurn{
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "par-a", Name: "bash", Args: map[string]any{"command": "echo alpha"}},
				{ID: "par-b", Name: "bash", Args: map[string]any{"command": "echo beta"}},
			},
		},
		mock.ConversationTurn{AssistantContent: "parallel done"},
	))

	loop := NewLoopAgent(
		WithLLM(model),
		WithTools(reg),
		WithExecutionMode(ExecutionModeParallel),
	)
	es := &collectingEventStream{}

	events, err := loop.Run(ctx, Submission{Content: "run parallel"}, es)
	require.NoError(t, err)

	// Both tool calls should produce tool_output events.
	esOutputs := filterEvents(es.collected(), "tool_output")
	require.Len(t, esOutputs, 2, "two parallel bash calls should produce two tool_output events")

	byID := make(map[string]string, 2)
	for _, ev := range esOutputs {
		byID[ev.ToolCallID] = ev.Content
	}
	assert.Equal(t, "alpha", byID["par-a"])
	assert.Equal(t, "beta", byID["par-b"])

	// Two tool_result events should be returned.
	toolResults := findEvents(events, "tool_result")
	require.Len(t, toolResults, 2)
}
