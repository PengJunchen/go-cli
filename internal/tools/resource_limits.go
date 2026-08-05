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

// ApplyResourceLimits configures the child process represented by cmd to be
// constrained by the given resource limits. The parent process's rlimits are
// temporarily lowered so the child inherits them at fork time; callers must
// call saveRlimits before and restoreRlimits after to avoid affecting the
// parent. When both limits are zero the function is a no-op.
func ApplyResourceLimits(cmd *exec.Cmd, limits ResourceLimits) {
	applyRlimits(cmd, limits)
}
