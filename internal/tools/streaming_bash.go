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

func (c *streamingBashOutputCapture) addSourceBytes(stream string, source []byte) (captured, newlyTruncated bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.truncated {
		return false, false
	}
	if len(source) == 0 {
		return true, false
	}

	target := &c.stdout
	if stream == "stderr" {
		target = &c.stderr
	}

	retainedBytes := c.stdout.Len() + c.stderr.Len()
	remaining := c.maxOutput - retainedBytes
	if remaining < len(source) {
		if remaining > 0 {
			_, _ = target.Write(source[:remaining])
		}
		c.truncated = true
		return false, true
	}

	_, _ = target.Write(source)
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
		// Validation completes synchronously before any local process helper is
		// entered, so this concrete rejection can attest the no-process path.
		reportPreparedNoProcessCompletion(ctx)
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("streaming_bash.sandbox_blocked", "tool", "bash", "err", err)
		return nil, err
	}

	// Determine the effective timeout.
	timeout := t.Timeout
	if t.TimeoutTier {
		timeout = classifyCommand(command)
	}

	timeout = runtimeBashTimeout(ctx, timeout)
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := execCtx.Err(); err != nil {
		return nil, fmt.Errorf("bash: canceled before start: %w", err)
	}

	// Wrap the command with resource limits.
	command = wrapCommandWithLimits(command, t.ResourceLimits)

	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = t.Workdir
	cmd.Env = filteredEnv(t.Env)
	if _, supplied := bashRuntimeInputFromContext(ctx); supplied {
		cmd.Stdin = runtimeBashStdin(ctx)
	}

	managed, err := startManagedLocalBashCommand(execCtx, cancel, cmd)
	if err != nil {
		return nil, fmt.Errorf("bash: start: %w", err)
	}

	maxOutput := t.MaxOutput
	if maxOutput <= 0 {
		maxOutput = defaultBashMaxOutput
	}

	capture := newStreamingBashOutputCapture(maxOutput)
	lifecycle := managed.collect(execCtx, func(reader io.Reader, stream string) (bool, error) {
		return readPipe(reader, stream, sink, call.ID, capture)
	})
	reportPreparedLocalProcessCompletion(ctx, lifecycle)
	ms := time.Since(start).Milliseconds()
	output, truncated := capture.output()
	metadata := map[string]any{
		"duration_ms":       ms,
		"truncated":         truncated,
		"containment_scope": processContainmentScope,
	}
	if lifecycle.cleanupErr != nil {
		metadata["cleanup_error"] = lifecycle.cleanupErr.Error()
	}
	readerErr := lifecycle.readerErr
	if readerErr != nil {
		metadata["read_error"] = readerErr.Error()
	}
	result := &ToolResult{Output: output, Metadata: metadata}
	runErr := lifecycle.runErr

	if lifecycle.interrupted {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		metadata["exit_code"] = -1
		message := "canceled"
		cause := error(execCtx.Err())
		if cause == nil {
			cause = context.Canceled
		}
		if errors.Is(cause, context.DeadlineExceeded) {
			message = "timed out"
			cause = fmt.Errorf("bash: timed out after %s: %w", timeout.String(), cause)
		}
		metadata["error"] = message
		logger.Error("streaming_bash.execution_interrupted",
			"tool", "bash",
			"duration_ms", ms,
			"reason", message,
			"timeout", timeout.String())
		return result, localLifecycleError(cause, lifecycle)
	}

	if !lifecycle.quiescent {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		metadata["exit_code"] = exitCode(runErr)
		cause := errors.Join(errProcessCleanupUnconfirmed, lifecycle.cleanupErr, readerErr)
		return result, pendingProcessErrorWithDone(cause, true, lifecycle.executionDone)
	}

	if lifecycle.escalated {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		metadata["exit_code"] = exitCode(runErr)
		return result, newUnknownExecutionError(errors.Join(errProcessGroupRemained, runErr))
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

// readPipe reads source-framed lines from pipe and pushes each through the StreamSink.
// Output is retained within the shared capture limit; after the limit or a
// scanner error, the remaining bytes are drained so the child can finish.
// The returned bool reports whether the pipe was drained to EOF.
func readPipe(pipe io.Reader, stream string, sink StreamSink, toolCallID string, capture *streamingBashOutputCapture) (bool, error) {
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 64*1024), streamingBashMaxLineBytes)
	scanner.Split(scanStreamingBashSourceLines)
	stopCapturing := false
	for scanner.Scan() {
		if stopCapturing {
			continue
		}
		source := scanner.Bytes()
		line := source
		if len(line) > 0 && line[len(line)-1] == '\n' {
			line = line[:len(line)-1]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
		}
		captured, newlyTruncated := capture.addSourceBytes(stream, source)
		if !captured {
			stopCapturing = true
		}
		if captured && sink != nil {
			_ = sink.Send(string(line), toolCallID, stream) //nolint:errcheck // best-effort streaming
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

func scanStreamingBashSourceLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if lineEnding := bytes.IndexByte(data, '\n'); lineEnding >= 0 {
		return lineEnding + 1, data[:lineEnding+1], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func sendTruncationMarker(sink StreamSink, toolCallID, stream string) {
	if sink != nil {
		_ = sink.Send(strings.TrimPrefix(streamingBashTruncatedMarker, "\n"), toolCallID, stream) //nolint:errcheck // best-effort streaming
	}
}
