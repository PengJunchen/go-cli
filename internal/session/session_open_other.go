//go:build !darwin && !linux

package session

import (
	"os"
)

func openSessionDirectoryNoFollow(_ string) (*os.File, error) {
	return nil, ErrUnsupportedSessionNoFollowOpen
}

func openSessionFileNoFollow(_, _ string, _ int, _ os.FileMode) (*os.File, error) {
	return nil, ErrUnsupportedSessionNoFollowOpen
}
