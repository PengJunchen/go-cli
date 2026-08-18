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

	var stdoutBuf, stderrBuf bytes.Buffer

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		readPipe(stdoutPipe, "stdout", sink, call.ID, &stdoutBuf, maxOutput)
	}()
	go func() {
		defer wg.Done()
		readPipe(stderrPipe, "stderr", sink, call.ID, &stderrBuf, maxOutput)
	}()
	wg.Wait()

	runErr := cmd.Wait()
	ms := time.Since(start).Milliseconds()

	// Build output: stdout first, then stderr with a separator if both are
	// non-empty.
	output := stdoutBuf.String()
	if stderrBuf.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += stderrBuf.String()
	}

	metadata := map[string]any{"duration_ms": ms}

	if execCtx.Err() != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		metadata["exit_code"] = -1
		metadata["error"] = "timed out"
		logger.Error("streaming_bash.timeout",
			"tool", "bash",
			"duration_ms", ms,
			"timeout", timeout.String())
		return &ToolResult{Output: output, Metadata: metadata}, fmt.Errorf("bash: timed out after %s: %w", timeout.String(), execCtx.Err())
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
		return &ToolResult{Output: output, Metadata: metadata}, fmt.Errorf("bash: %w", runErr)
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
		"output_bytes", len(output))

	return &ToolResult{Output: output, Metadata: metadata}, nil
}

// readPipe reads lines from pipe and pushes each through the StreamSink.
// Output is accumulated in buf up to maxOutput bytes; when the limit is
// exceeded a "[output truncated]" marker is appended and reading stops.
func readPipe(pipe io.Reader, stream string, sink StreamSink, toolCallID string, buf *bytes.Buffer, maxOutput int) {
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 1 MiB max line
	for scanner.Scan() {
		line := scanner.Text()
		if buf.Len()+len(line)+1 > maxOutput {
			buf.WriteString("\n[output truncated]")
			break
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
		if sink != nil {
			_ = sink.Send(line, toolCallID, stream) //nolint:errcheck // best-effort streaming
		}
	}
}
