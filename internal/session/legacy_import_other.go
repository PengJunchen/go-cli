//go:build !darwin && !linux

package session

import "context"

func publishImportedSession(context.Context, string, string, []byte) error {
	return ErrUnsupportedSessionNoFollowOpen
}
