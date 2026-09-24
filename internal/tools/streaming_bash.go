package tools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

const (
	streamingBashTruncatedMarker = "\n[output truncated]"
	streamingBashMaxLineBytes    = 1 << 20
)

// StreamSink is the interface for emitting real-time tool output. It is
// satisfied by an adapter wrapping core.EventStream (the adapter lives in the
// core package to avoid a circular dependency, since core already imports
// tools). When nil, the streaming tool accumulates output without streaming.
type StreamSink interface {
	// Send pushes a single output line. toolCallID associates the line with
	// the originating tool call, and stream is "stdout" or "stderr".
	Send(content, toolCallID, stream string) error
}

// StreamingBashTool extends ToolDefinition with streaming execution. A
// streaming tool pushes output lines to a StreamSink in real time as the
// command runs, while still returning the accumulated output as a ToolResult.
type StreamingBashTool interface {
	ToolDefinition
	// ExecuteStreaming runs the command and pushes each output line through
	// the StreamSink. When sink is nil the method behaves like Execute,
	// accumulating output without streaming.
	ExecuteStreaming(ctx context.Context, call ToolCall, sink StreamSink) (*ToolResult, error)
}

// StreamingBashToolImpl embeds *BashTool to reuse all configuration
// (Name, Description, PromptGuidelines, sandbox, timeout, etc.) and adds
// pipe-based streaming execution.
type StreamingBashToolImpl struct {
	*BashTool
}

// streamingBashOutputCapture retains a bounded, combined stdout/stderr prefix.
type streamingBashOutputCapture struct {
	mu        sync.Mutex
	maxOutput int
	stdout    bytes.Buffer
	stderr    bytes.Buffer
	truncated bool
}

func newStreamingBashOutputCapture(maxOutput int) *streamingBashOutputCapture {
	return &streamingBashOutputCapture{maxOutput: maxOutput}
}

func (c *streamingBashOutputCapture) addLine(stream, line string) (captured, newlyTruncated bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.truncated {
		return false, false
	}

	target, other := &c.stdout, &c.stderr
	if stream == "stderr" {
		target, other = &c.stderr, &c.stdout
	}

	retainedBytes := c.stdout.Len() + c.stderr.Len()
	if c.stdout.Len() > 0 && c.stderr.Len() > 0 {
		retainedBytes++ // The combined output inserts one separator between streams.
	}
	lineBytes := len(line) + 1
	if target.Len() == 0 && other.Len() > 0 {
		lineBytes++ // The combined output inserts one separator between streams.
	}
	if retainedBytes+lineBytes > c.maxOutput {
		c.truncated = true
		return false, true
	}

	target.WriteString(line)
	target.WriteByte('\n')
	return true, false
}

func (c *streamingBashOutputCapture) markTruncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.truncated {
		return false
	}
	c.truncated = true
	return true
}

func (c *streamingBashOutputCapture) output() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	stdout := c.stdout.String()
	stderr := c.stderr.String()
	var output strings.Builder
	output.Grow(len(stdout) + len(stderr) + len(streamingBashTruncatedMarker) + 1)
	output.WriteString(stdout)
	if stderr != "" {
		if stdout != "" {
			output.WriteByte('\n')
		}
		output.WriteString(stderr)
	}
	if c.truncated {
		output.WriteString(streamingBashTruncatedMarker)
	}
	return output.String(), c.truncated
}

// Compile-time assertions that *StreamingBashToolImpl satisfies both
// ToolDefinition and the StreamingBashTool extension.
var (
	_ ToolDefinition    = (*StreamingBashToolImpl)(nil)
	_ StreamingBashTool = (*StreamingBashToolImpl)(nil)
)

// NewStreamingBashTool returns a StreamingBashToolImpl configured with the
// same defaults and options as BashTool.
func NewStreamingBashTool(opts ...BashToolOption) *StreamingBashToolImpl {
	return &StreamingBashToolImpl{BashTool: NewBashTool(opts...)}
}

// Execute provides backward compatibility by calling ExecuteStreaming with a
// nil StreamSink. This satisfies the ToolDefinition interface.
func (t *StreamingBashToolImpl) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	return t.ExecuteStreaming(ctx, call, nil)
}

// ExecuteStreaming runs the command with pipe-based stdout/stderr reading.
// Each line is pushed through the StreamSink. The accumulated output is
// returned as a ToolResult.
func (t *StreamingBashToolImpl) ExecuteStreaming(ctx context.Context, call ToolCall, sink StreamSink) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	start := time.Now()

	command, ok := call.Args["command"].(string)
	if !ok || strings.TrimSpace(command) == "" {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("streaming_bash.missing_command", "tool", "bash")
		return nil, errors.New("bash: missing string argument 'command'")
	}

	// A sandbox is required by default. When Sandbox is nil, return an
	// error unless the caller explicitly opted out via WithNoSandbox().
	if t.Sandbox == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("streaming_bash.sandbox_required", "tool", "bash")
		return nil, errors.New("bash: sandbox is required but not configured; use --no-sandbox to override")
	}

	// Sandbox validation.
	workDir := t.Workdir
	if absDir, absErr := filepath.Abs(t.Workdir); absErr == nil {
		workDir = absDir
	}
	if err := t.Sandbox.Validate(ctx, command, workDir); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("streaming_bash.sandbox_blocked", "tool", "bash", "err", err)
		return nil, err
	}

	// Determine the effective timeout.
	timeout := t.Timeout
	if t.TimeoutTier {
		timeout = classifyCommand(command)
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Wrap the command with resource limits.
	command = wrapCommandWithLimits(command, t.ResourceLimits)

	cmd := exec.CommandContext(execCtx, "bash", "-lc", command)
	cmd.Dir = t.Workdir
	cmd.Env = filteredEnv(t.Env)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("bash: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("bash: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("bash: start: %w", err)
	}

	maxOutput := t.MaxOutput
	if maxOutput <= 0 {
		maxOutput = defaultBashMaxOutput
	}

	capture := newStreamingBashOutputCapture(maxOutput)
	var stdoutDrained, stderrDrained bool
	var stdoutReadErr, stderrReadErr error

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		stdoutDrained, stdoutReadErr = readPipe(stdoutPipe, "stdout", sink, call.ID, capture)
		if !stdoutDrained {
			cancel()
		}
	}()
	go func() {
		defer wg.Done()
		stderrDrained, stderrReadErr = readPipe(stderrPipe, "stderr", sink, call.ID, capture)
		if !stderrDrained {
			cancel()
		}
	}()
	wg.Wait()

	runErr := cmd.Wait()
	ms := time.Since(start).Milliseconds()
	output, truncated := capture.output()
	metadata := map[string]any{
		"duration_ms": ms,
		"truncated":   truncated,
	}
	readerErr := errors.Join(stdoutReadErr, stderrReadErr)
	if readerErr != nil {
		metadata["read_error"] = readerErr.Error()
	}
	result := &ToolResult{Output: output, Metadata: metadata}

	if ctx.Err() != nil || errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		metadata["exit_code"] = -1
		metadata["error"] = "timed out"
		logger.Error("streaming_bash.timeout",
			"tool", "bash",
			"duration_ms", ms,
			"timeout", timeout.String())
		return result, fmt.Errorf("bash: timed out after %s: %w", timeout.String(), execCtx.Err())
	}

	if readerErr != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		if runErr != nil {
			metadata["exit_code"] = exitCode(runErr)
		} else {
			metadata["exit_code"] = 0
		}
		logger.Error("streaming_bash.read_failed",
			"tool", "bash",
			"duration_ms", ms,
			"err", readerErr)
		return result, fmt.Errorf("bash: output read failed: %w", readerErr)
	}

	if runErr != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		exitCode := exitCode(runErr)
		metadata["exit_code"] = exitCode
		logger.Error("streaming_bash.exec_failed",
			"tool", "bash",
			"duration_ms", ms,
			"exit_code", exitCode,
			"err", runErr)
		return result, fmt.Errorf("bash: %w", runErr)
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	if code, ok := processExitCode(runErr); ok {
		metadata["exit_code"] = code
	} else {
		metadata["exit_code"] = 0
	}
	logger.Info("streaming_bash.exec",
		"tool", "bash",
		"duration_ms", ms,
		"output_bytes", len(output),
		"truncated", truncated)

	return result, nil
}

// readPipe reads lines from pipe and pushes each through the StreamSink.
// Output is retained within the shared capture limit; after the limit or a
// scanner error, the remaining bytes are drained so the child can finish.
// The returned bool reports whether the pipe was drained to EOF.
func readPipe(pipe io.Reader, stream string, sink StreamSink, toolCallID string, capture *streamingBashOutputCapture) (bool, error) {
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 64*1024), streamingBashMaxLineBytes)
	stopCapturing := false
	for scanner.Scan() {
		if stopCapturing {
			continue
		}
		line := scanner.Text()
		captured, newlyTruncated := capture.addLine(stream, line)
		if !captured {
			stopCapturing = true
		}
		if captured && sink != nil {
			_ = sink.Send(line, toolCallID, stream) //nolint:errcheck // best-effort streaming
		}
		if newlyTruncated {
			sendTruncationMarker(sink, toolCallID, stream)
		}
	}

	if scanErr := scanner.Err(); scanErr != nil {
		if capture.markTruncated() {
			sendTruncationMarker(sink, toolCallID, stream)
		}
		_, drainErr := io.Copy(io.Discard, pipe)
		readerErr := fmt.Errorf("bash: %s output scanner: %w", stream, scanErr)
		if drainErr != nil {
			readerErr = errors.Join(readerErr, fmt.Errorf("bash: %s output drain: %w", stream, drainErr))
			return false, readerErr
		}
		return true, readerErr
	}

	return true, nil
}

func sendTruncationMarker(sink StreamSink, toolCallID, stream string) {
	if sink != nil {
		_ = sink.Send(strings.TrimPrefix(streamingBashTruncatedMarker, "\n"), toolCallID, stream) //nolint:errcheck // best-effort streaming
	}
}
