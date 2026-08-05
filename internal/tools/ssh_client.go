package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
)

// SSHClient is the contract for executing commands on a remote host via SSH.
// Implementations may use a persistent connection or stateless exec-based
// invocations of the system ssh command.
type SSHClient interface {
	// Connect validates configuration and optionally tests connectivity.
	Connect(ctx context.Context) error
	// Exec runs a command on the remote host and returns captured stdout,
	// stderr, the exit code, and any transport-level error. A non-zero exit
	// code is reported via the return value, not via err (err is nil for
	// successful remote executions even when the command exits non-zero).
	Exec(ctx context.Context, command string) (stdout string, stderr string, exitCode int, err error)
	// Close releases any underlying resources. For stateless implementations
	// this is a no-op.
	Close() error
}

// SSHConfig holds the connection parameters for an SSH client.
type SSHConfig struct {
	Host           string
	Port           int
	User           string
	KeyPath        string
	Password       string
	KnownHostsPath string
}

// DefaultSSHClient implements SSHClient using the system ssh command via
// os/exec. It is stateless: each Exec call spawns a new ssh process, so
// Close is a no-op and there is no persistent connection to manage.
type DefaultSSHClient struct {
	config SSHConfig
}

var _ SSHClient = (*DefaultSSHClient)(nil)

// NewDefaultSSHClient returns a DefaultSSHClient with the given configuration.
func NewDefaultSSHClient(config SSHConfig) *DefaultSSHClient {
	return &DefaultSSHClient{config: config}
}

// Connect validates the configuration and optionally tests connectivity with
// a best-effort connection probe (key-based auth only). Password-based auth
// cannot be tested non-interactively, so only config validation is performed.
func (c *DefaultSSHClient) Connect(ctx context.Context) error {
	if c.config.Host == "" {
		return fmt.Errorf("ssh: host is required")
	}
	if c.config.KeyPath == "" && c.config.Password == "" {
		return fmt.Errorf("ssh: either key_path or password is required")
	}
	if c.config.Password != "" {
		if _, err := exec.LookPath("sshpass"); err != nil {
			return fmt.Errorf("ssh: password auth requires sshpass to be installed: %w", err)
		}
	}

	// Best-effort connection test for key-based auth. BatchMode=yes prevents
	// interactive prompts. On failure we log a warning but do not return an
	// error because the exec-based approach is stateless — a failed probe
	// does not prevent future Exec calls from succeeding.
	if c.config.KeyPath != "" {
		args := c.baseArgs()
		args = append(args, "-o", "BatchMode=yes", "echo", "ok")
		cmd := exec.CommandContext(ctx, "ssh", args...)
		var probeOut, probeErr bytes.Buffer
		cmd.Stdout = &probeOut
		cmd.Stderr = &probeErr
		if err := cmd.Run(); err != nil {
			slog.Warn("ssh.connect_probe_failed",
				"host", c.config.Host,
				"err", err,
				"stderr", probeErr.String())
		}
	}

	return nil
}

// Exec runs a command on the remote host via ssh. It returns captured stdout,
// stderr, the remote exit code, and any transport-level error. A non-zero exit
// code from the remote command is NOT treated as an error — it is reported via
// exitCode with err == nil. Transport errors (context cancellation, ssh binary
// not found, etc.) are returned via err with exitCode set to -1.
func (c *DefaultSSHClient) Exec(ctx context.Context, command string) (string, string, int, error) {
	args := c.baseArgs()
	args = append(args, command)

	var name string
	var cmdArgs []string
	if c.config.Password != "" {
		// Prefix with sshpass for password-based auth.
		name = "sshpass"
		cmdArgs = append([]string{"-p", c.config.Password, "ssh"}, args...)
	} else {
		name = "ssh"
		cmdArgs = args
	}

	cmd := exec.CommandContext(ctx, name, cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	// An *exec.ExitError means the remote command ran but exited non-zero.
	// This is a successful transport-level execution; report via exitCode.
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		return stdout.String(), stderr.String(), ee.ExitCode(), nil
	}

	if runErr != nil {
		// Transport-level failure (context cancel, binary not found, etc.).
		return stdout.String(), stderr.String(), -1, fmt.Errorf("ssh exec: %w", runErr)
	}

	return stdout.String(), stderr.String(), 0, nil
}

// Close is a no-op for the stateless exec-based client.
func (c *DefaultSSHClient) Close() error {
	return nil
}

// baseArgs builds the common ssh arguments (options + target) shared by
// Connect and Exec. The remote command is appended by the caller.
func (c *DefaultSSHClient) baseArgs() []string {
	args := []string{
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=10",
	}
	if c.config.Port != 0 && c.config.Port != 22 {
		args = append(args, "-p", strconv.Itoa(c.config.Port))
	}
	if c.config.KeyPath != "" {
		args = append(args, "-i", c.config.KeyPath)
	}
	if c.config.KnownHostsPath != "" {
		args = append(args, "-o", "UserKnownHostsFile="+c.config.KnownHostsPath)
	}
	// Build the target string: [user@]host
	target := c.config.Host
	if c.config.User != "" {
		target = c.config.User + "@" + c.config.Host
	}
	args = append(args, target)
	return args
}
