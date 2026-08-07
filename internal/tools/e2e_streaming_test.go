//go:build e2e

package tools

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// orderedEvent wraps a streamEvent with a monotonically increasing arrival
// index so tests can assert ordering.
type orderedEvent struct {
	streamEvent
	order int
}

// orderedStreamSink is a thread-safe StreamSink that records each event with
// an arrival-order index.
type orderedStreamSink struct {
	mu      sync.Mutex
	events  []orderedEvent
	counter int
}

func (m *orderedStreamSink) Send(content, toolCallID, stream string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, orderedEvent{
		streamEvent: streamEvent{
			Content:    content,
			ToolCallID: toolCallID,
			Stream:     stream,
		},
		order: m.counter,
	})
	m.counter++
	return nil
}

func (m *orderedStreamSink) getEvents() []orderedEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]orderedEvent, len(m.events))
	copy(out, m.events)
	return out
}

// TestE2EBashStreamingEcho verifies that a simple echo command streams
// output to the sink before ExecuteStreaming returns, and that the final
// result matches the expected output.
func TestE2EBashStreamingEcho(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tool := NewStreamingBashTool()
	sink := &mockStreamSink{}

	result, err := tool.ExecuteStreaming(ctx, ToolCall{
		ID:   "e2e-echo",
		Name: "bash",
		Args: map[string]any{"command": "echo hello world"},
	}, sink)
	require.NoError(t, err)

	// tool_output events must have been received BEFORE ExecuteStreaming
	// returns.
	events := sink.getEvents()
	require.NotEmpty(t, events, "sink should have events when ExecuteStreaming returns")

	// Final result output.
	assert.Equal(t, "hello world\n", result.Output)

	// Every event should carry the correct ToolCallID and stream.
	for _, ev := range events {
		assert.Equal(t, "e2e-echo", ev.ToolCallID)
		assert.Equal(t, "stdout", ev.Stream)
	}
}

// TestE2EBashStreamingEventOrder verifies that streaming events arrive in
// the correct order and that all events are collected before ExecuteStreaming
// returns.
func TestE2EBashStreamingEventOrder(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tool := NewStreamingBashTool()
	sink := &orderedStreamSink{}

	result, err := tool.ExecuteStreaming(ctx, ToolCall{
		ID:   "e2e-order",
		Name: "bash",
		Args: map[string]any{"command": "echo line1; echo line2; echo line3"},
	}, sink)
	require.NoError(t, err)

	events := sink.getEvents()

	// All events must be collected before ExecuteStreaming returns.
	require.Len(t, events, 3, "all events should be collected before result is returned")

	// Verify order indices are monotonically increasing.
	assert.Equal(t, 0, events[0].order)
	assert.Equal(t, 1, events[1].order)
	assert.Equal(t, 2, events[2].order)

	// Verify content arrives in order.
	assert.Equal(t, "line1", events[0].Content)
	assert.Equal(t, "line2", events[1].Content)
	assert.Equal(t, "line3", events[2].Content)

	// Verify all events carry the correct ToolCallID.
	for _, ev := range events {
		assert.Equal(t, "e2e-order", ev.ToolCallID)
	}

	// The result output should contain all three lines.
	assert.Contains(t, result.Output, "line1")
	assert.Contains(t, result.Output, "line2")
	assert.Contains(t, result.Output, "line3")
}

// TestE2EBashStreamingLargeOutput verifies that a large output is truncated
// and that the command does not block/hang.
func TestE2EBashStreamingLargeOutput(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tool := NewStreamingBashTool(WithMaxOutput(5000))
	sink := &mockStreamSink{}

	done := make(chan struct{})
	var (
		result *ToolResult
		err    error
	)
	go func() {
		result, err = tool.ExecuteStreaming(ctx, ToolCall{
			ID:   "e2e-large",
			Name: "bash",
			Args: map[string]any{"command": "seq 1 10000"},
		}, sink)
		close(done)
	}()

	select {
	case <-done:
		// Command completed.
	case <-time.After(10 * time.Second):
		t.Fatal("command did not complete within 10 seconds")
	}

	require.NoError(t, err)
	assert.Contains(t, result.Output, "[output truncated]")

	// The sink should have received many events, but not all 10000 due to
	// truncation.
	events := sink.getEvents()
	assert.Greater(t, len(events), 0, "sink should have received some events")
	assert.Less(t, len(events), 10000, "sink should not have received all 10000 events")
}

// TestE2EBashStreamingTimeoutCancel verifies that a command exceeding the
// timeout returns an error quickly, with correct metadata, and without
// leaking goroutines.
func TestE2EBashStreamingTimeoutCancel(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tool := NewStreamingBashTool(WithTimeout(200 * time.Millisecond))

	start := time.Now()

	result, err := tool.ExecuteStreaming(ctx, ToolCall{
		ID:   "e2e-timeout",
		Name: "bash",
		Args: map[string]any{"command": "sleep 5"},
	}, nil)

	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")

	// Should return quickly, well within 1 second.
	assert.Less(t, elapsed, time.Second, "should return within ~1 second, not wait for sleep 5")

	// Result metadata.
	require.NotNil(t, result)
	assert.Equal(t, -1, result.Metadata["exit_code"])
	assert.Equal(t, "timed out", result.Metadata["error"])
}

// TestE2EBashStreamingStdoutStderrSeparation verifies that stdout and stderr
// are captured as separate streams.
func TestE2EBashStreamingStdoutStderrSeparation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tool := NewStreamingBashTool()
	sink := &mockStreamSink{}

	_, err := tool.ExecuteStreaming(ctx, ToolCall{
		ID:   "e2e-sep",
		Name: "bash",
		Args: map[string]any{"command": "echo to_stdout; echo to_stderr >&2"},
	}, sink)
	require.NoError(t, err)

	events := sink.getEvents()
	require.NotEmpty(t, events)

	var stdoutContents, stderrContents []string
	for _, ev := range events {
		assert.Equal(t, "e2e-sep", ev.ToolCallID)
		switch ev.Stream {
		case "stdout":
			stdoutContents = append(stdoutContents, ev.Content)
		case "stderr":
			stderrContents = append(stderrContents, ev.Content)
		}
	}

	assert.NotEmpty(t, stdoutContents, "should have stdout events")
	assert.NotEmpty(t, stderrContents, "should have stderr events")
	assert.Contains(t, strings.Join(stdoutContents, "\n"), "to_stdout")
	assert.Contains(t, strings.Join(stderrContents, "\n"), "to_stderr")
}

// BenchmarkStreamingBashVsBashTool compares the performance of the streaming
// bash tool (with nil sink) against the original bash tool.
func BenchmarkStreamingBashVsBashTool(b *testing.B) {
	ctx := context.Background()
	call := ToolCall{
		ID:   "bench",
		Name: "bash",
		Args: map[string]any{"command": "echo benchmark_test"},
	}

	b.Run("StreamingBashTool", func(b *testing.B) {
		tool := NewStreamingBashTool()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, err := tool.ExecuteStreaming(ctx, call, nil)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("OriginalBashTool", func(b *testing.B) {
		tool := NewBashTool()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, err := tool.Execute(ctx, call)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}
