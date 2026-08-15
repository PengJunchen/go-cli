//go:build !darwin && !linux

package session

import "os"

// flockExclusive is a no-op on platforms that do not support flock (e.g.
// Windows). Concurrent-write safety relies solely on the in-process mutex.
func flockExclusive(_ *os.File) error { return nil }

// flockUnlock is a no-op on platforms that do not support flock.
func flockUnlock(_ *os.File) error { return nil }
