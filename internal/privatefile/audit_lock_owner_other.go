//go:build !darwin && !linux

package privatefile

import "os"

func ownedByCurrentUser(os.FileInfo) bool {
	return false
}

func auditLockOwnershipSupported() bool {
	return false
}
