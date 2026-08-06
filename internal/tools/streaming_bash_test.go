package tools

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// streamEvent is a single recorded output line from a mockStreamSink.
type streamEvent struct {
	Content    string
	ToolCallID string
	Stream     string
}

// mockStreamSink is a thread-safe StreamSink that records all events.
type mockStreamSink struct {
	mu     sync.Mutex
	events []streamEvent
}

func (m *mockStreamSink) Send(content, toolCallID, stream string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, streamEvent{
		Content:    content,
		ToolCallID: toolCallID,
		Stream:     stream,
	})
	return nil
}

func (m *mockStreamSink) getEvents() []streamEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]streamEvent, len(m.events))
	copy(out, m.events)
	return out
}

// TestStreamingBashEcho verifies that a simple echo command produces the
// expected output.
func TestStreamingBashEcho(t *testing.T) {
	tool := NewStreamingBashTool()
	result, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-1",
		Name: "bash",
		Args: map[string]any{"command": "echo hello"},
	})
	require.NoError(t, err)
	assert.Equal(t, "hello\n", result.Output)
	assert.Equal(t, 0, result.Metadata["exit_code"])
}

// TestStreamingBashStdoutStderrSeparated verifies that stdout and stderr are
// captured in separate streams and correctly combined in the output.
func TestStreamingBashStdoutStderrSeparated(t *testing.T) {
	tool := NewStreamingBashTool()
	sink := &mockStreamSink{}
	result, err := tool.ExecuteStreaming(context.Background(), ToolCall{
		ID:   "call-sep",
		Name: "bash",
		Args: map[string]any{"command": "echo out; echo err >&2"},
	}, sink)
	require.NoError(t, err)

	// Output should contain both lines, stdout first then stderr.
	assert.Contains(t, result.Output, "out")
	assert.Contains(t, result.Output, "err")

	events := sink.getEvents()
	// Should have events from both streams.
	var stdoutLines, stderrLines []string
	for _, ev := range events {
		assert.Equal(t, "call-sep", ev.ToolCallID)
		switch ev.Stream {
		case "stdout":
			stdoutLines = append(stdoutLines, ev.Content)
		case "stderr":
			stderrLines = append(stderrLines, ev.Content)
		}
	}
	assert.Contains(t, strings.Join(stdoutLines, "\n"), "out")
	assert.Contains(t, strings.Join(stderrLines, "\n"), "err")
}

// TestStreamingBashStreamSink verifies that the StreamSink receives
// tool_output events with correct fields.
func TestStreamingBashStreamSink(t *testing.T) {
	tool := NewStreamingBashTool()
	sink := &mockStreamSink{}
	_, err := tool.ExecuteStreaming(context.Background(), ToolCall{
		ID:   "call-stream",
		Name: "bash",
		Args: map[string]any{"command": "echo line1; echo line2"},
	}, sink)
	require.NoError(t, err)

	events := sink.getEvents()
	require.Len(t, events, 2)
	assert.Equal(t, "line1", events[0].Content)
	assert.Equal(t, "line2", events[1].Content)
	assert.Equal(t, "call-stream", events[0].ToolCallID)
	assert.Equal(t, "call-stream", events[1].ToolCallID)
	assert.Equal(t, "stdout", events[0].Stream)
	assert.Equal(t, "stdout", events[1].Stream)
}

// TestStreamingBashTimeout verifies that a command exceeding the timeout is
// killed and returns an error.
func TestStreamingBashTimeout(t *testing.T) {
	tool := NewStreamingBashTool(WithTimeout(200 * time.Millisecond))
	result, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-timeout",
		Name: "bash",
		Args: map[string]any{"command": "sleep 5"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
	assert.Equal(t, -1, result.Metadata["exit_code"])
	assert.Equal(t, "timed out", result.Metadata["error"])
}

// TestStreamingBashTruncation verifies that output exceeding MaxOutput is
// truncated with the "[output truncated]" marker.
func TestStreamingBashTruncation(t *testing.T) {
	// Set a very small max output so truncation is triggered.
	tool := NewStreamingBashTool(WithMaxOutput(20))
	result, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-trunc",
		Name: "bash",
		Args: map[string]any{"command": "echo abcdefghijklmnopqrstuvwxyz0123456789"},
	})
	// The command itself succeeds (exit 0), only the output is truncated.
	require.NoError(t, err)
	assert.Contains(t, result.Output, "[output truncated]")
}

// TestStreamingBashSandbox verifies that sandbox validation blocks
// blacklisted commands.
func TestStreamingBashSandbox(t *testing.T) {
	sandbox := NewDefaultBashSandbox()
	tool := NewStreamingBashTool(WithBashSandbox(sandbox))
	_, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-sandbox",
		Name: "bash",
		Args: map[string]any{"command": "rm -rf /tmp/test"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blacklisted")
}

// TestStreamingBashBackwardCompatible verifies that Execute (nil sink) works
// correctly and produces the same output as ExecuteStreaming with nil.
func TestStreamingBashBackwardCompatible(t *testing.T) {
	tool := NewStreamingBashTool()

	// Execute uses nil sink internally.
	result1, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-bc1",
		Name: "bash",
		Args: map[string]any{"command": "echo backward"},
	})
	require.NoError(t, err)
	assert.Equal(t, "backward\n", result1.Output)

	// ExecuteStreaming with explicit nil should produce the same result.
	result2, err := tool.ExecuteStreaming(context.Background(), ToolCall{
		ID:   "call-bc2",
		Name: "bash",
		Args: map[string]any{"command": "echo backward"},
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, result1.Output, result2.Output)
}

// TestStreamingBashExitCode verifies that a failing command returns the
// correct exit code in metadata.
func TestStreamingBashExitCode(t *testing.T) {
	tool := NewStreamingBashTool()
	result, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-exit",
		Name: "bash",
		Args: map[string]any{"command": "exit 42"},
	})
	require.Error(t, err)
	assert.Equal(t, 42, result.Metadata["exit_code"])
}

// TestStreamingBashMissingCommand verifies that a missing command argument
// returns an error.
func TestStreamingBashMissingCommand(t *testing.T) {
	tool := NewStreamingBashTool()
	_, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-missing",
		Name: "bash",
		Args: map[string]any{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing string argument 'command'")
}

// TestStreamingBashMultilineOutput verifies that multiple lines of output are
// each streamed as separate events.
func TestStreamingBashMultilineOutput(t *testing.T) {
	tool := NewStreamingBashTool()
	sink := &mockStreamSink{}
	result, err := tool.ExecuteStreaming(context.Background(), ToolCall{
		ID:   "call-multi",
		Name: "bash",
		Args: map[string]any{"command": "printf 'a\\nb\\nc\\n'"},
	}, sink)
	require.NoError(t, err)
	assert.Equal(t, "a\nb\nc\n", result.Output)

	events := sink.getEvents()
	require.Len(t, events, 3)
	assert.Equal(t, "a", events[0].Content)
	assert.Equal(t, "b", events[1].Content)
	assert.Equal(t, "c", events[2].Content)
}

// TestStreamingBashImplementsInterfaces verifies compile-time interface
// satisfaction at runtime.
func TestStreamingBashImplementsInterfaces(t *testing.T) {
	tool := NewStreamingBashTool()
	var _ ToolDefinition = tool
	var _ StreamingBashTool = tool
	assert.Equal(t, "bash", tool.Name())
}
