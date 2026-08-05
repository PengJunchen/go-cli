package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// RemoteBashToolOption configures a RemoteBashTool.
type RemoteBashToolOption func(*RemoteBashTool)

// WithRemoteBashSandbox attaches a BashSandbox that validates commands before
// remote execution. When set, Execute calls Validate before running the command
// and returns the error without executing if validation fails.
func WithRemoteBashSandbox(sb BashSandbox) RemoteBashToolOption {
	return func(t *RemoteBashTool) { t.sandbox = sb }
}

// WithRemoteBashMaxOutput caps the number of bytes of captured output.
func WithRemoteBashMaxOutput(n int) RemoteBashToolOption {
	return func(t *RemoteBashTool) { t.maxOutput = n }
}

// WithRemoteBashTimeout sets the execution timeout.
func WithRemoteBashTimeout(d time.Duration) RemoteBashToolOption {
	return func(t *RemoteBashTool) { t.timeout = d }
}

// WithRemoteBashHosts registers additional SSH clients keyed by host name so
// the host argument can select a different remote target.
func WithRemoteBashHosts(hosts map[string]SSHClient) RemoteBashToolOption {
	return func(t *RemoteBashTool) {
		if t.hostClients == nil {
			t.hostClients = map[string]SSHClient{}
		}
		for k, v := range hosts {
			t.hostClients[k] = v
		}
	}
}

// RemoteBashTool executes shell commands on a remote host via SSH. It
// implements the ToolDefinition interface. Remote execution is high-risk, so
// the tool is marked as [REQUIRES APPROVAL] in its description.
type RemoteBashTool struct {
	client      SSHClient
	hostClients map[string]SSHClient
	sandbox     BashSandbox
	maxOutput   int
	timeout     time.Duration
}

var _ ToolDefinition = (*RemoteBashTool)(nil)
var _ Parameterized = (*RemoteBashTool)(nil)

// NewRemoteBashTool returns a RemoteBashTool backed by the given SSHClient.
// The default client is used when the host argument is omitted. Additional
// clients may be registered with WithRemoteBashHosts.
func NewRemoteBashTool(client SSHClient, opts ...RemoteBashToolOption) *RemoteBashTool {
	t := &RemoteBashTool{
		client:    client,
		maxOutput: defaultBashMaxOutput,
		timeout:   defaultBashTimeout,
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Name returns the tool name.
func (t *RemoteBashTool) Name() string { return "remote_bash" }

// Description returns a brief description of the tool.
func (t *RemoteBashTool) Description() string {
	return "remote_bash: [REQUIRES APPROVAL] runs a shell command on a remote host via SSH. " +
		"Args: command (string, required), host (string, optional - uses default from config)."
}

// Parameters returns the JSON Schema describing the tool's input parameters.
func (t *RemoteBashTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The shell command to execute on the remote host.",
			},
			"host": map[string]any{
				"type":        "string",
				"description": "Optional host name. Uses the default configured host when omitted.",
			},
		},
		"required": []string{"command"},
	}
}

// Execute runs the given command on the remote host. It validates the command
// against the sandbox (if configured), selects the appropriate SSH client
// based on the optional host argument, and returns captured stdout/stderr/exit
// code in the result metadata.
func (t *RemoteBashTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	start := time.Now()

	command, ok := call.Args["command"].(string)
	if !ok || strings.TrimSpace(command) == "" {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("remote_bash.missing_command", "tool", "remote_bash")
		return nil, errors.New("remote_bash: missing string argument 'command'")
	}

	// Select the SSH client: use a named host if provided, otherwise the default.
	hostName, _ := call.Args["host"].(string)
	client := t.client
	if hostName != "" {
		if c, ok := t.hostClients[hostName]; ok {
			client = c
		} else {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			logger.Error("remote_bash.unknown_host", "tool", "remote_bash", "host", hostName)
			return nil, fmt.Errorf("remote_bash: unknown host %q", hostName)
		}
	}

	if client == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, errors.New("remote_bash: no SSH client configured")
	}

	// Validate the command against the sandbox (if configured). An empty
	// workDir is passed because remote execution has no local working
	// directory; sandboxes with an empty path whitelist allow all paths.
	if t.sandbox != nil {
		if err := t.sandbox.Validate(ctx, command, ""); err != nil {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			logger.Error("remote_bash.sandbox_blocked", "tool", "remote_bash", "err", err)
			return nil, err
		}
	}

	// Apply the tool's timeout.
	execCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	stdout, stderr, exitCode, execErr := client.Exec(execCtx, command)
	ms := time.Since(start).Milliseconds()

	// Build the combined output with the limitedWriter pattern (reused from
	// bash.go) to cap the amount of buffered data.
	var buf bytes.Buffer
	limited := &limitedWriter{max: t.maxOutput, buf: &buf}
	limited.Write([]byte(stdout))
	if stderr != "" {
		limited.Write([]byte(stderr))
	}
	limited.truncate()
	output := buf.String()

	metadata := map[string]any{
		"duration_ms": ms,
		"exit_code":   exitCode,
		"stdout":      stdout,
		"stderr":      stderr,
	}
	if hostName != "" {
		metadata["host"] = hostName
	}

	if execCtx.Err() != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		metadata["error"] = "timed out"
		logger.Error("remote_bash.timeout",
			"tool", "remote_bash",
			"duration_ms", ms,
			"timeout", t.timeout.String())
		return &ToolResult{Output: output, Metadata: metadata, ToolCallID: call.ID},
			fmt.Errorf("remote_bash: timed out after %s: %w", t.timeout.String(), execCtx.Err())
	}

	if execErr != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("remote_bash.exec_failed",
			"tool", "remote_bash",
			"duration_ms", ms,
			"exit_code", exitCode,
			"err", execErr)
		return &ToolResult{Output: output, Metadata: metadata, ToolCallID: call.ID},
			fmt.Errorf("remote_bash: %w", execErr)
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("remote_bash.exec",
		"tool", "remote_bash",
		"duration_ms", ms,
		"exit_code", exitCode,
		"output_bytes", len(output))

	return &ToolResult{Output: output, Metadata: metadata, ToolCallID: call.ID}, nil
}
