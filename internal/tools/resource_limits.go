package tools

import (
	"fmt"
	"os/exec"
	"runtime"
	"time"
)

// ResourceLimits describes per-process limits inherited by Bash descendants.
// CPU time is rounded up to seconds on Linux and Darwin. Memory is a Linux
// virtual-address-space limit rounded down to KiB; Darwin does not support it.
// These limits do not aggregate a process tree or constrain wall-clock time.
type ResourceLimits struct {
	// MaxCPU bounds each process's CPU time. Zero adds no limit.
	MaxCPU time.Duration
	// MaxMemory bounds each process's virtual address space in bytes.
	// Zero adds no limit.
	MaxMemory int64
}

// ApplyResourceLimits is a legacy no-op retained for backward compatibility.
// Use WithResourceLimits on a Bash tool to apply limits to its child process.
func ApplyResourceLimits(cmd *exec.Cmd, limits ResourceLimits) {
	applyRlimits(cmd, limits)
}

// bashResourceSettings holds validated, shell-compatible resource units.
type bashResourceSettings struct {
	cpuSeconds uint64
	memoryKiB  uint64
}

func (s bashResourceSettings) enabled() bool {
	return s.cpuSeconds != 0 || s.memoryKiB != 0
}

func effectiveBashResourceSettings(limits ResourceLimits, sandbox BashSandbox) (bashResourceSettings, error) {
	if limits.MaxCPU < 0 || limits.MaxMemory < 0 {
		return bashResourceSettings{}, fmt.Errorf("bash: resource limits must be non-negative")
	}
	if configured, ok := sandbox.(*DefaultBashSandbox); ok && configured != nil {
		if configured.maxCPU < 0 || configured.maxMemory < 0 {
			return bashResourceSettings{}, fmt.Errorf("bash: sandbox resource limits must be non-negative")
		}
		if configured.maxCPU > 0 && (limits.MaxCPU == 0 || configured.maxCPU < limits.MaxCPU) {
			limits.MaxCPU = configured.maxCPU
		}
		if configured.maxMemory > 0 && (limits.MaxMemory == 0 || configured.maxMemory < limits.MaxMemory) {
			limits.MaxMemory = configured.maxMemory
		}
	}
	if limits.MaxCPU > 0 && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return bashResourceSettings{}, fmt.Errorf("bash: CPU resource limit unsupported on %s", runtime.GOOS)
	}
	if limits.MaxMemory > 0 && runtime.GOOS != "linux" {
		return bashResourceSettings{}, fmt.Errorf("bash: memory resource limit unsupported on %s", runtime.GOOS)
	}
	if limits.MaxMemory > 0 && limits.MaxMemory < 1024 {
		return bashResourceSettings{}, fmt.Errorf("bash: memory resource limit must be at least 1024 bytes")
	}
	settings := bashResourceSettings{memoryKiB: uint64(limits.MaxMemory / 1024)}
	if limits.MaxCPU > 0 {
		settings.cpuSeconds = uint64(limits.MaxCPU / time.Second)
		if limits.MaxCPU%time.Second != 0 {
			settings.cpuSeconds++
		}
	}
	return settings, nil
}
