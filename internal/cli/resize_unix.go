//go:build darwin || linux

package cli

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// monitorResize listens for SIGWINCH signals and invalidates the cached
// terminal width so that the next terminalWidth() call re-queries the actual
// size. It runs in a goroutine started by startResizeMonitor and exits when
// winchStop is closed.
func (le *DefaultLineEditor) monitorResize() {
	defer close(le.winchDone)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	defer signal.Stop(sigCh)
	for {
		select {
		case <-le.winchStop:
			return
		case <-sigCh:
			le.invalidateTermWidth()
			slog.Debug("cli_line_editor_sigwinch")
		}
	}
}
