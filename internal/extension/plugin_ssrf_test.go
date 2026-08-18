//go:build !no_plugin

package extension_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// TestPluginLoadHTTP_SSRSafeClient verifies that the default loader's HTTP
// fetcher uses the SSRF-safe client: requests to internal, link-local, and
// cloud-metadata IPs are blocked at dial time (AC-1).
func TestPluginLoadHTTP_SSRSafeClient(t *testing.T) {
	t.Parallel()
	loader := extension.NewDefaultPluginLoader()
	for _, endpoint := range []string{
		"https://10.0.0.1/ext",        // RFC 1918 private
		"https://169.254.169.254/ext", // cloud metadata / link-local
		"https://192.168.1.1/ext",     // RFC 1918 private
		"https://172.16.0.1/ext",      // RFC 1918 private
		"https://[fc00::1]/ext",       // IPv6 unique local
	} {
		_, err := loader.Load(context.Background(), endpoint)
		require.Error(t, err, "endpoint %q should be blocked", endpoint)
		// The block comes from the shared dial-time Control (DNS-rebinding
		// defense), surfaced as tools.ErrPrivateIP.
		assert.ErrorIs(t, err, tools.ErrPrivateIP, "endpoint %q", endpoint)
	}
}

// TestPluginLoadHTTP_LocalhostAllowed verifies that loopback development
// endpoints are not blocked by the SSRF-safe client, so a localhost HTTP
// server keeps working (AC-3).
func TestPluginLoadHTTP_LocalhostAllowed(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"extensions":[{"name":"local"}]}`)) //nolint:errcheck
	}))
	defer srv.Close()

	loader := extension.NewDefaultPluginLoader()
	exts, err := loader.Load(context.Background(), srv.URL)
	require.NoError(t, err)
	require.Len(t, exts, 1)
	assert.Equal(t, "local", exts[0].Name())
}

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
