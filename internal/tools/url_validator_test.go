package tools

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateURL_RejectsPrivateIPs(t *testing.T) {
	tests := []string{
		"http://127.0.0.1/",
		"http://10.0.0.1/",
		"http://172.16.0.1/",
		"http://192.168.1.1/",
		"http://169.254.169.254/latest/meta-data/",
	}
	for _, rawURL := range tests {
		t.Run(rawURL, func(t *testing.T) {
			err := ValidateURL(rawURL)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrPrivateIP)
		})
	}
}

func TestValidateURL_RejectsInvalidScheme(t *testing.T) {
	tests := []string{
		"file:///etc/passwd",
		"ftp://example.com/",
	}
	for _, rawURL := range tests {
		t.Run(rawURL, func(t *testing.T) {
			err := ValidateURL(rawURL)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidScheme)
		})
	}
}

func TestValidateURL_AcceptsPublicURLs(t *testing.T) {
	tests := []string{
		"http://example.com",
		"https://api.openai.com",
	}
	for _, rawURL := range tests {
		t.Run(rawURL, func(t *testing.T) {
			err := ValidateURL(rawURL)
			// Skip when DNS is unavailable (e.g. offline CI sandbox) so the
			// test suite remains green without network access.
			if err != nil {
				var dnsErr *net.DNSError
				if assert.ErrorAs(t, err, &dnsErr) {
					t.Skipf("DNS resolution unavailable, skipping: %v", err)
				}
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateURL_RejectsIPv6Loopback(t *testing.T) {
	err := ValidateURL("http://[::1]/")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPrivateIP)
}

func TestSSRFSafeHTTPClient(t *testing.T) {
	// httptest.NewServer binds to 127.0.0.1; the SSRF-safe client must
	// refuse to dial that address.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewSSRFSafeHTTPClient(5 * time.Second)
	resp, err := client.Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected error when connecting to private IP, got nil")
	}
	assert.ErrorIs(t, err, ErrPrivateIP)
}
