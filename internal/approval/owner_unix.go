//go:build unix

package approval

import (
	"fmt"
	"os"
	"syscall"
)

// validateFileOwner checks the current Unix user without weakening file modes.
func validateFileOwner(info os.FileInfo, path string) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ErrOwnershipUnsupported
	}
	if stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("private config %s owned by uid %d, expected %d", path, stat.Uid, os.Getuid())
	}
	return nil
}
