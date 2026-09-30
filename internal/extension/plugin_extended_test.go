//go:build !no_plugin

package extension_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
)

func TestPluginLoaderHTTPNoExtensions(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"extensions":[]}`)) //nolint:errcheck
	}))
	defer srv.Close()
	assertRemoteRejectedNoRequest(t, srv.URL+"/empty", &requests)
}

func TestPluginLoaderHTTPMissingKey(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{}`)) //nolint:errcheck
	}))
	defer srv.Close()
	assertRemoteRejectedNoRequest(t, srv.URL+"/missing", &requests)
}

func TestPluginLoaderHTTPEmptyBody(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write(nil) //nolint:errcheck
	}))
	defer srv.Close()
	assertRemoteRejectedNoRequest(t, srv.URL+"/empty-body", &requests)
}

func TestPluginLoaderHTTPConcurrent(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"extensions":[{"name":"unused"}]}`)) //nolint:errcheck
	}))
	defer srv.Close()
	loader := extension.NewDefaultPluginLoader()
	const workers = 16
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := loader.Load(context.Background(), srv.URL)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.ErrorIs(t, err, extension.ErrRemoteExtensionUnsupported)
	}
	assert.Zero(t, requests.Load())
}

func TestPluginLoaderCanceledContext(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer srv.Close()
	loader := extension.NewDefaultPluginLoader()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := loader.Load(ctx, srv.URL)
	require.ErrorIs(t, err, extension.ErrRemoteExtensionUnsupported)
	assert.Zero(t, requests.Load())
}

func TestPluginLoaderHTTPS(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer srv.Close()
	loader := extension.NewDefaultPluginLoader(extension.WithPluginHTTPClient(srv.Client()))
	_, err := loader.Load(context.Background(), srv.URL)
	require.ErrorIs(t, err, extension.ErrRemoteExtensionUnsupported)
	assert.Zero(t, requests.Load())
}
