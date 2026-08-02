//go:build !no_plugin

package extension_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
)

// AC-5/AC-7: the default loader routes an http(s) path to the JSON-over-HTTP
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

// AC-8: the gRPC scheme is recognized but flagged unsupported in the
// zero-dependency build.
func TestPluginLoaderGRPCUnsupported(t *testing.T) {
	loader := extension.NewDefaultPluginLoader()
	_, err := loader.Load(context.Background(), "grpc://127.0.0.1:50051")
	require.ErrorIs(t, err, extension.ErrUnsupportedRPC)
}

// AC-7: loading a nonexistent .so path surfaces a clear, wrapped error.
func TestPluginLoaderingNonExistentPlugin(t *testing.T) {
	loader := extension.NewDefaultPluginLoader()
	missing := filepath.Join(t.TempDir(), "nonexistent.so")
	_, err := loader.Load(context.Background(), missing)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open plugin")
	assert.Contains(t, err.Error(), missing)
}

// AC-7: an HTTP endpoint that fails to decode returns a clear error.
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

// AC-7: an HTTP endpoint returning non-200 surfaces the status in the error.
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
