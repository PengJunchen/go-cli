//go:build !darwin && !linux

package privatefile

import (
	"context"
	"os"
)

func lockOS(context.Context, *os.File) (func() error, error) {
	return nil, errUnsupportedLock
}
