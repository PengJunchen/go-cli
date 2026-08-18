//go:build darwin || linux

package session

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// flockExclusive acquires an exclusive advisory lock on the file descriptor
// underlying f. It blocks until the lock can be acquired.
func flockExclusive(f *os.File) error {
	if f == nil {
		return fmt.Errorf("session: cannot flock nil file")
	}
	return unix.Flock(int(f.Fd()), unix.LOCK_EX)
}

// flockUnlock releases the advisory lock held on f.
func flockUnlock(f *os.File) error {
	if f == nil {
		return nil
	}
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
