//go:build !darwin && !linux

package privatefile

import (
	"context"
	"errors"
	"os"
)

var errUnsupportedLock = errors.New("privatefile: advisory file locks are unsupported on this platform")

func lockOS(context.Context, *os.File) (func() error, error) {
	return nil, errUnsupportedLock
}
