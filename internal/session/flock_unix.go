//go:build darwin || linux

package session

import (
	"context"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// flockExclusive acquires an exclusive advisory lock on the file descriptor
// underlying f. It blocks until the lock can be acquired.
func flockExclusive(f *os.File) error {
	return flockExclusiveContext(context.Background(), f)
}

// flockExclusiveContext acquires a session-file lock while remaining
// responsive to cancellation by retrying non-blocking OS lock attempts.
func flockExclusiveContext(ctx context.Context, f *os.File) error {
	if f == nil {
		return fmt.Errorf("session: cannot flock nil file")
	}
	if ctx == nil {
		return fmt.Errorf("session: lock context is required")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN && err != unix.EINTR {
			return err
		}
		if err == unix.EINTR {
			continue
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// flockUnlock releases the advisory lock held on f.
func flockUnlock(f *os.File) error {
	if f == nil {
		return nil
	}
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
