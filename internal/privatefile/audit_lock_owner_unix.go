//go:build darwin || linux

package privatefile

import (
	"os"
	"syscall"
)

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(syscall.Geteuid())
}

func auditLockOwnershipSupported() bool {
	return true
}
