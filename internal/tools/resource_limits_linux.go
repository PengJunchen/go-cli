//go:build linux

package tools

import (
	"log/slog"
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
	if err := syscall.Getrlimit(syscall.RLIMIT_AS, &s.as); err != nil {
		slog.Debug("rlimit: getrlimit failed", "error", err)
	}
	return s
}

// restoreRlimits restores the saved RLIMIT_AS to the current process.
func restoreRlimits(s rlimitSnapshot) {
	if err := syscall.Setrlimit(syscall.RLIMIT_AS, &s.as); err != nil {
		slog.Debug("rlimit: setrlimit restore failed", "error", err)
	}
}

// applyRlimits sets memory resource limits on the current process so the
// child inherits them at fork time. On Linux, RLIMIT_AS bounds the total
// virtual address space. Callers must save before and restore after to avoid
// affecting the parent.
//
// Under the race detector (TSan), setting RLIMIT_AS is skipped because TSan
// maps a large shadow-memory region that would exceed the limit and cause OOM.
//
// RLIMIT_CPU is not set because it is cumulative and would risk killing the
// parent process. CPU time is bounded by the context timeout instead.
func applyRlimits(_ *exec.Cmd, limits ResourceLimits) {
	if raceEnabled {
		return
	}
	if limits.MaxMemory > 0 {
		if err := syscall.Setrlimit(syscall.RLIMIT_AS, &syscall.Rlimit{
			Cur: uint64(limits.MaxMemory), Max: uint64(limits.MaxMemory),
		}); err != nil {
			slog.Debug("rlimit: setrlimit apply failed", "error", err)
		}
	}
}
