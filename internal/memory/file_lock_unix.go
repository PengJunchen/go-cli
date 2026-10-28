//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package memory

import (
	"context"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func lockMemoryFile(ctx context.Context, file *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN && err != unix.EINTR {
			return err
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func unlockMemoryFile(file *os.File) { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN) }
