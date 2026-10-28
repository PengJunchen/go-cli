//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package memory

import (
	"context"
	"errors"
	"os"
)

func lockMemoryFile(context.Context, *os.File) error {
	return errors.New("memory: cross-process file locking is unavailable on this platform")
}

func unlockMemoryFile(*os.File) {}
