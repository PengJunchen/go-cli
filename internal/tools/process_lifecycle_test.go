package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBashCancellationKillsPipeHoldingDescendant(t *testing.T) {
	testCancellationKillsPipeHoldingDescendant(t, func(ctx context.Context, call ToolCall) (*ToolResult, error) {
		return NewBashTool(WithTimeout(100*time.Millisecond), WithNoSandbox()).Execute(ctx, call)
	})
}

func TestStreamingBashCancellationKillsPipeHoldingDescendant(t *testing.T) {
	testCancellationKillsPipeHoldingDescendant(t, func(ctx context.Context, call ToolCall) (*ToolResult, error) {
		return NewStreamingBashTool(WithTimeout(100*time.Millisecond), WithNoSandbox()).Execute(ctx, call)
	})
}

func TestStreamingBashCancellationReturnsWithinBudget(t *testing.T) {
	testCancellationKillsPipeHoldingDescendant(t, func(ctx context.Context, call ToolCall) (*ToolResult, error) {
		return NewStreamingBashTool(WithTimeout(100*time.Millisecond), WithNoSandbox()).Execute(ctx, call)
	})
}

func TestBashNormalExitCleansRedirectedDescendant(t *testing.T) {
	testNormalExitCleansRedirectedDescendant(t, func(ctx context.Context, call ToolCall) (*ToolResult, error) {
		return NewBashTool(WithTimeout(2*time.Second), WithNoSandbox()).Execute(ctx, call)
	})
}

func TestStreamingBashNormalExitCleansRedirectedDescendant(t *testing.T) {
	testNormalExitCleansRedirectedDescendant(t, func(ctx context.Context, call ToolCall) (*ToolResult, error) {
		return NewStreamingBashTool(WithTimeout(2*time.Second), WithNoSandbox()).Execute(ctx, call)
	})
}

func TestBashNormalExitCleansPipeHoldingDescendant(t *testing.T) {
	testNormalExitCleansPipeHoldingDescendant(t, func(ctx context.Context, call ToolCall) (*ToolResult, error) {
		return NewBashTool(WithTimeout(10*time.Second), WithNoSandbox()).Execute(ctx, call)
	})
}

func TestStreamingBashNormalExitCleansPipeHoldingDescendant(t *testing.T) {
	testNormalExitCleansPipeHoldingDescendant(t, func(ctx context.Context, call ToolCall) (*ToolResult, error) {
		return NewStreamingBashTool(WithTimeout(10*time.Second), WithNoSandbox()).Execute(ctx, call)
	})
}

func TestBashCancellationAtStartupRace(t *testing.T) {
	for i := 0; i < 8; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		_, err := NewBashTool(WithTimeout(time.Second), WithNoSandbox()).Execute(ctx, ToolCall{
			ID:   fmt.Sprintf("call-start-race-%d", i),
			Name: "bash",
			Args: map[string]any{"command": "sleep 0.2"},
		})
		cancel()
		require.ErrorIs(t, err, context.DeadlineExceeded)
	}
}

func TestDefaultSSHClientCancellationStopsLocalTransportDescendants(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep is required for process lifecycle tests")
	}
	marker := filepath.Join(t.TempDir(), "ssh-child-finished")
	installFakeSSH(t, fmt.Sprintf("(sleep 0.6; printf finished > %s) & child=$!; wait \"$child\"", shellQuoteTestArgument(marker)))
	client := NewDefaultSSHClient(SSHConfig{Host: "example.invalid", KeyPath: "/tmp/fake-key"})
	started := time.Now()
	result, err := NewRemoteBashTool(client, WithRemoteBashTimeout(100*time.Millisecond)).Execute(context.Background(), ToolCall{
		ID:   "call-ssh-local-cancel",
		Name: "remote_bash",
		Args: map[string]any{"command": "ignored by fake ssh"},
	})
	elapsed := time.Since(started)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotNil(t, result)
	assert.Equal(t, processContainmentScope, result.Metadata["local_ssh_process_group_scope"])
	assert.Equal(t, "unknown", result.Metadata["remote_completion"])
	assert.Less(t, elapsed, 400*time.Millisecond, "the owned local ssh group must stop within the bounded cleanup budget")
	var unknown interface{ OutcomeUnknown() bool }
	require.ErrorAs(t, err, &unknown)
	assert.True(t, unknown.OutcomeUnknown(), "stopping local ssh cannot prove whether the remote command ran")
	var pending PendingExecutionError
	require.ErrorAs(t, err, &pending, "the remote command may continue after its local SSH transport stops")
	select {
	case <-pending.ExecutionDone():
		t.Fatal("local SSH process-group quiescence cannot prove remote completion")
	default:
	}
	if remaining := started.Add(750 * time.Millisecond).Sub(time.Now()); remaining > 0 {
		time.Sleep(remaining)
	}
	assert.NoFileExists(t, marker, "a local transport descendant in the owned group must be stopped")
}

func testCancellationKillsPipeHoldingDescendant(t *testing.T, execute func(context.Context, ToolCall) (*ToolResult, error)) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required for process lifecycle tests")
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep is required for process lifecycle tests")
	}

	marker := filepath.Join(t.TempDir(), "child-finished")
	command := fmt.Sprintf(
		"(sleep 0.6; printf finished > %s) & child=$!; printf '%%s\\n' \"$child\" >&2; wait \"$child\"",
		shellQuoteTestArgument(marker),
	)
	started := time.Now()
	result, err := execute(context.Background(), ToolCall{
		ID:   "call-process-lifecycle",
		Name: "bash",
		Args: map[string]any{"command": command},
	})
	elapsed := time.Since(started)

	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	var unknown interface{ OutcomeUnknown() bool }
	require.ErrorAs(t, err, &unknown)
	assert.True(t, unknown.OutcomeUnknown())
	var pending PendingExecutionError
	assert.NotErrorAs(t, err, &pending, "a stopped local process group has unknown outcome but no pending resource")
	require.NotNil(t, result)
	assert.Equal(t, processContainmentScope, result.Metadata["containment_scope"])
	_, parseErr := fmt.Sscan(strings.TrimSpace(result.Output), new(int))
	require.NoError(t, parseErr, "the command should report the child PID before the deadline")
	assert.Less(t, elapsed, 400*time.Millisecond, "cancellation must not wait for the descendant's inherited output pipes")

	// Allow a wrongly detached child to reach its delayed write before checking
	// for it. This also ensures the finite child is never left running after a
	// failing regression test.
	if remaining := started.Add(750 * time.Millisecond).Sub(time.Now()); remaining > 0 {
		time.Sleep(remaining)
	}
	assert.NoFileExists(t, marker, "a descendant in the command's owned process scope must be stopped before it performs later work")
}

func shellQuoteTestArgument(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func installFakeSSH(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	require.NoError(t, os.WriteFile(sshPath, []byte("#!/bin/sh\n"+script+"\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func testNormalExitCleansRedirectedDescendant(t *testing.T, execute func(context.Context, ToolCall) (*ToolResult, error)) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required for process lifecycle tests")
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep is required for process lifecycle tests")
	}

	started := time.Now()
	marker := filepath.Join(t.TempDir(), "redirected-child-finished")
	command := fmt.Sprintf("(sleep 0.6; printf finished > %s) >/dev/null 2>&1 & exit 0", shellQuoteTestArgument(marker))
	result, err := execute(context.Background(), ToolCall{
		ID:   "call-normal-exit-child",
		Name: "bash",
		Args: map[string]any{"command": command},
	})

	assert.Error(t, err, "normal shell exit with a live descendant cannot claim a complete execution")
	assert.NotNil(t, result)
	assert.Equal(t, processContainmentScope, result.Metadata["containment_scope"])
	assert.Less(t, time.Since(started), 500*time.Millisecond, "post-exit process cleanup must stay bounded")
	if remaining := started.Add(750 * time.Millisecond).Sub(time.Now()); remaining > 0 {
		time.Sleep(remaining)
	}
	assert.NoFileExists(t, marker, "pipe EOF does not prove a redirected child stopped")
}

func testNormalExitCleansPipeHoldingDescendant(t *testing.T, execute func(context.Context, ToolCall) (*ToolResult, error)) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required for process lifecycle tests")
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep is required for process lifecycle tests")
	}

	started := time.Now()
	marker := filepath.Join(t.TempDir(), "pipe-child-finished")
	command := fmt.Sprintf("(sleep 3; printf finished > %s) & exit 0", shellQuoteTestArgument(marker))
	result, err := execute(context.Background(), ToolCall{
		ID:   "call-normal-exit-pipe-child",
		Name: "bash",
		Args: map[string]any{"command": command},
	})

	assert.Error(t, err, "normal leader exit with a child holding the pipe must be noticed before the command timeout")
	assert.NotNil(t, result)
	assert.Equal(t, processContainmentScope, result.Metadata["containment_scope"])
	assert.Less(t, time.Since(started), time.Second, "leader exit must trigger bounded group cleanup")
	assert.NoFileExists(t, marker, "the inherited-pipe descendant must not outlive its owned process group")
}

func TestRemoteBashMetadataRemainsBounded(t *testing.T) {
	const maxOutput = 50
	client := &mockSSHClient{
		execFn: func(_ context.Context, _ string) (string, string, int, error) {
			return strings.Repeat("O", 40), strings.Repeat("E", 40), 0, nil
		},
	}
	tool := NewRemoteBashTool(client, WithRemoteBashMaxOutput(maxOutput))

	result, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-remote-bounded-output",
		Name: "remote_bash",
		Args: map[string]any{"command": "noisy"},
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	stdout, ok := result.Metadata["stdout"].(string)
	require.True(t, ok, "stdout metadata should remain a string")
	stderr, ok := result.Metadata["stderr"].(string)
	require.True(t, ok, "stderr metadata should remain a string")
	assert.LessOrEqual(t, len(stdout)+len(stderr), maxOutput, "metadata must share the configured combined source-byte budget")
	assert.Equal(t, true, result.Metadata["truncated"], "metadata must report that output was discarded")
	assert.Contains(t, result.Output, "[output truncated]")
	assert.LessOrEqual(t, len(result.Output), maxOutput+len("\n[output truncated]"))
	assert.NotContains(t, result.Metadata, "local_ssh_process_group_scope", "custom SSH clients do not declare local process containment")
}
