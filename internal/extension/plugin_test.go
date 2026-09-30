//go:build !no_plugin

package extension_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
)

func TestPluginLoaderRejectsHTTPDescriptorAsUnavailable(t *testing.T) {
	const canary = "remote-url-secret-canary-4a91"
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"extensions":[{"name":"must-not-load"}]}`)) //nolint:errcheck
	}))
	defer srv.Close()
	endpoint := srv.URL + "/extensions?token=" + canary

	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })
	loader := extension.NewDefaultPluginLoader()
	_, err := loader.Load(context.Background(), endpoint)
	require.ErrorIs(t, err, extension.ErrRemoteExtensionUnsupported)
	assert.Zero(t, requests.Load())
	assert.NotContains(t, err.Error(), canary)
	assert.NotContains(t, logs.String(), canary)
	assert.Contains(t, logs.String(), "<remote-extension-path>")

	manager := extension.NewPluginManager(loader)
	managerErr := manager.Load(context.Background(), []string{endpoint})
	require.ErrorIs(t, managerErr, extension.ErrRemoteExtensionUnsupported)
	assert.NotContains(t, managerErr.Error(), canary)
	assert.NotContains(t, logs.String(), canary)
	assert.Zero(t, requests.Load())
}

func TestPluginLoaderRejectsHTTPSDescriptorAsUnavailable(t *testing.T) {
	const canary = "https-url-secret-canary-390a"
	var requests atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"extensions":[]}`)) //nolint:errcheck
	}))
	defer srv.Close()
	loader := extension.NewDefaultPluginLoader(extension.WithPluginHTTPClient(srv.Client()))
	_, err := loader.Load(context.Background(), srv.URL+"/plugins?key="+canary)
	require.ErrorIs(t, err, extension.ErrRemoteExtensionUnsupported)
	assert.Zero(t, requests.Load())
	assert.NotContains(t, err.Error(), canary)
}

func TestPluginLoaderCanceledContextRejectsHTTPBeforeContext(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := extension.NewDefaultPluginLoader().Load(ctx, srv.URL+"/plugins")
	require.ErrorIs(t, err, extension.ErrRemoteExtensionUnsupported)
	assert.Zero(t, requests.Load())
}

func TestPluginLoaderGRPCUnsupported(t *testing.T) {
	loader := extension.NewDefaultPluginLoader()
	_, err := loader.Load(context.Background(), "grpc://127.0.0.1:50051")
	require.ErrorIs(t, err, extension.ErrUnsupportedRPC)
}

func TestPluginLoaderGRPCVariousCasing(t *testing.T) {
	loader := extension.NewDefaultPluginLoader()
	for _, path := range []string{"grpc://host", "GRPC://host"} {
		_, err := loader.Load(context.Background(), path)
		assert.ErrorIs(t, err, extension.ErrUnsupportedRPC, "path %q", path)
	}
}

func TestPluginLoaderGRPCPathRedactsSecrets(t *testing.T) {
	const canary = "grpc-url-secret-canary-42f8"
	path := "grpc://user:" + canary + "@host:50051/extensions?token=" + canary
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	loader := extension.NewDefaultPluginLoader()
	_, err := loader.Load(context.Background(), path)
	require.ErrorIs(t, err, extension.ErrUnsupportedRPC)
	assert.NotContains(t, err.Error(), canary)
	assert.NotContains(t, logs.String(), canary)
	assert.Contains(t, logs.String(), "<remote-extension-path>")

	managerErr := extension.NewPluginManager(loader).Load(context.Background(), []string{path})
	require.ErrorIs(t, managerErr, extension.ErrUnsupportedRPC)
	assert.NotContains(t, managerErr.Error(), canary)
	assert.NotContains(t, logs.String(), canary)
}

func TestPluginLoaderNames(t *testing.T) {
	assert.Equal(t, "default-plugin-loader", extension.NewDefaultPluginLoader().Name())
}

func TestPluginLoaderingNonExistentPlugin(t *testing.T) {
	loader := extension.NewDefaultPluginLoader()
	missing := filepath.Join(t.TempDir(), "nonexistent.so")
	_, err := loader.Load(context.Background(), missing)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open plugin")
	assert.Contains(t, err.Error(), missing)
}

func assertRemoteRejectedNoRequest(t *testing.T, path string, requests *atomic.Int32) {
	t.Helper()
	_, err := extension.NewDefaultPluginLoader().Load(context.Background(), path)
	require.ErrorIs(t, err, extension.ErrRemoteExtensionUnsupported)
	assert.Zero(t, requests.Load())
}

func TestPluginLoaderRejectsHTTPVariantsBeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(strings.Repeat("x", 128))) //nolint:errcheck
	}))
	defer srv.Close()
	for _, path := range []string{
		srv.URL + "/empty?extensions=[]",
		"http://10.0.0.1/extensions",
		"https://169.254.169.254/latest/meta-data",
		"https://192.168.1.1/extensions",
	} {
		assertRemoteRejectedNoRequest(t, path, &requests)
	}
}
