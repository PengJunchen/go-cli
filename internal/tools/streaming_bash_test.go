package tools

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type dataThenErrorReader struct {
	data     []byte
	err      error
	afterErr error
	once     bool
}

func (r *dataThenErrorReader) Read(p []byte) (int, error) {
	if !r.once {
		r.once = true
		return copy(p, r.data), r.err
	}
	if r.afterErr != nil {
		return 0, r.afterErr
	}
	return 0, io.EOF
}

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

type scriptedReadErrorReader struct {
	data     []byte
	readErr  error
	drainErr error
	sentData bool
}

func (r *scriptedReadErrorReader) Read(p []byte) (int, error) {
	if !r.sentData {
		r.sentData = true
		return copy(p, r.data), r.readErr
	}
	if r.drainErr != nil {
		return 0, r.drainErr
	}
	return 0, io.EOF
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
	tool := NewStreamingBashTool(WithNoSandbox())
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
	tool := NewStreamingBashTool(WithNoSandbox())
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
	tool := NewStreamingBashTool(WithNoSandbox())
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

func TestStreamingBashReportsPreparedProcessCompletionAfterNormalAndError(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		command   string
		wantError bool
	}{
		{name: "normal", command: "printf joined"},
		{name: "nonzero exit", command: "printf failed; exit 7", wantError: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			reported := make(chan struct{})
			var proof PreparedCompletionProof
			var processDone <-chan struct{}
			ctx := withPreparedCompletionReporter(context.Background(), func(got PreparedCompletionProof, done <-chan struct{}) {
				proof = got
				processDone = done
				close(reported)
			})

			tool := NewStreamingBashTool(WithNoSandbox())
			_, err := tool.ExecuteStreaming(ctx, ToolCall{
				ID:   "call-completion",
				Name: "bash",
				Args: map[string]any{"command": testCase.command},
			}, &mockStreamSink{})
			if testCase.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			select {
			case <-reported:
			case <-time.After(5 * time.Second):
				t.Fatal("streaming bash did not report its completed local process group")
			}
			assert.Equal(t, CompletionOwnedProcessGroupJoined, proof)
			require.NotNil(t, processDone)
			select {
			case <-processDone:
			default:
				t.Fatal("streaming bash reported before the process group and readers joined")
			}
		})
	}
}

// TestStreamingBashTimeout verifies that a command exceeding the timeout is
// killed and returns an error.
func TestStreamingBashTimeout(t *testing.T) {
	tool := NewStreamingBashTool(WithTimeout(200*time.Millisecond), WithNoSandbox())
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
	tool := NewStreamingBashTool(WithMaxOutput(20), WithNoSandbox())
	result, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-trunc",
		Name: "bash",
		Args: map[string]any{"command": "echo abcdefghijklmnopqrstuvwxyz0123456789"},
	})
	// The command itself succeeds (exit 0), only the output is truncated.
	require.NoError(t, err)
	assert.Contains(t, result.Output, "[output truncated]")
}

func TestReadPipeReportsReaderErrorAfterDraining(t *testing.T) {
	const maxOutput = 10
	readErr := errors.New("injected reader failure")
	reader := &scriptedReadErrorReader{
		data:    []byte("kept\n"),
		readErr: readErr,
	}
	capture := newStreamingBashOutputCapture(maxOutput)

	drained, err := readPipe(reader, "stdout", nil, "call-reader-error", capture)
	require.True(t, drained)
	require.ErrorIs(t, err, readErr)
	output, truncated := capture.output()
	assert.True(t, truncated)
	assert.Contains(t, output, "kept\n")
	assert.Contains(t, output, "[output truncated]")
	assert.LessOrEqual(t, len(output), maxOutput+len("\n[output truncated]"))
}

func TestReadPipeReportsDrainFailure(t *testing.T) {
	const maxOutput = 10
	readErr := errors.New("injected scanner read failure")
	drainErr := errors.New("injected drain failure")
	reader := &scriptedReadErrorReader{
		data:     []byte("kept\n"),
		readErr:  readErr,
		drainErr: drainErr,
	}
	capture := newStreamingBashOutputCapture(maxOutput)

	drained, err := readPipe(reader, "stderr", nil, "call-drain-error", capture)
	assert.False(t, drained)
	assert.ErrorIs(t, err, readErr)
	assert.ErrorIs(t, err, drainErr)
	output, truncated := capture.output()
	assert.True(t, truncated)
	assert.Contains(t, output, "kept\n")
	assert.Contains(t, output, "[output truncated]")
	assert.LessOrEqual(t, len(output), maxOutput+len("\n[output truncated]"))
}

// TestStreamingBashDrainsPastMaxOutput verifies that a finite command can
// finish after output capture reaches its limit, while retained output and
// streamed events stay bounded and the result reports truncation.
func TestStreamingBashDrainsPastMaxOutput(t *testing.T) {
	const (
		maxOutput = 20
		timeout   = 1200 * time.Millisecond
	)
	tool := NewStreamingBashTool(
		WithTimeout(timeout),
		WithMaxOutput(maxOutput),
		WithNoSandbox(),
	)
	sink := &mockStreamSink{}
	started := time.Now()
	result, err := tool.ExecuteStreaming(context.Background(), ToolCall{
		ID:   "call-drain",
		Name: "bash",
		Args: map[string]any{"command": "printf 'x\\n%.0s' {1..100000}"},
	}, sink)
	elapsed := time.Since(started)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Less(t, elapsed, timeout)
	assert.LessOrEqual(t, len(result.Output), maxOutput+len("\n[output truncated]"))
	assert.Contains(t, result.Output, "[output truncated]")
	assert.LessOrEqual(t, len(result.Output)-len("\n[output truncated]"), maxOutput)
	assert.Equal(t, true, result.Metadata["truncated"])

	var streamedBytes int
	var truncationEvents int
	for _, event := range sink.getEvents() {
		if event.Content == "[output truncated]" {
			truncationEvents++
		}
		streamedBytes += len(event.Content) + 1
	}
	assert.Equal(t, 1, truncationEvents)
	assert.LessOrEqual(t, streamedBytes, maxOutput+len("\n[output truncated]"))
}

func TestStreamingBashDrainsConcurrentStdoutStderr(t *testing.T) {
	const (
		maxOutput = 20
		timeout   = 3 * time.Second
	)
	tool := NewStreamingBashTool(
		WithTimeout(timeout),
		WithMaxOutput(maxOutput),
		WithNoSandbox(),
	)
	sink := &mockStreamSink{}
	result, err := tool.ExecuteStreaming(context.Background(), ToolCall{
		ID:   "call-drain-both",
		Name: "bash",
		Args: map[string]any{"command": "for ((i=0; i<100000; i++)); do printf 'x\\n'; printf 'y\\n' >&2; done"},
	}, sink)
	require.NoError(t, err)
	require.NotNil(t, result)
	// Combined ToolResult output adds one presentation separator when both
	// streams retain bytes; that separator is not a source byte.
	assert.LessOrEqual(t, len(result.Output), maxOutput+1+len("\n[output truncated]"))
	assert.Contains(t, result.Output, "[output truncated]")
	assert.LessOrEqual(t, len(result.Output)-len("\n[output truncated]"), maxOutput+1)
	assert.Equal(t, true, result.Metadata["truncated"])

	var streamedBytes int
	var truncationEvents int
	for _, event := range sink.getEvents() {
		if event.Content == "[output truncated]" {
			truncationEvents++
		}
		streamedBytes += len(event.Content)
		if event.Content != "[output truncated]" {
			streamedBytes++
		}
	}
	assert.Equal(t, 1, truncationEvents)
	assert.LessOrEqual(t, streamedBytes, maxOutput+len("\n[output truncated]"))
}

func TestStreamingBashLongLineReportsScannerErrorAndDrains(t *testing.T) {
	const (
		maxOutput = 20
		timeout   = 3 * time.Second
	)
	tool := NewStreamingBashTool(
		WithTimeout(timeout),
		WithMaxOutput(maxOutput),
		WithNoSandbox(),
	)
	started := time.Now()
	result, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-long-line",
		Name: "bash",
		Args: map[string]any{"command": "printf '%2000000s' ''"},
	})
	elapsed := time.Since(started)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "timed out")
	require.NotNil(t, result)
	assert.Less(t, elapsed, timeout)
	assert.LessOrEqual(t, len(result.Output), maxOutput+len("\n[output truncated]"))
	assert.Contains(t, result.Output, "[output truncated]")
	assert.LessOrEqual(t, len(result.Output)-len("\n[output truncated]"), maxOutput)
	assert.Equal(t, true, result.Metadata["truncated"])
	assert.Contains(t, result.Metadata["read_error"], "token too long")
}

func TestReadPipeSurfacesNonEOFReadErrorAfterDraining(t *testing.T) {
	readFailure := errors.New("injected pipe read failure")
	capture := newStreamingBashOutputCapture(4)
	reader := &dataThenErrorReader{data: []byte("abcdef\n"), err: readFailure, once: false}

	drained, err := readPipe(reader, "stdout", nil, "call-read-error", capture)
	output, truncated := capture.output()

	require.ErrorIs(t, err, readFailure)
	assert.True(t, drained, "the reader reaches EOF after the reported read error")
	assert.True(t, truncated)
	assert.Contains(t, output, "[output truncated]")
	assert.LessOrEqual(t, len(output), 4+len(streamingBashTruncatedMarker))
}

func TestReadPipeReportsFailedDrain(t *testing.T) {
	readFailure := errors.New("injected persistent pipe failure")
	capture := newStreamingBashOutputCapture(2)
	reader := &dataThenErrorReader{data: []byte("ok\n"), err: readFailure, afterErr: readFailure}

	drained, err := readPipe(reader, "stderr", nil, "call-drain-error", capture)
	output, truncated := capture.output()

	require.ErrorIs(t, err, readFailure)
	assert.False(t, drained, "an error during the post-scanner drain cannot claim EOF")
	assert.True(t, truncated)
	assert.Contains(t, output, "[output truncated]")
	assert.LessOrEqual(t, len(output), 2+len(streamingBashTruncatedMarker))
}

func TestStreamingBashCaptureCountsCRLFAndEOFSourceBytes(t *testing.T) {
	capture := newStreamingBashOutputCapture(6)
	stdoutDrained, stdoutErr := readPipe(strings.NewReader("a\r\nb"), "stdout", nil, "call-source-budget", capture)
	stderrDrained, stderrErr := readPipe(strings.NewReader("x\r\ny"), "stderr", nil, "call-source-budget", capture)

	require.NoError(t, stdoutErr)
	require.NoError(t, stderrErr)
	assert.True(t, stdoutDrained)
	assert.True(t, stderrDrained)
	assert.Equal(t, "a\r\nb", capture.stdout.String(), "CRLF delimiters and EOF tails count as their original source bytes")
	assert.Equal(t, "x\r", capture.stderr.String(), "the second stream may retain only the remaining source-byte budget")
	assert.Equal(t, 6, capture.stdout.Len()+capture.stderr.Len())
	_, truncated := capture.output()
	assert.True(t, truncated)
}

func TestStreamingBashCaptureDoesNotInventEOFNewline(t *testing.T) {
	capture := newStreamingBashOutputCapture(4)
	drained, err := readPipe(strings.NewReader("tail"), "stdout", nil, "call-eof-tail", capture)

	require.NoError(t, err)
	assert.True(t, drained)
	assert.Equal(t, "tail", capture.stdout.String())
	output, truncated := capture.output()
	assert.Equal(t, "tail", output)
	assert.False(t, truncated)
}

func TestStreamingBashCancellationWaitsForReaders(t *testing.T) {
	tool := NewStreamingBashTool(
		WithTimeout(5*time.Second),
		WithMaxOutput(1<<20),
		WithNoSandbox(),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &mockStreamSink{}
	done := make(chan error, 1)
	go func() {
		_, err := tool.ExecuteStreaming(ctx, ToolCall{
			ID:   "call-cancel-readers",
			Name: "bash",
			Args: map[string]any{"command": "while :; do printf 'x\\n'; done"},
		}, sink)
		done <- err
	}()

	deadline := time.Now().Add(time.Second)
	for len(sink.getEvents()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	require.NotEmpty(t, sink.getEvents(), "the command should start streaming before cancellation")
	started := time.Now()
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
		assert.Less(t, time.Since(started), time.Second)
		finishedEvents := len(sink.getEvents())
		time.Sleep(10 * time.Millisecond)
		assert.Equal(t, finishedEvents, len(sink.getEvents()), "reader goroutines must be joined before ExecuteStreaming returns")
	case <-time.After(time.Second):
		t.Fatal("ExecuteStreaming did not return after context cancellation")
	}
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
	tool := NewStreamingBashTool(WithNoSandbox())

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
	tool := NewStreamingBashTool(WithNoSandbox())
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
	tool := NewStreamingBashTool(WithNoSandbox())
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
