//go:build linux

package tools

import (
	"os/exec"
	"syscall"
)

// rlimitSnapshot captures the current process's RLIMIT_AS so it can be
// restored after a child process inherits it.
type rlimitSnapshot struct {
	as syscall.Rlimit
}

// saveRlimits records the current process's RLIMIT_AS.
func saveRlimits() rlimitSnapshot {
	var s rlimitSnapshot
	// Best-effort: rlimit may be unavailable on some kernels.
	_ = syscall.Getrlimit(syscall.RLIMIT_AS, &s.as)
	return s
}

// restoreRlimits restores the saved RLIMIT_AS to the current process.
func restoreRlimits(s rlimitSnapshot) {
	// Best-effort: restoration failure is non-fatal.
	_ = syscall.Setrlimit(syscall.RLIMIT_AS, &s.as)
}

// applyRlimits sets memory resource limits on the current process so the
// child inherits them at fork time. On Linux, RLIMIT_AS bounds the total
// virtual address space. Callers must save before and restore after to avoid
// affecting the parent.
//
// RLIMIT_CPU is not set because it is cumulative and would risk killing the
// parent process. CPU time is bounded by the context timeout instead.
func applyRlimits(_ *exec.Cmd, limits ResourceLimits) {
	if limits.MaxMemory > 0 {
		// Best-effort: rlimit may be unavailable or insufficient permissions.
		_ = syscall.Setrlimit(syscall.RLIMIT_AS, &syscall.Rlimit{
			Cur: uint64(limits.MaxMemory), Max: uint64(limits.MaxMemory),
		})
	}
}
