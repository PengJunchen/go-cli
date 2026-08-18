//go:build !no_plugin

package extension_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
)

// the default loader routes an http(s) path to the JSON-over-HTTP
// loader and returns the wrapped rpc extensions.
func TestPluginLoaderHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck // response encoding is best-effort
			"extensions": []map[string]any{{"name": "a"}, {"name": "b"}},
		})
	}))
	defer srv.Close()

	loader := extension.NewDefaultPluginLoader()
	exts, err := loader.Load(context.Background(), srv.URL)
	require.NoError(t, err)
	require.Len(t, exts, 2)
	assert.Equal(t, "a", exts[0].Name())
	assert.Equal(t, "b", exts[1].Name())

	// Loaded extensions satisfy the Extension contract.
	require.NoError(t, exts[0].Init(context.Background(), extension.NewExtensionRegistry()))
	require.NoError(t, exts[0].Shutdown(context.Background()))
}

// the gRPC scheme is recognized but flagged unsupported in the
// zero-dependency build.
func TestPluginLoaderGRPCUnsupported(t *testing.T) {
	loader := extension.NewDefaultPluginLoader()
	_, err := loader.Load(context.Background(), "grpc://127.0.0.1:50051")
	require.ErrorIs(t, err, extension.ErrUnsupportedRPC)
}

// loading a nonexistent .so path surfaces a clear, wrapped error.
func TestPluginLoaderingNonExistentPlugin(t *testing.T) {
	loader := extension.NewDefaultPluginLoader()
	missing := filepath.Join(t.TempDir(), "nonexistent.so")
	_, err := loader.Load(context.Background(), missing)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open plugin")
	assert.Contains(t, err.Error(), missing)
}

// an HTTP endpoint that fails to decode returns a clear error.
func TestPluginLoaderingHTTPBadPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not-json")) //nolint:errcheck // response writing is best-effort
	}))
	defer srv.Close()

	loader := extension.NewDefaultPluginLoader()
	_, err := loader.Load(context.Background(), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode")
}

// an HTTP endpoint returning non-200 surfaces the status in the error.
func TestPluginLoaderingHTTPNonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	loader := extension.NewDefaultPluginLoader()
	_, err := loader.Load(context.Background(), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

// TestPluginLoaderSSRFBlocksPrivateIPs verifies that endpoints at private or
// link-local IP ranges are rejected (AC-1). HTTPS is used so the HTTPS gate
// passes and the IP check is the layer that rejects.
func TestPluginLoaderSSRFBlocksPrivateIPs(t *testing.T) {
	loader := extension.NewDefaultPluginLoader()
	for _, endpoint := range []string{
		"https://169.254.1.1/ext",    // link-local
		"https://10.0.0.1/ext",       // RFC 1918 10/8
		"https://172.16.0.1/ext",     // RFC 1918 172.16/12 lower bound
		"https://172.31.255.255/ext", // RFC 1918 172.16/12 upper bound
		"https://192.168.1.1/ext",    // RFC 1918 192.168/16
	} {
		_, err := loader.Load(context.Background(), endpoint)
		require.Error(t, err, "endpoint %q should be blocked", endpoint)
		assert.Contains(t, err.Error(), "blocked")
	}
}

// TestPluginLoaderSSRFResponseSizeLimit verifies that a response body exceeding
// 10MB is rejected (AC-2). The test server runs on 127.0.0.1 so the HTTPS and
// IP checks pass, leaving the size check as the only rejection point.
func TestPluginLoaderSSRFResponseSizeLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"extensions":[`)) //nolint:errcheck // best-effort
		chunk := []byte(strings.Repeat(" ", 4096))
		for i := 0; i < 10*1024*1024/4096+1; i++ {
			_, _ = w.Write(chunk) //nolint:errcheck // best-effort
		}
	}))
	defer srv.Close()

	loader := extension.NewDefaultPluginLoader()
	_, err := loader.Load(context.Background(), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
}

// TestPluginLoaderSSRFRejectsNonHTTPS verifies that non-HTTPS endpoints are
// rejected when the host is not localhost (AC-3).
func TestPluginLoaderSSRFRejectsNonHTTPS(t *testing.T) {
	loader := extension.NewDefaultPluginLoader()
	_, err := loader.Load(context.Background(), "http://example.com/ext")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTPS")
}
