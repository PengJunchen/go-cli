package cli

import (
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestTerminalWidth_InvalidatedOnReadLine verifies that invalidateTermWidth
// clears the cached value so the next terminalWidth() call re-queries the
// actual terminal size instead of returning a stale width.
func TestTerminalWidth_InvalidatedOnReadLine(t *testing.T) {
	le := NewDefaultLineEditor(strings.NewReader(""), io.Discard)

	// Store a stale width that does not match the real terminal.
	le.termWidth.Store(120)

	// Before invalidation, the cached value is returned.
	assert.Equal(t, 120, le.terminalWidth())

	// Invalidate the cache.
	le.invalidateTermWidth()

	// After invalidation, terminalWidth re-queries. For non-TTY input,
	// queryTermWidth falls back to 80.
	assert.Equal(t, 80, le.terminalWidth())
}

// TestTerminalWidth_ConcurrentSafe starts the resize monitor and exercises
// terminalWidth() from 100 goroutines concurrently. The test passes under
// -race if no data race is detected.
func TestTerminalWidth_ConcurrentSafe(t *testing.T) {
	le := NewDefaultLineEditor(strings.NewReader(""), io.Discard)
	le.startResizeMonitor()
	defer le.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = le.terminalWidth()
		}()
	}
	wg.Wait()
	// Reaching here without the race detector firing means the test passes.
}
