package tools

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamingBashLongLineRetainsSourcePrefix(t *testing.T) {
	const limit = 128
	payload := "diagnostic: keep this useful prefix " + strings.Repeat("x", 1_100_000)
	path := filepath.Join(t.TempDir(), "long-output")
	require.NoError(t, os.WriteFile(path, []byte(payload), 0o600))

	for _, tc := range []struct {
		name     string
		stream   string
		withSink bool
		exit     int
	}{
		{name: "stdout nil sink", stream: "stdout"},
		{name: "stdout sink", stream: "stdout", withSink: true},
		{name: "stderr nil sink", stream: "stderr"},
		{name: "stderr sink", stream: "stderr", withSink: true},
		{name: "nonzero exit", stream: "stdout", withSink: true, exit: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := "cat " + shellQuoteTestArgument(path)
			if tc.stream == "stderr" {
				command += " >&2"
			}
			if tc.exit != 0 {
				command += "; exit 7"
			}
			var sink StreamSink
			var events *mockStreamSink
			if tc.withSink {
				events = &mockStreamSink{}
				sink = events
			}
			tool := NewStreamingBashTool(WithMaxOutput(limit), WithTimeout(5*time.Second), WithNoSandbox())
			result, err := tool.ExecuteStreaming(context.Background(), ToolCall{
				ID: "long-line", Name: "bash", Args: map[string]any{"command": command},
			}, sink)
			if tc.exit == 0 {
				require.NoError(t, err)
			} else {
				var exitErr *exec.ExitError
				require.ErrorAs(t, err, &exitErr)
				assert.Equal(t, tc.exit, exitErr.ExitCode())
			}
			require.NotNil(t, result)
			assert.Equal(t, payload[:limit]+streamingBashTruncatedMarker, result.Output)
			assert.Equal(t, tc.exit, result.Metadata["exit_code"])
			assert.Equal(t, true, result.Metadata["truncated"])
			assert.NotContains(t, result.Metadata, "read_error")
			if events != nil {
				assert.Equal(t, []streamEvent{{Content: "[output truncated]", ToolCallID: "long-line", Stream: tc.stream}}, events.getEvents())
			}
		})
	}
}

func TestReadPipeDrainsLongLineAfterExactLimit(t *testing.T) {
	const first = "head\n"
	capture := newStreamingBashOutputCapture(len(first))
	sink := &mockStreamSink{}
	drained, err := readPipe(strings.NewReader(first+strings.Repeat("x", 1_100_000)), "stdout", sink, "after-limit", capture)
	require.NoError(t, err)
	require.True(t, drained)
	output, truncated := capture.output()
	assert.Equal(t, first+streamingBashTruncatedMarker, output)
	assert.True(t, truncated)
	assert.Equal(t, []streamEvent{
		{Content: "head", ToolCallID: "after-limit", Stream: "stdout"},
		{Content: "[output truncated]", ToolCallID: "after-limit", Stream: "stdout"},
	}, sink.getEvents())
}

func TestReadPipeReportsErrorAfterLongLineTruncation(t *testing.T) {
	readErr := errors.New("failure after long output")
	reader := io.MultiReader(strings.NewReader(strings.Repeat("x", 1_100_000)), &dataThenErrorReader{err: readErr})
	capture := newStreamingBashOutputCapture(128)
	drained, err := readPipe(reader, "stdout", nil, "long-error", capture)
	require.ErrorIs(t, err, readErr)
	assert.True(t, drained)
	output, truncated := capture.output()
	assert.Equal(t, strings.Repeat("x", 128)+streamingBashTruncatedMarker, output)
	assert.True(t, truncated)
}

type gatedEOFReader struct {
	source  *strings.Reader
	reached chan struct{}
	release chan struct{}
}

func (r *gatedEOFReader) Read(p []byte) (int, error) {
	if r.source.Len() > 0 {
		return r.source.Read(p)
	}
	close(r.reached)
	<-r.release
	return 0, io.EOF
}

func TestReadPipeDeliversCapturedLineWhenOtherStreamTruncates(t *testing.T) {
	const size = 64 * 1024
	line := strings.Repeat("a", size)
	reader := &gatedEOFReader{source: strings.NewReader(line), reached: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	capture := newStreamingBashOutputCapture(size)
	sink := &mockStreamSink{}
	type readResult struct {
		drained bool
		err     error
	}
	done := make(chan readResult, 1)
	go func() {
		drained, err := readPipe(reader, "stdout", sink, "shared-limit", capture)
		done <- readResult{drained: drained, err: err}
		close(done)
	}()
	defer func() {
		release.Do(func() { close(reader.release) })
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("stdout reader did not join during cleanup")
		}
	}()
	select {
	case <-reader.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("stdout did not reach the controlled EOF boundary")
	}
	output, truncated := capture.output()
	require.Equal(t, line, output)
	require.False(t, truncated)

	drained, err := readPipe(strings.NewReader("x"), "stderr", sink, "shared-limit", capture)
	require.NoError(t, err)
	require.True(t, drained)
	release.Do(func() { close(reader.release) })
	select {
	case result := <-done:
		require.NoError(t, result.err)
		require.True(t, result.drained)
	case <-time.After(5 * time.Second):
		t.Fatal("stdout did not finish after EOF was released")
	}
	events := sink.getEvents()
	require.Len(t, events, 2, "a complete captured line remains deliverable after the other stream truncates")
	assert.Equal(t, streamEvent{Content: "[output truncated]", ToolCallID: "shared-limit", Stream: "stderr"}, events[0])
	assert.Equal(t, "stdout", events[1].Stream)
	assert.Equal(t, "shared-limit", events[1].ToolCallID)
	if events[1].Content != line {
		t.Fatalf("captured line changed: got %d bytes, want %d", len(events[1].Content), len(line))
	}
}

func TestStreamingBashLongLineWithinLimitIsOneCompleteEvent(t *testing.T) {
	// Place the multi-byte characters across the reader's fixed-size boundary.
	line := strings.Repeat("a", 65535) + "中😀" + strings.Repeat("b", 1_050_000)
	payload := line + "\r\n"
	path := filepath.Join(t.TempDir(), "full-line")
	require.NoError(t, os.WriteFile(path, []byte(payload), 0o600))
	sink := &mockStreamSink{}
	tool := NewStreamingBashTool(WithMaxOutput(len(payload)), WithTimeout(5*time.Second), WithNoSandbox())
	result, err := tool.ExecuteStreaming(context.Background(), ToolCall{
		ID: "full-line", Name: "bash", Args: map[string]any{"command": "cat " + shellQuoteTestArgument(path)},
	}, sink)
	require.NoError(t, err)
	require.NotNil(t, result)
	if result.Output != payload {
		t.Fatalf("untruncated source changed: got %d bytes, want %d", len(result.Output), len(payload))
	}
	assert.Equal(t, false, result.Metadata["truncated"])
	assert.Equal(t, 0, result.Metadata["exit_code"])
	assert.NotContains(t, result.Metadata, "read_error")
	events := sink.getEvents()
	require.Len(t, events, 1)
	if events[0].Content != line {
		t.Fatalf("line was split or changed: got %d bytes, want %d", len(events[0].Content), len(line))
	}
	assert.Equal(t, "full-line", events[0].ToolCallID)
	assert.Equal(t, "stdout", events[0].Stream)
}

func TestReadPipeLineBoundariesAndExactSourceLimit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		source    string
		limit     int
		wantEvent string
		truncated bool
	}{
		{name: "EOF exactly at limit", source: "tail", limit: 4, wantEvent: "tail"},
		{name: "newline exactly at limit", source: "tail\n", limit: 5, wantEvent: "tail"},
		{name: "CRLF exactly at limit", source: "tail\r\n", limit: 6, wantEvent: "tail"},
		{name: "EOF bare CR", source: "tail\r", limit: 5, wantEvent: "tail\r"},
		{name: "one byte beyond limit", source: "tails", limit: 4, wantEvent: "[output truncated]", truncated: true},
		{name: "CRLF across read boundary", source: strings.Repeat("a", 65535) + "\r\n", limit: 65537, wantEvent: strings.Repeat("a", 65535)},
		{name: "EOF on read boundary", source: strings.Repeat("a", 65536), limit: 65536, wantEvent: strings.Repeat("a", 65536)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := newStreamingBashOutputCapture(tc.limit)
			sink := &mockStreamSink{}
			drained, err := readPipe(strings.NewReader(tc.source), "stdout", sink, "boundary", capture)
			require.NoError(t, err)
			require.True(t, drained)
			output, truncated := capture.output()
			want := tc.source
			if tc.truncated {
				want = tc.source[:tc.limit] + streamingBashTruncatedMarker
			}
			if output != want {
				t.Fatalf("source capture mismatch: got %d bytes, want %d", len(output), len(want))
			}
			assert.Equal(t, tc.truncated, truncated)
			events := sink.getEvents()
			require.Len(t, events, 1)
			if events[0].Content != tc.wantEvent {
				t.Fatalf("stream event mismatch: got %d bytes, want %d", len(events[0].Content), len(tc.wantEvent))
			}
			assert.Equal(t, "stdout", events[0].Stream)
			assert.Equal(t, "boundary", events[0].ToolCallID)
		})
	}
}

func TestStreamingBashLongLinesDrainBothStreamsAfterBudget(t *testing.T) {
	const limit = 128
	path := filepath.Join(t.TempDir(), "both-output")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", 1_100_000)), 0o600))
	cat := "cat " + shellQuoteTestArgument(path)
	command := cat + " & first=$!; " + cat + " >&2 & second=$!; wait \"$first\" && wait \"$second\""
	sink := &mockStreamSink{}
	tool := NewStreamingBashTool(WithMaxOutput(limit), WithTimeout(5*time.Second), WithNoSandbox())
	result, err := tool.ExecuteStreaming(context.Background(), ToolCall{
		ID: "both", Name: "bash", Args: map[string]any{"command": command},
	}, sink)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, limit, strings.Count(result.Output, "x"))
	assert.Equal(t, 1, strings.Count(result.Output, "[output truncated]"))
	assert.LessOrEqual(t, len(result.Output), limit+1+len(streamingBashTruncatedMarker))
	assert.Equal(t, true, result.Metadata["truncated"])
	assert.Equal(t, 0, result.Metadata["exit_code"])
	assert.NotContains(t, result.Metadata, "read_error")
	events := sink.getEvents()
	require.Len(t, events, 1)
	assert.Equal(t, "[output truncated]", events[0].Content)
	assert.Equal(t, "both", events[0].ToolCallID)
}

type longLineFailingSink struct {
	called int
}

func (s *longLineFailingSink) Send(_, _, _ string) error {
	s.called++
	return errors.New("sink unavailable")
}

func TestStreamingBashLongLineSinkFailureRemainsBestEffort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sink-output")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", 1_100_000)), 0o600))
	sink := &longLineFailingSink{}
	tool := NewStreamingBashTool(WithMaxOutput(128), WithTimeout(5*time.Second), WithNoSandbox())
	result, err := tool.ExecuteStreaming(context.Background(), ToolCall{
		ID: "sink-failure", Name: "bash", Args: map[string]any{"command": "cat " + shellQuoteTestArgument(path)},
	}, sink)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, strings.Repeat("x", 128)+streamingBashTruncatedMarker, result.Output)
	assert.Equal(t, 0, result.Metadata["exit_code"])
	assert.Equal(t, true, result.Metadata["truncated"])
	assert.NotContains(t, result.Metadata, "read_error")
	assert.Equal(t, 1, sink.called)
}
