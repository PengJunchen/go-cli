package tools

import (
	"os/exec"
	"time"
)

// ResourceLimits describes process-level CPU and memory limits applied to
// child processes spawned by the bash tool.
type ResourceLimits struct {
	// MaxCPU bounds the CPU time a command may consume. Zero means no limit.
	MaxCPU time.Duration
	// MaxMemory bounds the maximum memory (in bytes) a command may consume.
	// Zero means no limit.
	MaxMemory int64
}

// ApplyResourceLimits is retained for backward compatibility. Resource limits
// are now applied via wrapCommandWithLimits which prepends ulimit to the
// command string, affecting only the child process. When both limits are zero
// the function is a no-op.
func ApplyResourceLimits(cmd *exec.Cmd, limits ResourceLimits) {
	applyRlimits(cmd, limits)
}
