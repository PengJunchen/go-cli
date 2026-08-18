//go:build darwin || linux

package cli

import (
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestTerminalWidth_SIGWINCHInvalidates verifies that a SIGWINCH signal
// received by the process invalidates the cached terminal width, causing
// termWidth to be reset to 0 so the next terminalWidth() call re-queries.
func TestTerminalWidth_SIGWINCHInvalidates(t *testing.T) {
	le := NewDefaultLineEditor(strings.NewReader(""), io.Discard)
	le.termWidth.Store(200)

	le.startResizeMonitor()
	defer le.Stop()

	// Allow the monitor goroutine time to call signal.Notify before we
	// send the signal; otherwise the signal may be lost (SIGWINCH's
	// default action is to ignore).
	time.Sleep(100 * time.Millisecond)

	// Send SIGWINCH to the current process, retrying a few times in case
	// of timing jitter.
	p, err := os.FindProcess(os.Getpid())
	require.NoError(t, err)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		require.NoError(t, p.Signal(syscall.SIGWINCH))
		if le.termWidth.Load() == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("SIGWINCH did not invalidate termWidth within 3s timeout")
}
