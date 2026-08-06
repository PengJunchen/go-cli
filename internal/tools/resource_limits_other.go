//go:build !darwin && !linux

package tools

import "os/exec"

// applyRlimits is retained for backward compatibility but is a no-op.
// Resource limits are now applied via wrapCommandWithLimits so only the
// child process is affected, not the Go runtime.
func applyRlimits(_ *exec.Cmd, _ ResourceLimits) {}

// wrapCommandWithLimits is a no-op on unsupported platforms. Memory
// limits are only enforced on Linux.
func wrapCommandWithLimits(command string, _ ResourceLimits) string {
	return command
}
