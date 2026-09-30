//go:build !no_plugin

package extension

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"plugin"
	"strings"
)

// This file defines the PluginLoader contract and the default native Go
// plugin implementation. Remote descriptors are rejected because this build
// has no remote call transport; grpc:// remains an explicitly unsupported
// transport.

// ErrUnsupportedRPC is returned when a load path requests gRPC, which is not
// embedded in this build.
var ErrUnsupportedRPC = errors.New("extension: gRPC RPC loading is not embedded in this build")

var factorySymbols = []string{"New", "LoadExtensions"}

// PluginLoader loads extensions from a local shared object or another resource
// location supported by its implementation.
type PluginLoader interface {
	Name() string
	Load(ctx context.Context, path string) ([]Extension, error)
}

// DefaultPluginLoader loads native Go shared objects.
type DefaultPluginLoader struct {
	name       string
	httpClient *http.Client
}

var _ PluginLoader = (*DefaultPluginLoader)(nil)

// PluginLoaderOption configures a DefaultPluginLoader.
type PluginLoaderOption func(*DefaultPluginLoader)

// WithPluginHTTPClient is retained for source compatibility. Remote extension
// descriptors are unavailable, so this client is never used.
func WithPluginHTTPClient(client *http.Client) PluginLoaderOption {
	return func(loader *DefaultPluginLoader) { loader.httpClient = client }
}

// NewDefaultPluginLoader creates a DefaultPluginLoader. A nil-safe default is
// provided by the process-wide registry in manager.go.
func NewDefaultPluginLoader(options ...PluginLoaderOption) PluginLoader {
	loader := &DefaultPluginLoader{name: "default-plugin-loader"}
	for _, option := range options {
		option(loader)
	}
	return loader
}

func (loader *DefaultPluginLoader) Name() string {
	if loader.name == "" {
		return "default-plugin-loader"
	}
	return loader.name
}

// Load rejects HTTP(S) before it can issue a request, then routes local paths
// to the Go shared-object loader and grpc:// paths to ErrUnsupportedRPC.
func (loader *DefaultPluginLoader) Load(ctx context.Context, path string) ([]Extension, error) {
	slog.Info("extension.plugin.load", "loader", loader.Name(), "path", safeExtensionPath(path))
	if isRemoteExtensionPath(path) {
		return nil, ErrRemoteExtensionUnsupported
	}
	if strings.HasPrefix(strings.ToLower(path), "grpc://") {
		return nil, ErrUnsupportedRPC
	}
	return loader.loadGoPlugin(path)
}

func (loader *DefaultPluginLoader) loadGoPlugin(path string) ([]Extension, error) {
	loaded, err := plugin.Open(path)
	if err != nil {
		return nil, fmt.Errorf("extension: open plugin %q: %w", path, err)
	}
	var factoryErr error
	for _, symbol := range factorySymbols {
		value, lookupErr := loaded.Lookup(symbol)
		if lookupErr != nil {
			continue
		}
		factory, ok := value.(func() ([]Extension, error))
		if !ok {
			factoryErr = fmt.Errorf("extension: plugin symbol %q has unexpected signature", symbol)
			continue
		}
		extensions, factoryErr := factory()
		if factoryErr != nil {
			return nil, fmt.Errorf("extension: plugin factory %q: %w", symbol, factoryErr)
		}
		if len(extensions) == 0 {
			return nil, fmt.Errorf("extension: plugin %q factory %q returned no extensions", path, symbol)
		}
		slog.Info("extension.plugin.loaded", "path", path, "symbol", symbol, "count", len(extensions))
		return extensions, nil
	}
	if factoryErr != nil {
		return nil, factoryErr
	}
	return nil, fmt.Errorf("extension: plugin %q exposes neither %s factory symbol", path, strings.Join(factorySymbols, " nor "))
}
