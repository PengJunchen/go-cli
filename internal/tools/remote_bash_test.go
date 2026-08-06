package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// ---------------------------------------------------------------------------
// Mock SSHClient
// ---------------------------------------------------------------------------

// mockSSHClient is a test double for SSHClient. It records calls and returns
// configurable results.
type mockSSHClient struct {
	connectErr error
	execFn     func(ctx context.Context, command string) (string, string, int, error)
	closeErr   error
	execCalls  []string
	closeCount int
}

func (m *mockSSHClient) Connect(_ context.Context) error {
	return m.connectErr
}

func (m *mockSSHClient) Exec(ctx context.Context, command string) (string, string, int, error) {
	m.execCalls = append(m.execCalls, command)
	if m.execFn != nil {
		return m.execFn(ctx, command)
	}
	return "", "", 0, nil
}

func (m *mockSSHClient) Close() error {
	m.closeCount++
	return m.closeErr
}

var _ SSHClient = (*mockSSHClient)(nil)

// ---------------------------------------------------------------------------
// SSHConfig / DefaultSSHClient tests
// ---------------------------------------------------------------------------

func TestSSHConfigEmptyHost(t *testing.T) {
	client := NewDefaultSSHClient(SSHConfig{})
	err := client.Connect(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host")
}

func TestSSHConfigNoAuthMethod(t *testing.T) {
	client := NewDefaultSSHClient(SSHConfig{Host: "example.com"})
	err := client.Connect(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key_path")
	assert.Contains(t, err.Error(), "password")
}

func TestSSHConfigKeyPathPassesValidation(t *testing.T) {
	client := NewDefaultSSHClient(SSHConfig{
		Host:    "example.com",
		KeyPath: "/tmp/fake_key",
	})
	err := client.Connect(context.Background())
	require.NoError(t, err)
}

func TestSSHConfigPasswordPassesValidation(t *testing.T) {
	// Password auth validation does not require sshpass to be installed;
	// the check is deferred to Exec. Connect only verifies the field is set.
	client := NewDefaultSSHClient(SSHConfig{
		Host:     "example.com",
		Password: "secret",
	})
	err := client.Connect(context.Background())
	// sshpass may or may not be installed; either way the config itself
	// is valid. If sshpass is missing we get a specific error.
	if err != nil {
		assert.Contains(t, err.Error(), "sshpass")
	}
}

func TestDefaultSSHClientCloseIsNoOp(t *testing.T) {
	client := NewDefaultSSHClient(SSHConfig{
		Host:    "example.com",
		KeyPath: "/tmp/fake_key",
	})
	err := client.Close()
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// RemoteBashTool metadata tests
// ---------------------------------------------------------------------------

func TestRemoteBashToolName(t *testing.T) {
	tool := NewRemoteBashTool(&mockSSHClient{})
	assert.Equal(t, "remote_bash", tool.Name())
}

func TestRemoteBashToolDescription(t *testing.T) {
	tool := NewRemoteBashTool(&mockSSHClient{})
	desc := tool.Description()
	assert.Contains(t, desc, "remote_bash")
	assert.Contains(t, desc, "REQUIRES APPROVAL")
}

func TestRemoteBashToolParameters(t *testing.T) {
	tool := NewRemoteBashTool(&mockSSHClient{})
	params := tool.Parameters()
	m, ok := params.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "object", m["type"])

	props, ok := m["properties"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, props, "command")
	assert.Contains(t, props, "host")

	required, ok := m["required"].([]string)
	require.True(t, ok)
	assert.Contains(t, required, "command")
}

// ---------------------------------------------------------------------------
// RemoteBashTool Execute tests
// ---------------------------------------------------------------------------

func TestRemoteBashToolExecuteSuccess(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mc := &mockSSHClient{
		execFn: func(_ context.Context, cmd string) (string, string, int, error) {
			assert.Equal(t, "ls -la", cmd)
			return "total 0\n", "", 0, nil
		},
	}
	tool := NewRemoteBashTool(mc)

	res, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-1",
		Args: map[string]any{"command": "ls -la"},
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, "call-1", res.ToolCallID)
	assert.Equal(t, "total 0\n", res.Output)
	assert.Equal(t, 0, res.Metadata["exit_code"])
	assert.Equal(t, "total 0\n", res.Metadata["stdout"])
}

func TestRemoteBashToolExecuteWithStderr(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mc := &mockSSHClient{
		execFn: func(_ context.Context, _ string) (string, string, int, error) {
			return "out line\n", "err line\n", 0, nil
		},
	}
	tool := NewRemoteBashTool(mc)

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "echo test"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "out line")
	assert.Contains(t, res.Output, "err line")
	assert.Equal(t, "out line\n", res.Metadata["stdout"])
	assert.Equal(t, "err line\n", res.Metadata["stderr"])
	assert.Equal(t, 0, res.Metadata["exit_code"])
}

func TestRemoteBashToolExecuteNonZeroExit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mc := &mockSSHClient{
		execFn: func(_ context.Context, _ string) (string, string, int, error) {
			return "", "command not found\n", 127, nil
		},
	}
	tool := NewRemoteBashTool(mc)

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "nonexistent"},
	})
	// Non-zero exit code is not an error at the SSH transport level.
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 127, res.Metadata["exit_code"])
}

func TestRemoteBashToolExecuteTransportError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mc := &mockSSHClient{
		execFn: func(_ context.Context, _ string) (string, string, int, error) {
			return "", "", -1, errors.New("connection refused")
		},
	}
	tool := NewRemoteBashTool(mc)

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "echo hi"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
	// Result may still be returned with partial data.
	if res != nil {
		assert.Equal(t, -1, res.Metadata["exit_code"])
	}
}

func TestRemoteBashToolMissingCommand(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewRemoteBashTool(&mockSSHClient{})

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "command")

	// Non-string command.
	_, err = tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": 42},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "command")

	// Empty/whitespace command.
	_, err = tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "   "},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "command")
}

// ---------------------------------------------------------------------------
// Sandbox validation tests
// ---------------------------------------------------------------------------

func TestRemoteBashToolSandboxBlocksCommand(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mc := &mockSSHClient{}
	tool := NewRemoteBashTool(mc, WithRemoteBashSandbox(
		NewDefaultBashSandbox(WithBlacklist(defaultCommandBlacklist)),
	))

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "rm -rf /"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blacklisted")
	// The command should never have been sent to the remote host.
	assert.Empty(t, mc.execCalls)
}

func TestRemoteBashToolSandboxAllowsSafeCommand(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mc := &mockSSHClient{
		execFn: func(_ context.Context, _ string) (string, string, int, error) {
			return "ok\n", "", 0, nil
		},
	}
	tool := NewRemoteBashTool(mc, WithRemoteBashSandbox(
		NewDefaultBashSandbox(WithBlacklist(defaultCommandBlacklist)),
	))

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "ls -la"},
	})
	require.NoError(t, err)
	assert.Equal(t, "ok\n", res.Output)
	assert.Len(t, mc.execCalls, 1)
}

func TestRemoteBashToolWithoutSandboxExecutesAll(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mc := &mockSSHClient{
		execFn: func(_ context.Context, _ string) (string, string, int, error) {
			return "done\n", "", 0, nil
		},
	}
	tool := NewRemoteBashTool(mc)

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "rm -rf /tmp/test"},
	})
	require.NoError(t, err)
	assert.Equal(t, "done\n", res.Output)
	assert.Len(t, mc.execCalls, 1)
}

// ---------------------------------------------------------------------------
// Output limiting tests
// ---------------------------------------------------------------------------

func TestRemoteBashToolOutputLimiting(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	longOutput := strings.Repeat("A", 500)
	mc := &mockSSHClient{
		execFn: func(_ context.Context, _ string) (string, string, int, error) {
			return longOutput, "", 0, nil
		},
	}
	tool := NewRemoteBashTool(mc, WithRemoteBashMaxOutput(50))

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "cat bigfile"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "[output truncated]")
	assert.Less(t, len(res.Output), 100, "output should be truncated well below original size")
}

func TestRemoteBashToolOutputLimitingWithStderr(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mc := &mockSSHClient{
		execFn: func(_ context.Context, _ string) (string, string, int, error) {
			return strings.Repeat("O", 100), strings.Repeat("E", 100), 0, nil
		},
	}
	tool := NewRemoteBashTool(mc, WithRemoteBashMaxOutput(30))

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "noisy"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "[output truncated]")
}

// ---------------------------------------------------------------------------
// Host selection tests
// ---------------------------------------------------------------------------

func TestRemoteBashToolDefaultHost(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	defaultMC := &mockSSHClient{
		execFn: func(_ context.Context, _ string) (string, string, int, error) {
			return "default-host-response\n", "", 0, nil
		},
	}
	tool := NewRemoteBashTool(defaultMC)

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "hostname"},
	})
	require.NoError(t, err)
	assert.Equal(t, "default-host-response\n", res.Output)
	assert.Len(t, defaultMC.execCalls, 1)
}

func TestRemoteBashToolHostSelection(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	defaultMC := &mockSSHClient{
		execFn: func(_ context.Context, _ string) (string, string, int, error) {
			return "default\n", "", 0, nil
		},
	}
	secondaryMC := &mockSSHClient{
		execFn: func(_ context.Context, _ string) (string, string, int, error) {
			return "secondary\n", "", 0, nil
		},
	}
	tool := NewRemoteBashTool(defaultMC, WithRemoteBashHosts(map[string]SSHClient{
		"secondary": secondaryMC,
	}))

	// Explicit host selection.
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "hostname", "host": "secondary"},
	})
	require.NoError(t, err)
	assert.Equal(t, "secondary\n", res.Output)
	assert.Empty(t, defaultMC.execCalls, "default client should not be called")
	assert.Len(t, secondaryMC.execCalls, 1)
}

func TestRemoteBashToolUnknownHost(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	defaultMC := &mockSSHClient{}
	tool := NewRemoteBashTool(defaultMC)

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "echo hi", "host": "unknown-host"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown host")
	assert.Empty(t, defaultMC.execCalls)
}

// ---------------------------------------------------------------------------
// Timeout tests
// ---------------------------------------------------------------------------

func TestRemoteBashToolContextCancel(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mc := &mockSSHClient{
		execFn: func(ctx context.Context, _ string) (string, string, int, error) {
			<-ctx.Done()
			return "", "", -1, ctx.Err()
		},
	}
	tool := NewRemoteBashTool(mc, WithRemoteBashTimeout(5*time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tool.Execute(ctx, ToolCall{
		Args: map[string]any{"command": "echo should not run"},
	})
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Option tests
// ---------------------------------------------------------------------------

func TestWithRemoteBashSandbox(t *testing.T) {
	sb := NewDefaultBashSandbox()
	tool := NewRemoteBashTool(&mockSSHClient{}, WithRemoteBashSandbox(sb))
	assert.NotNil(t, tool.sandbox)
}

func TestWithRemoteBashMaxOutput(t *testing.T) {
	tool := NewRemoteBashTool(&mockSSHClient{}, WithRemoteBashMaxOutput(4096))
	assert.Equal(t, 4096, tool.maxOutput)
}

func TestWithRemoteBashTimeout(t *testing.T) {
	tool := NewRemoteBashTool(&mockSSHClient{}, WithRemoteBashTimeout(10*time.Second))
	assert.Equal(t, 10*time.Second, tool.timeout)
}

func TestWithRemoteBashHosts(t *testing.T) {
	mc := &mockSSHClient{}
	tool := NewRemoteBashTool(&mockSSHClient{}, WithRemoteBashHosts(map[string]SSHClient{
		"web1": mc,
	}))
	assert.Contains(t, tool.hostClients, "web1")
}
