//go:build !darwin && !linux

package cli

import (
	"io"
	"strings"
	"testing"
)

// TestResizeMonitor_OtherPlatformNoOp verifies that on non-Unix platforms the
// resize monitor is a no-op: startResizeMonitor does not panic, Stop returns
// immediately without blocking, and Stop is idempotent.
func TestResizeMonitor_OtherPlatformNoOp(t *testing.T) {
	le := NewDefaultLineEditor(strings.NewReader(""), io.Discard)

	// Starting the monitor must not panic.
	le.startResizeMonitor()

	// Stop must not block or panic.
	le.Stop()

	// Stop is idempotent.
	le.Stop()

	// winchDone should be closed (no goroutine leak).
	select {
	case <-le.winchDone:
		// expected
	default:
		t.Fatal("winchDone should be closed on non-Unix platforms")
	}
}
