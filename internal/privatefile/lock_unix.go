//go:build darwin || linux

package privatefile

import (
	"context"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func lockOS(ctx context.Context, file *os.File) (func() error, error) {
	if file == nil {
		return nil, fmt.Errorf("privatefile: cannot lock nil file")
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() error { return unix.Flock(int(file.Fd()), unix.LOCK_UN) }, nil
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN && err != unix.EINTR {
			return nil, err
		}
		if err == unix.EINTR {
			continue
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
