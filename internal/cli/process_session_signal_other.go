//go:build !darwin && !linux

package cli

import "os"

func registerProcessSessionReleaseSignal() (<-chan os.Signal, func()) {
	return nil, func() {}
}
