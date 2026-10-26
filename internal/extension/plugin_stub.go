//go:build no_plugin

package extension

//exempt:scan008 // build-tag stub, no executable logic

import (
	"context"
	"errors"
	"net/http"
)

// This variant removes the native shared-object loader. The public loader
// constructor and options remain source compatible; process-based SDK loaders
// are selected separately by the extension manager and remain available.

// ErrUnsupportedRPC is returned by the no_plugin stub loader, signaling that
// plugin loading is disabled in this build.
var ErrUnsupportedRPC = errors.New("extension: plugin loading is not embedded in this build (no_plugin)")

// PluginLoader loads extensions from a resource location.
type PluginLoader interface {
	// Name returns the loader identifier.
	Name() string
	// Load loads and instantiates extensions from the given path or endpoint.
	Load(ctx context.Context, path string) ([]Extension, error)
}

// DefaultPluginLoader is the no_plugin stub of the default PluginLoader.
type DefaultPluginLoader struct {
	name       string
	httpClient *http.Client
}

// NewDefaultPluginLoader creates a DefaultPluginLoader for the no_plugin build.
func NewDefaultPluginLoader(options ...PluginLoaderOption) PluginLoader {
	loader := &DefaultPluginLoader{name: "default-plugin-loader"}
	for _, option := range options {
		option(loader)
	}
	return loader
}

var _ PluginLoader = (*DefaultPluginLoader)(nil)

// Name returns the loader identifier.
func (l *DefaultPluginLoader) Name() string {
	if l.name == "" {
		return "default-plugin-loader"
	}
	return l.name
}

// Load always returns ErrUnsupportedRPC because plugin loading is disabled in
// the no_plugin build.
func (l *DefaultPluginLoader) Load(_ context.Context, _ string) ([]Extension, error) {
	return nil, ErrUnsupportedRPC
}

// PluginLoaderOption configures a loader in either build variant.
type PluginLoaderOption func(*DefaultPluginLoader)

// WithPluginHTTPClient retains API parity; remote plugins remain unsupported.
func WithPluginHTTPClient(client *http.Client) PluginLoaderOption {
	return func(loader *DefaultPluginLoader) { loader.httpClient = client }
}
