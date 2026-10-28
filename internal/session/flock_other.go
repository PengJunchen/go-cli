//go:build !darwin && !linux

package session

import (
	"context"
	"os"
)

// flockExclusive fails closed when the platform cannot provide the durable
// session lock required by R2.
func flockExclusive(_ *os.File) error { return ErrUnsupportedSessionLock }

func flockExclusiveContext(_ context.Context, _ *os.File) error { return ErrUnsupportedSessionLock }

// flockUnlock is a no-op on platforms that do not support flock.
func flockUnlock(_ *os.File) error { return nil }

func trySessionReadLock(_ *os.File) (bool, error) { return false, ErrUnsupportedSessionLock }
