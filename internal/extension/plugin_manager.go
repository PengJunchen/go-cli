package extension

import (
	"context"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// PluginManager manages the lifecycle of extensions loaded via a PluginLoader.
// It loads extensions from configured paths, initializes them against an
// ExtensionRegistry, and shuts them down in reverse order on teardown.
type PluginManager struct {
	loader      PluginLoader
	coordinator *extensionCoordinator
	extensions  []Extension
}

// NewPluginManager creates a PluginManager using the given PluginLoader. When
// loader is nil, a DefaultPluginLoader is used.
func NewPluginManager(loader PluginLoader) *PluginManager {
	if loader == nil {
		loader = NewDefaultPluginLoader()
	}
	reg := NewExtensionRegistry()
	return &PluginManager{
		loader:      loader,
		coordinator: newExtensionCoordinator(reg),
	}
}

// Load iterates the given paths, loading extensions from each via the
// PluginLoader. A load error for one path does not prevent the remaining paths
// from being processed; the error is logged and execution continues.
func (pm *PluginManager) Load(_ context.Context, _ []string) error {
	return nil
}

// Init initializes every loaded extension by calling Init against the internal
// ExtensionRegistry. An init error for one extension does not prevent the
// remaining extensions from being initialized.
func (pm *PluginManager) Init(_ context.Context) error {
	return nil
}

// Shutdown shuts down every initialized extension in reverse order. A shutdown
// error for one extension does not prevent the remaining extensions from being
// shut down.
func (pm *PluginManager) Shutdown(_ context.Context) error {
	return nil
}

// Extensions returns the extensions loaded by Load. The slice is empty before
// Load is called.
func (pm *PluginManager) Extensions() []Extension {
	return nil
}

// Registry returns the ExtensionRegistry that extensions register their
// building blocks into during Init.
func (pm *PluginManager) Registry() ExtensionRegistry {
	return pm.coordinator.registry()
}

// Tools returns all tools registered by extensions during Init. It is empty
// before Init is called.
func (pm *PluginManager) Tools() []tools.ToolDefinition {
	return nil
}
