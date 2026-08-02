//go:build no_plugin

package extension

import (
	"context"
	"errors"
)

// This file is the build-tag stub selected when the extension package is
// compiled with `-tags no_plugin`. The real plugin.go (build tag !no_plugin)
// depends on the stdlib `plugin` package and the full JSON-over-HTTP / gRPC
// routing. When plugins are disabled the package still builds by providing the
// same PluginLoader contract as a stub that returns ErrUnsupportedRPC for every
// load, so manager.go's process-wide loader registry and callers that merely
// reference the interface keep compiling.

// ErrUnsupportedRPC is returned by the no_plugin stub loader, signalling that
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
	name string
}

// NewDefaultPluginLoader creates a DefaultPluginLoader for the no_plugin build.
func NewDefaultPluginLoader() PluginLoader {
	return &DefaultPluginLoader{name: "default-plugin-loader"}
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
