package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

const (
	// defaultBashTimeout is the default execution timeout when none is
	// configured.
	defaultBashTimeout = 30 * time.Second

	// defaultBashMaxOutput caps the amount of captured output.
	defaultBashMaxOutput = 1 << 20 // 1 MiB

	// defaultBashWorkdir is the working directory used when none is configured.
	defaultBashWorkdir = "."
)

// BashToolOption configures a BashTool.
type BashToolOption func(*BashTool)

// WithTimeout sets the command execution timeout.
func WithTimeout(d time.Duration) BashToolOption {
	return func(t *BashTool) { t.Timeout = d }
}

// WithEnv merges additional environment variables into the command's
// environment.
func WithEnv(env map[string]string) BashToolOption {
	return func(t *BashTool) {
		if t.Env == nil {
			t.Env = map[string]string{}
		}
		for k, v := range env {
			t.Env[k] = v
		}
	}
}

// WithBashWorkdir sets the working directory for executed commands.
func WithBashWorkdir(dir string) BashToolOption {
	return func(t *BashTool) { t.Workdir = dir }
}

// WithMaxOutput caps the number of bytes of captured output.
func WithMaxOutput(n int) BashToolOption {
	return func(t *BashTool) { t.MaxOutput = n }
}

// WithBashSandbox attaches a BashSandbox that validates commands before
// execution. When set, Execute calls Validate before running the command and
// returns the error without executing if validation fails. When unset (the
// default) no validation is performed, preserving backward compatibility.
func WithBashSandbox(sb BashSandbox) BashToolOption {
	return func(t *BashTool) { t.Sandbox = sb }
}

// BashTool executes shell commands and returns their combined output. It
// implements the ToolDefinition interface.
type BashTool struct {
	// Timeout bounds how long a command may run before it is canceled.
	Timeout time.Duration
	// Env holds additional environment variables applied on top of the
	// process environment.
	Env map[string]string
	// Workdir is the directory the command runs in.
	Workdir string
	// MaxOutput caps the number of bytes of captured output.
	MaxOutput int
	// Sandbox, when non-nil, validates commands before execution.
	Sandbox BashSandbox
}

var _ ToolDefinition = (*BashTool)(nil)

// NewBashTool returns a BashTool with a default timeout of 30s. Options may
// override the defaults.
func NewBashTool(opts ...BashToolOption) *BashTool {
	t := &BashTool{
		Timeout:   defaultBashTimeout,
		Workdir:   defaultBashWorkdir,
		MaxOutput: defaultBashMaxOutput,
		Env:       map[string]string{},
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Name returns the tool name.
func (t *BashTool) Name() string { return "bash" }

// Description returns a brief description of the tool.
func (t *BashTool) Description() string {
	return "bash: runs a shell command and returns its combined output. Args: command (string)."
}

// Execute runs the given shell command under the tool's timeout and captures
// combined stdout+stderr. A command that exceeds the timeout is killed and an
// error is returned.
func (t *BashTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	start := time.Now()

	command, ok := call.Args["command"].(string)
	if !ok || strings.TrimSpace(command) == "" {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("bash.missing_command", "tool", "bash")
		return nil, errors.New("bash: missing string argument 'command'")
	}

	// When a sandbox is configured, validate the command before execution.
	if t.Sandbox != nil {
		if err := t.Sandbox.Validate(ctx, command, t.Workdir); err != nil {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			logger.Error("bash.sandbox_blocked", "tool", "bash", "err", err)
			return nil, err
		}
	}

	execCtx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, "bash", "-lc", command)
	cmd.Dir = t.Workdir
	cmd.Env = append(os.Environ(), envSlice(t.Env)...)

	var buf bytes.Buffer
	// Bound the amount of data buffered so hostile or noisy output cannot
	// exhaust memory.
	limited := &limitedWriter{max: t.MaxOutput, buf: &buf}
	cmd.Stdout = limited
	cmd.Stderr = limited

	runErr := cmd.Run()
	ms := time.Since(start).Milliseconds()
	limited.truncate()

	output := buf.String()
	metadata := map[string]any{"duration_ms": ms}

	if execCtx.Err() != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		metadata["exit_code"] = -1
		metadata["error"] = "timed out"
		logger.Error("bash.timeout",
			"tool", "bash",
			"duration_ms", ms,
			"timeout", t.Timeout.String())
		return &ToolResult{Output: output, Metadata: metadata}, fmt.Errorf("bash: timed out after %s: %w", t.Timeout.String(), execCtx.Err())
	}

	if runErr != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		exitCode := exitCode(runErr)
		metadata["exit_code"] = exitCode
		logger.Error("bash.exec_failed",
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
	logger.Info("bash.exec",
		"tool", "bash",
		"duration_ms", ms,
		"output_bytes", len(output))

	return &ToolResult{Output: output, Metadata: metadata}, nil
}

// PromptGuidelines returns usage hints for the bash tool.
func (t *BashTool) PromptGuidelines() []string {
	return []string{"Use bash for shell commands. Avoid using cat/sed/grep - use read/edit/grep tools instead"}
}

// envSlice converts the tool's environment map to "K=V" entries suitable for
// cmd.Env.
func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// exitCode extracts a process exit code from a command run error, defaulting
// to -1 when the error is not a generic exit error.
func exitCode(err error) int {
	if code, ok := processExitCode(err); ok {
		return code
	}
	return -1
}

// processExitCode extracts the numeric exit code from an *exec.ExitError.
func processExitCode(err error) (int, bool) {
	var ee *exec.ExitError
	if ok := errors.As(err, &ee); ok {
		return ee.ExitCode(), true
	}
	return 0, false
}

// limitedWriter buffers output up to max bytes and discards anything beyond it.
type limitedWriter struct {
	max  int
	buf  *bytes.Buffer
	over bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	remaining := w.max - w.buf.Len()
	if remaining <= 0 {
		w.over = true
		return len(p), nil
	}
	if len(p) > remaining {
		w.buf.Write(p[:remaining])
		w.over = true
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

// truncate appends a marker when output was truncated.
func (w *limitedWriter) truncate() {
	if w.over {
		w.buf.WriteString("\n[output truncated]")
	}
}
