package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
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

// SSHExecResult contains bounded output and a remote exit status. Truncated
// reports whether the local client discarded bytes after reaching maxOutput.
type SSHExecResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Truncated bool
}

// BoundedSSHClient is an optional extension for clients that can enforce an
// output budget while reading from the local SSH transport.
type BoundedSSHClient interface {
	ExecBounded(ctx context.Context, command string, maxOutput int) (SSHExecResult, error)
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
	if err := ctx.Err(); err != nil {
		return err
	}
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
		execCtx, cancel := context.WithCancel(ctx)
		cmd := exec.Command("ssh", args...)
		managed, err := startManagedLocalCommand(cancel, cmd)
		if err == nil {
			capture := newBoundedOutputCapture(16 * 1024)
			lifecycle := managed.collect(execCtx, func(reader io.Reader, stream string) (bool, error) {
				_, readErr := io.Copy(capture.writer(stream), reader)
				return readErr == nil, readErr
			})
			_, probeErr, _, _ := capture.snapshot("")
			if lifecycle.runErr != nil {
				err = lifecycle.runErr
			}
			if lifecycle.cleanupErr != nil || !lifecycle.quiescent {
				err = errors.Join(err, errors.Join(lifecycle.cleanupErr, errProcessCleanupUnconfirmed))
			}
			if err != nil {
				slog.Warn("ssh.connect_probe_failed",
					"host", c.config.Host,
					"err", err,
					"stderr", probeErr)
			}
		} else {
			slog.Warn("ssh.connect_probe_failed",
				"host", c.config.Host,
				"err", err,
				"stderr", "")
		}
		cancel()
	}

	return nil
}

// sshCommandSpec describes the resolved command for an Exec invocation. The
// env field is nil when the child process should inherit the parent
// environment (key-based auth); when non-nil it is the exact environment for
// the child (password-based auth, which delivers the password via SSHPASS).
type sshCommandSpec struct {
	name string
	args []string
	env  []string
}

// buildExecCommand constructs the command spec for running command on the
// remote host. For password-based auth the password is delivered through the
// SSHPASS environment variable (sshpass -e) rather than as a command-line
// argument (sshpass -p), so it is not exposed in the process argument list.
// The child environment is restricted to PATH, HOME and SSHPASS to avoid
// leaking unrelated variables; ssh does not forward env vars to the remote
// host unless SendEnv is configured, so SSHPASS is not propagated to
// grandchildren such as the remote shell.
func (c *DefaultSSHClient) buildExecCommand(command string) sshCommandSpec {
	args := c.baseArgs()
	args = append(args, command)

	if c.config.Password != "" {
		return sshCommandSpec{
			name: "sshpass",
			args: append([]string{"-e", "ssh"}, args...),
			env: []string{
				"SSHPASS=" + c.config.Password,
				"PATH=" + os.Getenv("PATH"),
				"HOME=" + os.Getenv("HOME"),
			},
		}
	}
	return sshCommandSpec{
		name: "ssh",
		args: args,
	}
}

// Exec runs a command on the remote host via ssh. It returns captured stdout,
// stderr, the remote exit code, and any transport-level error. A non-zero exit
// code from the remote command is NOT treated as an error — it is reported via
// exitCode with err == nil. Transport errors (context cancellation, ssh binary
// not found, etc.) are returned via err with exitCode set to -1.
func (c *DefaultSSHClient) Exec(ctx context.Context, command string) (string, string, int, error) {
	result, err := c.ExecBounded(ctx, command, defaultBashMaxOutput)
	return result.Stdout, result.Stderr, result.ExitCode, err
}

// ExecBounded runs ssh while draining both local transport pipes and retaining
// only maxOutput bytes across stdout and stderr.
func (c *DefaultSSHClient) ExecBounded(ctx context.Context, command string, maxOutput int) (SSHExecResult, error) {
	if err := ctx.Err(); err != nil {
		return SSHExecResult{ExitCode: -1}, newNotDispatchedExecutionError(fmt.Errorf("ssh exec canceled before start: %w", err))
	}
	spec := c.buildExecCommand(command)
	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.Command(spec.name, spec.args...)
	if spec.env != nil {
		cmd.Env = spec.env
	}
	managed, err := startManagedLocalCommand(cancel, cmd)
	if err != nil {
		return SSHExecResult{ExitCode: -1}, newNotDispatchedExecutionError(fmt.Errorf("ssh exec: %w", err))
	}
	capture := newBoundedOutputCapture(maxOutput)
	lifecycle := managed.collect(execCtx, func(reader io.Reader, stream string) (bool, error) {
		_, readErr := io.Copy(capture.writer(stream), reader)
		return readErr == nil, readErr
	})
	stdout, stderr, _, truncated := capture.snapshot("")
	result := SSHExecResult{Stdout: stdout, Stderr: stderr, Truncated: truncated, ExitCode: -1}
	if lifecycle.cleanupErr != nil || !lifecycle.quiescent {
		cause := errors.Join(errProcessCleanupUnconfirmed, lifecycle.cleanupErr, lifecycle.readerErr)
		if execCtx.Err() != nil {
			cause = errors.Join(cause, execCtx.Err())
		}
		return result, pendingProcessError(fmt.Errorf("ssh exec: %w", cause), true)
	}
	if lifecycle.interrupted {
		cause := execCtx.Err()
		if cause == nil {
			cause = context.Canceled
		}
		return result, pendingProcessError(fmt.Errorf("ssh exec: remote completion is unknown after local transport stopped: %w", cause), true)
	}
	if lifecycle.escalated {
		return result, pendingProcessError(fmt.Errorf("ssh exec: remote completion is unknown after local transport cleanup: %w", errors.Join(errProcessGroupRemained, lifecycle.runErr)), true)
	}
	if lifecycle.readerErr != nil {
		return result, pendingProcessError(fmt.Errorf("ssh exec: remote completion is unknown after output read failed: %w", lifecycle.readerErr), true)
	}
	// An *exec.ExitError means the remote command ran but exited non-zero.
	// This is a successful transport-level execution; report via exitCode.
	var ee *exec.ExitError
	if errors.As(lifecycle.runErr, &ee) {
		if ee.ExitCode() == 255 {
			// OpenSSH uses 255 for transport/protocol failures, for which there
			// is no terminal receipt proving whether the remote command ran.
			cause := fmt.Errorf("ssh exec exited 255 without a remote completion receipt: %w", ee)
			return result, pendingProcessError(cause, true)
		}
		if c.config.Password != "" && ee.ExitCode() >= 1 && ee.ExitCode() <= 7 {
			// sshpass reserves statuses 1-7 for its own failures, while also
			// forwarding a remote command's exit status. These values are
			// ambiguous without a separate remote receipt. The local process and
			// readers are joined, so report unknown outcome without a live-resource
			// marker.
			cause := fmt.Errorf("sshpass exited %d without an unambiguous remote completion receipt: %w", ee.ExitCode(), ee)
			return result, newUnknownExecutionError(cause)
		}
		result.ExitCode = ee.ExitCode()
		return result, nil
	}

	if lifecycle.runErr != nil {
		// Transport-level failure (context cancel, binary not found, etc.).
		return result, pendingProcessError(fmt.Errorf("ssh exec: remote completion is unknown after transport failure: %w", lifecycle.runErr), true)
	}

	result.ExitCode = 0
	return result, nil
}

// Close is a no-op for the stateless exec-based client.
func (c *DefaultSSHClient) Close() error {
	return nil
}

// baseArgs builds the common ssh arguments (options + target) shared by
// Connect and Exec. The remote command is appended by the caller.
func (c *DefaultSSHClient) baseArgs() []string {
	args := []string{
		"-o", "StrictHostKeyChecking=yes",
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
