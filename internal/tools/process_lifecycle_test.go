package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

func TestPreparedBashWaitsForDurableProcessIdentityBeforeCommandRuns(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required for process lifecycle tests")
	}
	marker := filepath.Join(t.TempDir(), "prepared-bash-ran")
	observed := make(chan ProcessExecutionIdentity, 1)
	release := make(chan struct{})
	ctx := WithProcessExecutionIdentityObserver(context.Background(), func(_ context.Context, identity ProcessExecutionIdentity) error {
		observed <- identity
		<-release
		return nil
	})
	result := make(chan error, 1)
	go func() {
		_, err := NewBashTool(WithTimeout(5*time.Second), WithNoSandbox()).Execute(ctx, ToolCall{
			ID:   "call-durable-process-identity",
			Name: "bash",
			Args: map[string]any{"command": "printf ran > " + shellQuoteTestArgument(marker)},
		})
		result <- err
	}()

	select {
	case identity := <-observed:
		require.Equal(t, runtime.GOOS, identity.Platform)
		require.NotEmpty(t, identity.HostGeneration)
		require.Positive(t, identity.LeaderPID)
		require.NotEmpty(t, identity.ProcessStartToken)
		require.Equal(t, identity.LeaderPID, identity.ProcessGroupID)
		require.Positive(t, identity.OwnerPID)
		require.NotEmpty(t, identity.OwnerProcessStartToken)
		require.NoFileExists(t, marker, "the command must remain behind the start gate until its exact identity is durably recorded")
		close(release)
	case err := <-result:
		t.Fatalf("prepared Bash returned without waiting for the durable process identity: err=%v marker_exists=%t", err, fileExists(marker))
	case <-time.After(3 * time.Second):
		t.Fatal("prepared Bash did not report its process identity")
	}

	require.NoError(t, <-result)
	require.FileExists(t, marker, "the command starts after the observer confirms durable identity")
}

func TestPreparedBashDoesNotRunWhenProcessIdentityPersistenceFails(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required for process lifecycle tests")
	}
	marker := filepath.Join(t.TempDir(), "prepared-bash-must-not-run")
	persistErr := errors.New("identity journal sync failed")
	ctx := WithProcessExecutionIdentityObserver(context.Background(), func(context.Context, ProcessExecutionIdentity) error {
		return persistErr
	})
	_, err := NewBashTool(WithTimeout(5*time.Second), WithNoSandbox()).Execute(ctx, ToolCall{
		ID:   "call-identity-sync-failure",
		Name: "bash",
		Args: map[string]any{"command": "printf ran > " + shellQuoteTestArgument(marker)},
	})
	require.ErrorIs(t, err, persistErr)
	require.NoFileExists(t, marker, "an identity persistence failure must keep user code behind the gate")
}

func TestPreparedBashDoesNotSourceBashEnvBeforeIdentityPersistence(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required for process lifecycle tests")
	}
	directory := t.TempDir()
	marker := filepath.Join(directory, "bash-env-ran")
	envMarker := filepath.Join(directory, "env-ran")
	startup := filepath.Join(directory, "bash-env")
	envStartup := filepath.Join(directory, "env-startup")
	require.NoError(t, os.WriteFile(startup, []byte("touch "+shellQuoteTestArgument(marker)+"\n"), 0o600))
	require.NoError(t, os.WriteFile(envStartup, []byte("touch "+shellQuoteTestArgument(envMarker)+"\n"), 0o600))
	env := filteredEnv(map[string]string{"BASH_ENV": startup, "ENV": envStartup})
	require.Contains(t, env, "BASH_ENV="+startup)
	require.Contains(t, env, "ENV="+envStartup)
	control := exec.Command("bash", "--noprofile", "--norc", "-c", ":")
	control.Env = env
	require.NoError(t, control.Run())
	require.FileExists(t, marker, "the fixture must demonstrate that this Bash honors BASH_ENV")
	require.NoFileExists(t, envMarker, "noninteractive Bash must not source ENV")
	require.NoError(t, os.Remove(marker))
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseGate()
	observed := make(chan struct{})
	ctx := WithProcessExecutionIdentityObserver(context.Background(), func(context.Context, ProcessExecutionIdentity) error {
		close(observed)
		<-release
		return nil
	})
	result := make(chan error, 1)
	go func() {
		_, err := NewBashTool(WithTimeout(5*time.Second), WithNoSandbox(), WithEnv(map[string]string{"BASH_ENV": startup, "ENV": envStartup})).Execute(ctx, ToolCall{
			ID: "call-bash-env-gated", Name: "bash", Args: map[string]any{"command": ":"},
		})
		result <- err
	}()

	select {
	case <-observed:
	case err := <-result:
		t.Fatalf("prepared Bash returned before identity callback: err=%v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("prepared Bash did not enter the identity callback")
	}
	// cmd.Start returns before the new process necessarily completes startup.
	// Poll while the observer remains blocked so a wrongly sourced BASH_ENV has
	// a bounded window to make its side effect visible.
	startupDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(startupDeadline) {
		if fileExists(marker) || fileExists(envMarker) {
			releaseGate()
			require.NoError(t, <-result, "allow the blocked process to unwind after detecting the regression")
			t.Fatal("BASH_ENV or ENV executed before process identity persistence")
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.NoFileExists(t, marker, "BASH_ENV must not execute before process identity persistence")
	require.NoFileExists(t, envMarker, "ENV must not execute before process identity persistence")
	releaseGate()
	require.NoError(t, <-result)
	require.FileExists(t, marker, "the original Bash environment must be restored after identity persistence")
	require.NoFileExists(t, envMarker, "the original noninteractive Bash invocation must not source ENV")
}

func TestPreparedBashRejectsExportedFunctionsBeforeStart(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required for process lifecycle tests")
	}
	directory := t.TempDir()
	marker := filepath.Join(directory, "bash-function-ran")
	function := "() { touch " + shellQuoteTestArgument(marker) + "; }"
	env := filteredEnv(map[string]string{"BASH_FUNC_read%%": function})
	control := exec.Command("bash", "--noprofile", "--norc", "-c", "read -r _")
	control.Env = env
	control.Stdin = strings.NewReader("ready\n")
	require.NoError(t, control.Run())
	if !fileExists(marker) {
		t.Skip("this Bash does not import exported Bash functions")
	}
	require.NoError(t, os.Remove(marker))
	observerCalled := false
	ctx := WithProcessExecutionIdentityObserver(context.Background(), func(context.Context, ProcessExecutionIdentity) error {
		observerCalled = true
		return nil
	})
	_, err := NewBashTool(WithTimeout(5*time.Second), WithNoSandbox(), WithEnv(map[string]string{"BASH_FUNC_read%%": function})).Execute(ctx, ToolCall{
		ID: "call-bash-function-gated", Name: "bash", Args: map[string]any{"command": "read -r _ < /dev/null"},
	})
	require.ErrorContains(t, err, "cannot be restored safely")
	require.False(t, observerCalled, "unsupported exported functions must fail before starting a child")
	require.NoFileExists(t, marker, "an unsupported exported function must not run before the identity gate")
}

func TestPreparedBashDoesNotEvaluatePS4BeforeIdentityPersistence(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required for process lifecycle tests")
	}
	marker := filepath.Join(t.TempDir(), "ps4-command-substitution-ran")
	ps4 := "$(touch " + shellQuoteTestArgument(marker) + ")"
	env := filteredEnv(map[string]string{"SHELLOPTS": "xtrace", "PS4": ps4})
	control := exec.Command("bash", "--noprofile", "--norc", "-c", ":")
	control.Env = env
	require.NoError(t, control.Run())
	require.FileExists(t, marker, "the fixture must demonstrate that Bash expands PS4 when xtrace is enabled")
	require.NoError(t, os.Remove(marker))

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseGate()
	observed := make(chan struct{})
	ctx := WithProcessExecutionIdentityObserver(context.Background(), func(context.Context, ProcessExecutionIdentity) error {
		close(observed)
		<-release
		return nil
	})
	result := make(chan error, 1)
	go func() {
		_, err := NewBashTool(WithTimeout(5*time.Second), WithNoSandbox(), WithEnv(map[string]string{"SHELLOPTS": "xtrace", "PS4": ps4})).Execute(ctx, ToolCall{
			ID: "call-bash-ps4-gated", Name: "bash", Args: map[string]any{"command": ":"},
		})
		result <- err
	}()
	select {
	case <-observed:
	case err := <-result:
		t.Fatalf("prepared Bash returned before identity callback: err=%v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("prepared Bash did not enter the identity callback")
	}
	startupDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(startupDeadline) {
		if fileExists(marker) {
			releaseGate()
			require.NoError(t, <-result, "allow the blocked process to unwind after detecting the regression")
			t.Fatal("PS4 command substitution executed before process identity persistence")
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.NoFileExists(t, marker, "SHELLOPTS and PS4 must not affect the bootstrap before identity persistence")
	releaseGate()
	require.NoError(t, <-result)
	require.FileExists(t, marker, "the original Bash tracing environment must be restored after identity persistence")
}

func TestManagedBashStartGatePreservesArgv0(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is required for process lifecycle tests")
	}
	cmd := exec.Command("bash", "-lc", `printf '%s' "$0"`)
	cmd.Env = filteredEnv(nil)
	var output bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &stderr
	gate, err := installBashProcessStartGate(cmd)
	require.NoError(t, err)
	require.Equal(t, bashPath, cmd.Path)
	require.Equal(t, "bash", cmd.Args[0], "the original Bash argv0 must remain available to the post-gate exec")
	require.Empty(t, cmd.Env, "the bootstrap process must receive no user-controlled environment")
	require.NoError(t, cmd.Start())
	gate.closeReadEnd()
	require.NoError(t, gate.release())
	if err := cmd.Wait(); err != nil {
		t.Fatalf("gated command failed: %v; stderr=%s", err, stderr.String())
	}
	require.Equal(t, "bash", output.String(), "bash -lc must retain its original $0")
}

func TestManagedBashStartGateClosesControlPipeBeforeUserCommand(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required for process lifecycle tests")
	}
	cmd := exec.Command("bash", "-lc", `test ! -e /dev/fd/3 && printf closed || printf leaked`)
	cmd.Env = filteredEnv(nil)
	var output bytes.Buffer
	cmd.Stdout = &output
	gate, err := installBashProcessStartGate(cmd)
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	gate.closeReadEnd()
	require.NoError(t, gate.release())
	require.NoError(t, cmd.Wait())
	require.Equal(t, "closed", output.String(), "the control pipe must not leak into user Bash")
}

func TestManagedBashStartGateRestoresEnvironmentWithoutShellParsing(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is required for process lifecycle tests")
	}
	marker := filepath.Join(t.TempDir(), "environment-value-executed")
	value := " leading space\nquote: ' ; touch " + shellQuoteTestArgument(marker) + " $(touch " + shellQuoteTestArgument(marker) + ") trailing space "
	cmd := exec.Command("bash", "-lc", `printf '%s' "$GATE_VALUE"`)
	cmd.Env = filteredEnv(map[string]string{"GATE_VALUE": value})
	var output bytes.Buffer
	cmd.Stdout = &output
	gate, err := installBashProcessStartGate(cmd)
	require.NoError(t, err)
	require.Equal(t, bashPath, cmd.Path)
	require.NoError(t, cmd.Start())
	gate.closeReadEnd()
	require.NoError(t, gate.release())
	require.NoError(t, cmd.Wait())
	require.Equal(t, value, output.String(), "newline, quote, and shell metacharacters must survive opaque NUL-delimited restoration")
	require.NoFileExists(t, marker, "environment values must not be parsed as shell code")
}

func TestManagedBashStartGateRestoresBashOptions(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required for process lifecycle tests")
	}
	command := `shopt -q extglob && printf yes || printf no`
	env := filteredEnv(map[string]string{"BASHOPTS": "extglob"})
	control := exec.Command("bash", "-lc", command)
	control.Env = env
	var expected bytes.Buffer
	control.Stdout = &expected
	require.NoError(t, control.Run())

	cmd := exec.Command("bash", "-lc", command)
	cmd.Env = env
	var output bytes.Buffer
	cmd.Stdout = &output
	gate, err := installBashProcessStartGate(cmd)
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	gate.closeReadEnd()
	require.NoError(t, gate.release())
	require.NoError(t, cmd.Wait())
	require.Equal(t, expected.String(), output.String(), "BASHOPTS behavior must match an ordinary Bash invocation")
}

func TestPreparedStreamingBashWaitsForIdentityPersistence(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required for process lifecycle tests")
	}
	marker := filepath.Join(t.TempDir(), "prepared-streaming-bash-ran")
	observed := make(chan ProcessExecutionIdentity, 1)
	release := make(chan struct{})
	ctx := WithProcessExecutionIdentityObserver(context.Background(), func(_ context.Context, identity ProcessExecutionIdentity) error {
		observed <- identity
		<-release
		return nil
	})
	result := make(chan error, 1)
	go func() {
		_, err := NewStreamingBashTool(WithTimeout(5*time.Second), WithNoSandbox()).ExecuteStreaming(ctx, ToolCall{
			ID:   "call-streaming-durable-process-identity",
			Name: "bash",
			Args: map[string]any{"command": "printf ran > " + shellQuoteTestArgument(marker)},
		}, nil)
		result <- err
	}()

	select {
	case identity := <-observed:
		require.Equal(t, runtime.GOOS, identity.Platform)
		require.Positive(t, identity.LeaderPID)
		require.NotEmpty(t, identity.ProcessStartToken)
		require.Equal(t, identity.LeaderPID, identity.ProcessGroupID)
		require.NoFileExists(t, marker, "streaming Bash must wait for durable process identity before user code runs")
		close(release)
	case err := <-result:
		t.Fatalf("prepared streaming Bash returned without waiting for process identity: err=%v marker_exists=%t", err, fileExists(marker))
	case <-time.After(3 * time.Second):
		t.Fatal("prepared streaming Bash did not report its process identity")
	}

	require.NoError(t, <-result)
	require.FileExists(t, marker)
}

func TestPreparedStreamingBashCancellationBeforeGateReleaseDoesNotRun(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required for process lifecycle tests")
	}
	marker := filepath.Join(t.TempDir(), "prepared-streaming-bash-cancelled")
	ctx := WithProcessExecutionIdentityObserver(context.Background(), func(ctx context.Context, _ ProcessExecutionIdentity) error {
		<-ctx.Done()
		return ctx.Err()
	})
	tool := NewStreamingBashTool(WithTimeout(100*time.Millisecond), WithNoSandbox())
	_, err := tool.ExecuteStreaming(ctx, ToolCall{
		ID:   "call-streaming-process-identity-cancelled",
		Name: "bash",
		Args: map[string]any{"command": "printf ran > " + shellQuoteTestArgument(marker)},
	}, nil)
	require.Error(t, err)
	require.NoFileExists(t, marker, "cancellation while identity persistence is pending must abort before user code")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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
