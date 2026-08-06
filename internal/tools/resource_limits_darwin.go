//go:build darwin

package tools

import (
	"os/exec"
	"syscall"
)

// rlimitSnapshot captures the current process's RLIMIT_DATA so it can be
// restored after a child process inherits it.
type rlimitSnapshot struct {
	data syscall.Rlimit
}

// saveRlimits records the current process's RLIMIT_DATA.
func saveRlimits() rlimitSnapshot {
	var s rlimitSnapshot
	_ = syscall.Getrlimit(syscall.RLIMIT_DATA, &s.data) //nolint:errcheck
	return s
}

// restoreRlimits restores the saved RLIMIT_DATA to the current process.
func restoreRlimits(s rlimitSnapshot) {
	_ = syscall.Setrlimit(syscall.RLIMIT_DATA, &s.data) //nolint:errcheck
}

// applyRlimits sets resource limits on the current process so the child
// inherits them at fork time. On Darwin, SysProcAttr.Rlimit is not available,
// so the parent's rlimits are temporarily lowered. Callers must save before
// and restore after to avoid affecting the parent.
//
// Under the race detector (TSan), setting RLIMIT_DATA is skipped because TSan
// maps a large shadow-memory region that would exceed the limit and cause OOM.
//
// Only RLIMIT_DATA is set (RLIMIT_CPU is not, because it would also limit the
// parent process's cumulative CPU time). CPU time is bounded by the context
// timeout instead.
func applyRlimits(_ *exec.Cmd, limits ResourceLimits) {
	if raceEnabled {
		return
	}
	if limits.MaxMemory > 0 {
		_ = syscall.Setrlimit(syscall.RLIMIT_DATA, &syscall.Rlimit{ //nolint:errcheck
			Cur: uint64(limits.MaxMemory), Max: uint64(limits.MaxMemory),
		})
	}
}
