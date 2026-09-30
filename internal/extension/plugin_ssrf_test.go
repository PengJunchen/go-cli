//go:build !no_plugin

package extension_test

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// TestUnifiedSSRF_DNSRebinding verifies the dial-time Control is the last line
// of defense against DNS rebinding: even when a private address is reachable
// (a listener is up), the SSRF-safe client refuses to connect before any TCP
// connection is established (AC-4).
func TestUnifiedSSRF_DNSRebinding(t *testing.T) {
	t.Parallel()
	// A reachable listener on a loopback address simulates the destination a
	// rebinding attack would redirect a hostname to at dial time.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	accepted := make(chan struct{}, 1)
	go func() {
		conn, aErr := ln.Accept()
		if conn != nil {
			select {
			case accepted <- struct{}{}:
			default:
			}
			_ = conn.Close()
			return
		}
		_ = aErr
	}()

	addr := ln.Addr().String() // 127.0.0.1:<port>
	// NewSSRFSafeHTTPClient blocks loopback at dial time. Even though a
	// listener is reachable at this address, the Control function refuses the
	// connection before it is established — the defense that catches a rebinding
	// attack a pre-check (ValidateURL) would miss.
	client := tools.NewSSRFSafeHTTPClient(2 * time.Second)
	_, err = client.Get("http://" + addr + "/")
	require.Error(t, err)
	assert.ErrorIs(t, err, tools.ErrPrivateIP)

	// No connection should have been established.
	select {
	case <-accepted:
		t.Fatal("connection was established despite SSRF dial-time control")
	case <-time.After(150 * time.Millisecond):
		// dial-time control blocked the connection before it was made
	}
}
