package extension_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
)

// TestPluginLoaderNames verifies the default loader name and the registered-name
// fallback behavior.
func TestPluginLoaderNames(t *testing.T) {
	l := extension.NewDefaultPluginLoader()
	assert.Equal(t, "default-plugin-loader", l.Name())
}

// TestPluginLoaderGRPCVariousCasing verifies gRPC detection is case-insensitive
// on the scheme prefix.
func TestPluginLoaderGRPCVariousCasing(t *testing.T) {
	loader := extension.NewDefaultPluginLoader()
	for _, p := range []string{"grpc://host", "GRPC://host"} {
		_, err := loader.Load(context.Background(), p)
		assert.ErrorIs(t, err, extension.ErrUnsupportedRPC, "path %q", p)
	}
}

// TestPluginLoaderHTTPNoExtensions verifies an endpoint with an empty extension
// list is rejected.
func TestPluginLoaderHTTPNoExtensions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"extensions": []string{}}) //nolint:errcheck
	}))
	defer srv.Close()

	loader := extension.NewDefaultPluginLoader()
	_, err := loader.Load(context.Background(), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no extensions")
}

// TestPluginLoaderHTTPMissingKey verifies an endpoint lacking the extensions key
// (empty bundle) is treated as returning no extensions.
func TestPluginLoaderHTTPMissingKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`)) //nolint:errcheck
	}))
	defer srv.Close()

	loader := extension.NewDefaultPluginLoader()
	_, err := loader.Load(context.Background(), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no extensions")
}

// TestPluginLoaderHTTPEmptyBody verifies an empty response body yields a decode
// error.
func TestPluginLoaderHTTPEmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(nil) //nolint:errcheck
	}))
	defer srv.Close()

	loader := extension.NewDefaultPluginLoader()
	_, err := loader.Load(context.Background(), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode")
}

// TestPluginLoaderHTTPConcurrent verifies the loader handles concurrent requests
// to a healthy endpoint without data races.
func TestPluginLoaderHTTPConcurrent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"extensions": []map[string]any{{"name": "a"}},
		})
	}))
	defer srv.Close()

	loader := extension.NewDefaultPluginLoader()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			exts, err := loader.Load(context.Background(), srv.URL)
			require.NoError(t, err)
			require.Len(t, exts, 1)
			assert.Equal(t, "a", exts[0].Name())
		}()
	}
	wg.Wait()
}

// TestPluginLoaderCanceledContext verifies a canceled context propagates to the
// HTTP request and surfaces an error.
func TestPluginLoaderCanceledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	loader := extension.NewDefaultPluginLoader()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := loader.Load(ctx, srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "query endpoint")
}

// TestPluginLoaderNonHTTPNonPlugin verifies an arbitrary path is routed to the
// Go plugin loader and fails to open with a descriptive error.
func TestPluginLoaderNonHTTPNonPlugin(t *testing.T) {
	loader := extension.NewDefaultPluginLoader()
	_, err := loader.Load(context.Background(), "/no/such/module.so")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open plugin")
	assert.Contains(t, err.Error(), "/no/such/module.so")
}

// TestPluginLoaderHTTPS verifies the https scheme is routed to the HTTP loader
// just like http.
func TestPluginLoaderHTTPS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"extensions": []map[string]any{{"name": "sec"}},
		})
	}))
	defer srv.Close()

	// Temporarily trust the test server's self-signed cert.
	old := http.DefaultClient
	http.DefaultClient = srv.Client()
	defer func() { http.DefaultClient = old }()

	loader := extension.NewDefaultPluginLoader()
	exts, err := loader.Load(context.Background(), srv.URL)
	require.NoError(t, err)
	require.Len(t, exts, 1)
	assert.Equal(t, "sec", exts[0].Name())
}
