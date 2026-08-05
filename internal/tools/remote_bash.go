package tools

import (
	"context"
	"errors"
	"time"
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

// Execute runs the given command on the remote host. (stub)
func (t *RemoteBashTool) Execute(_ context.Context, _ ToolCall) (*ToolResult, error) {
	return nil, errors.New("remote_bash: not implemented")
}
