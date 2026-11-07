//go:build darwin || linux

package cli

import (
	"os"
	"os/signal"
	"syscall"
)

func registerProcessSessionReleaseSignal() (<-chan os.Signal, func()) {
	release := make(chan os.Signal, 1)
	signal.Notify(release, syscall.SIGUSR1)
	return release, func() { signal.Stop(release) }
}
