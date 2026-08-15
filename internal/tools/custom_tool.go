package tools

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

const (
	// defaultCustomToolMaxOutput caps the amount of captured output for custom
	// command tools, mirroring the bash tool default.
	defaultCustomToolMaxOutput = 1 << 20 // 1 MiB
)

// CustomCommandTool wraps an external executable as a ToolDefinition. The
// executable and its base arguments come from command, static arguments from
// staticArgs, and the dynamic "input" argument (parsed from the tool call) is
// appended last. Environment variables, a working directory, and a timeout may
// be configured.
//
// CustomCommandTool is intentionally decoupled from the config package: it
// works with native Go types (e.g. time.Duration). The assembly layer converts
// config.CustomToolConfig into these parameters.
type CustomCommandTool struct {
	name        string
	description string
	command     []string
	staticArgs  []string
	env         map[string]string
	timeout     time.Duration
	workingDir  string
	maxOutput   int
}

var _ ToolDefinition = (*CustomCommandTool)(nil)
var _ Parameterized = (*CustomCommandTool)(nil)

// NewCustomCommandTool builds a CustomCommandTool from its configuration. The
// caller (typically the assembly layer) is responsible for validating that
// name and command are non-empty before construction. A timeout of zero means
// no timeout; an empty workingDir inherits the process working directory.
func NewCustomCommandTool(
	name, description string,
	command, staticArgs []string,
	env map[string]string,
	timeout time.Duration,
	workingDir string,
) *CustomCommandTool {
	return &CustomCommandTool{
		name:        name,
		description: description,
		command:     command,
		staticArgs:  staticArgs,
		env:         env,
		timeout:     timeout,
		workingDir:  workingDir,
		maxOutput:   defaultCustomToolMaxOutput,
	}
}

// Name returns the configured tool name.
func (t *CustomCommandTool) Name() string { return t.name }

// Description returns the configured tool description.
func (t *CustomCommandTool) Description() string { return t.description }

// Parameters returns the JSON Schema describing the tool's single "input"
// string parameter: the dynamic argument appended to the command.
func (t *CustomCommandTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"input": map[string]any{
				"type":        "string",
				"description": "The dynamic argument appended to the command.",
			},
		},
		"additionalProperties": false,
	}
}

// Execute builds and runs the configured command, capturing combined stdout and
// stderr (up to maxOutput bytes). The dynamic input argument is appended after
// the base command arguments and static args when it is non-empty. A configured
// timeout cancels the command via context. The returned ToolResult carries the
// captured output and an "exit_code" metadata field; a non-zero exit, timeout,
// or start failure is also surfaced as an error.
func (t *CustomCommandTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	input, _ := call.Args["input"].(string) //nolint:errcheck

	// Build the argument list: base command args, then static args, then the
	// dynamic input (only when non-empty).
	args := make([]string, 0, len(t.command)-1+len(t.staticArgs)+1)
	if len(t.command) > 1 {
		args = append(args, t.command[1:]...)
	}
	args = append(args, t.staticArgs...)
	if input != "" {
		args = append(args, input)
	}

	execCtx := ctx
	var cancel context.CancelFunc
	if t.timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, t.timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(execCtx, t.command[0], args...) //nolint:gosec // command from trusted config
	if t.workingDir != "" {
		cmd.Dir = t.workingDir
	}
	cmd.Env = filteredEnv(t.env)

	var buf bytes.Buffer
	limited := &limitedWriter{max: t.maxOutput, buf: &buf}
	cmd.Stdout = limited
	cmd.Stderr = limited

	start := time.Now()
	runErr := cmd.Run()
	ms := time.Since(start).Milliseconds()
	limited.truncate()

	output := buf.String()
	metadata := map[string]any{"duration_ms": ms}

	if execCtx.Err() != nil {
		metadata["exit_code"] = -1
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("custom_tool.timeout", "tool", t.name, "duration_ms", ms, "err", execCtx.Err())
		return &ToolResult{Output: output, Metadata: metadata, ToolCallID: call.ID},
			fmt.Errorf("%s: timed out after %s: %w", t.name, t.timeout, execCtx.Err())
	}

	if runErr != nil {
		exitCode := exitCode(runErr)
		metadata["exit_code"] = exitCode
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("custom_tool.exec_failed", "tool", t.name, "duration_ms", ms, "exit_code", exitCode, "err", runErr)
		return &ToolResult{Output: output, Metadata: metadata, ToolCallID: call.ID},
			fmt.Errorf("%s: %w", t.name, runErr)
	}

	metadata["exit_code"] = 0
	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("custom_tool.done", "tool", t.name, "duration_ms", ms, "output_bytes", len(output))
	return &ToolResult{Output: output, Metadata: metadata, ToolCallID: call.ID}, nil
}
