package tools

import (
	"context"
	"fmt"
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

// Connect validates the configuration and optionally tests connectivity.
func (c *DefaultSSHClient) Connect(_ context.Context) error {
	if c.config.Host == "" {
		return fmt.Errorf("ssh: host is required")
	}
	if c.config.KeyPath == "" && c.config.Password == "" {
		return fmt.Errorf("ssh: either key_path or password is required")
	}
	return nil
}

// Exec runs a command on the remote host. (stub)
func (c *DefaultSSHClient) Exec(_ context.Context, _ string) (string, string, int, error) {
	return "", "", 0, fmt.Errorf("ssh: not implemented")
}

// Close is a no-op for the stateless exec-based client.
func (c *DefaultSSHClient) Close() error {
	return nil
}
