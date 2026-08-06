//go:build linux

package tools

import (
	"fmt"
	"os/exec"
)

// applyRlimits is retained for backward compatibility but is a no-op.
// Resource limits are now applied via wrapCommandWithLimits so only the
// child process is affected, not the Go runtime.
func applyRlimits(_ *exec.Cmd, _ ResourceLimits) {}

// wrapCommandWithLimits prepends ulimit -v (virtual memory) to the bash
// command so the limit only applies to the child process, not the parent
// Go runtime. MaxMemory is in bytes; ulimit -v takes KB.
func wrapCommandWithLimits(command string, limits ResourceLimits) string {
	if limits.MaxMemory > 0 {
		memKB := limits.MaxMemory / 1024
		return fmt.Sprintf("ulimit -v %d; %s", memKB, command)
	}
	return command
}
