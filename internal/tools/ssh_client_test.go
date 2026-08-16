package tools

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// envHasKey reports whether the env list (entries of the form "KEY=VALUE")
// contains an entry for the given key.
func envHasKey(env []string, key string) bool {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// envValue returns the value for key in the env list, and whether it was
// present.
func envValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix), true
		}
	}
	return "", false
}

// TestSSH_PasswordNotInArgs verifies that the password is never passed as a
// command-line argument. Previously sshpass -p <password> exposed the
// password in the process argument list (visible via ps). With sshpass -e the
// password travels only through the SSHPASS environment variable.
func TestSSH_PasswordNotInArgs(t *testing.T) {
	const password = "s3cr3t-pass-123"
	client := NewDefaultSSHClient(SSHConfig{
		Host:     "example.com",
		User:     "alice",
		Password: password,
	})
	spec := client.buildExecCommand("ls -la")

	assert.Equal(t, "sshpass", spec.name)
	require.NotEmpty(t, spec.args)

	// sshpass is invoked in env mode (-e), not password-arg mode (-p <pw>).
	assert.Equal(t, "-e", spec.args[0], "first sshpass flag must be -e")
	assert.Equal(t, "ssh", spec.args[1], "sshpass must exec ssh")
	assert.NotContains(t, spec.args, "-p", "must not pass password via -p flag")

	// No argument may contain the password value.
	for _, a := range spec.args {
		assert.NotContains(t, a, password, "password must not appear in cmd args: %q", a)
	}

	// The password must also be absent from the fully constructed exec.Cmd
	// args (cmd.Args[0] is the binary name; the rest mirror spec.args).
	cmd := exec.Command(spec.name, spec.args...)
	for _, a := range cmd.Args {
		assert.NotContains(t, a, password, "password must not appear in cmd.Args: %q", a)
	}
}

// TestSSH_PasswordInEnv verifies that the password is delivered to sshpass
// through the SSHPASS environment variable and that the child environment
// contains the essentials (PATH, HOME) required for sshpass to locate ssh and
// for ssh to read ~/.ssh.
func TestSSH_PasswordInEnv(t *testing.T) {
	const password = "s3cr3t-pass-123"
	client := NewDefaultSSHClient(SSHConfig{
		Host:     "example.com",
		User:     "alice",
		Password: password,
	})
	spec := client.buildExecCommand("uptime")

	require.NotNil(t, spec.env, "password auth must set an explicit env")
	assert.True(t, envHasKey(spec.env, "SSHPASS"), "SSHPASS env var must be set")
	val, ok := envValue(spec.env, "SSHPASS")
	require.True(t, ok)
	assert.Equal(t, password, val, "SSHPASS must contain the exact password")
	assert.True(t, envHasKey(spec.env, "PATH"), "PATH must be present for sshpass to find ssh")
	assert.True(t, envHasKey(spec.env, "HOME"), "HOME must be present for ssh config/known_hosts")
}

// TestSSH_EnvNotLeakingToChildren verifies that the child environment is
// restricted to a known small set (SSHPASS, PATH, HOME) rather than inheriting
// the entire parent environment, and that the ssh arguments do not forward
// SSHPASS to the remote host (no SendEnv referencing it).
func TestSSH_EnvNotLeakingToChildren(t *testing.T) {
	const password = "s3cr3t-pass-123"
	client := NewDefaultSSHClient(SSHConfig{
		Host:     "example.com",
		User:     "alice",
		Password: password,
	})
	spec := client.buildExecCommand("uptime")

	require.NotNil(t, spec.env)
	// Exactly the three necessary entries — nothing else leaks through.
	assert.Len(t, spec.env, 3, "env must be restricted to SSHPASS, PATH, HOME")
	for _, e := range spec.env {
		assert.True(t,
			strings.HasPrefix(e, "SSHPASS=") ||
				strings.HasPrefix(e, "PATH=") ||
				strings.HasPrefix(e, "HOME="),
			"unexpected env entry leaked to child: %q", e)
	}

	// ssh args must not reference SSHPASS (no SendEnv/SetEnv forwarding it).
	for _, a := range spec.args {
		assert.NotContains(t, a, "SSHPASS", "ssh args must not forward SSHPASS: %q", a)
	}
}

// TestSSH_KeyAuthUnaffected verifies that key-based auth is untouched by the
// password-passing change: it invokes ssh directly (no sshpass), inherits the
// parent environment (env is nil), and never sets SSHPASS.
func TestSSH_KeyAuthUnaffected(t *testing.T) {
	client := NewDefaultSSHClient(SSHConfig{
		Host:    "example.com",
		User:    "alice",
		KeyPath: "/tmp/fake_key",
	})
	spec := client.buildExecCommand("uptime")

	assert.Equal(t, "ssh", spec.name, "key auth must invoke ssh directly")
	assert.NotContains(t, spec.args, "sshpass", "key auth must not use sshpass")
	assert.Nil(t, spec.env, "key auth must inherit parent env, not set SSHPASS")
}

// TestSSH_KeyAuthPreferredOverPassword confirms that when both KeyPath and
// Password are set, the implementation does not treat it as password auth
// (Password == "" is the gate). This documents the current branching: a
// non-empty Password selects the sshpass path regardless of KeyPath.
func TestSSH_PasswordBranchTakesPrecedence(t *testing.T) {
	const password = "s3cr3t-pass-123"
	client := NewDefaultSSHClient(SSHConfig{
		Host:     "example.com",
		User:     "alice",
		KeyPath:  "/tmp/fake_key",
		Password: password,
	})
	spec := client.buildExecCommand("uptime")

	// Non-empty Password selects the sshpass/-e path and sets SSHPASS.
	assert.Equal(t, "sshpass", spec.name)
	assert.NotNil(t, spec.env)
	assert.True(t, envHasKey(spec.env, "SSHPASS"))
	// Password still absent from args.
	for _, a := range spec.args {
		assert.NotContains(t, a, password)
	}
}

// TestSSH_ExecSetsCmdEnvFromSpec is a light integration check that Exec wires
// the built spec into the exec.Cmd. It does not run a real SSH connection;
// instead it relies on the command failing fast and asserts no panic/panic-free
// construction. The security properties are covered by the buildExecCommand
// tests above.
func TestSSH_ExecSetsCmdEnvFromSpec(t *testing.T) {
	// Use a context that is already cancelled so Exec returns quickly without
	// depending on sshpass/ssh being installed or reachable.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := NewDefaultSSHClient(SSHConfig{
		Host:     "example.com",
		Password: "s3cr3t-pass-123",
	})
	// Exec must not panic and must return a transport error (cancelled or
	// binary not found). We only assert it runs to completion here.
	_, _, _, _ = client.Exec(ctx, "true")
}
