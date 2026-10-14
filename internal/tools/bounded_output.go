package tools

import (
	"bytes"
	"io"
	"sync"
)

// boundedOutputCapture drains arbitrarily large child output while retaining
// only one shared byte budget across stdout and stderr.
type boundedOutputCapture struct {
	mu        sync.Mutex
	max       int
	used      int
	stdout    bytes.Buffer
	stderr    bytes.Buffer
	combined  bytes.Buffer
	truncated bool
}

func newBoundedOutputCapture(max int) *boundedOutputCapture {
	if max < 0 {
		max = 0
	}
	return &boundedOutputCapture{max: max}
}

func (c *boundedOutputCapture) writer(stream string) io.Writer {
	return boundedOutputStream{capture: c, stream: stream}
}

func (c *boundedOutputCapture) write(stream string, data []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	remaining := c.max - c.used
	keep := len(data)
	if keep > remaining {
		keep = remaining
	}
	if keep > 0 {
		if stream == "stderr" {
			_, _ = c.stderr.Write(data[:keep])
		} else {
			_, _ = c.stdout.Write(data[:keep])
		}
		_, _ = c.combined.Write(data[:keep])
		c.used += keep
	}
	if keep < len(data) {
		c.truncated = true
	}
	// Returning the full input length is essential: the os/exec copier must
	// keep draining the pipe after the capture budget is exhausted.
	return len(data), nil
}

func (c *boundedOutputCapture) snapshot(marker string) (stdout, stderr, combined string, truncated bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	stdout = c.stdout.String()
	stderr = c.stderr.String()
	combined = c.combined.String()
	truncated = c.truncated
	if truncated {
		combined += marker
	}
	return stdout, stderr, combined, truncated
}

type boundedOutputStream struct {
	capture *boundedOutputCapture
	stream  string
}

func (w boundedOutputStream) Write(data []byte) (int, error) {
	return w.capture.write(w.stream, data)
}
