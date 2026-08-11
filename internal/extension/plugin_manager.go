package extension

import (
	"context"
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// PluginManager manages the lifecycle of extensions loaded via a PluginLoader.
// It loads extensions from configured paths, initializes them against an
// ExtensionRegistry, and shuts them down in reverse order on teardown.
type PluginManager struct {
	loader      PluginLoader
	coordinator *extensionCoordinator

	mu         sync.RWMutex
	extensions []Extension
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
func (pm *PluginManager) Load(ctx context.Context, paths []string) error {
	for _, path := range paths {
		exts, err := pm.loader.Load(ctx, path)
		if err != nil {
			slog.Warn("extension.plugin_manager.load_failed", "path", path, "err", err)
			continue
		}
		pm.mu.Lock()
		pm.extensions = append(pm.extensions, exts...)
		pm.mu.Unlock()
	}
	return nil
}

// Init initializes every loaded extension by calling Init against the internal
// ExtensionRegistry. An init error for one extension does not prevent the
// remaining extensions from being initialized.
func (pm *PluginManager) Init(ctx context.Context) error {
	pm.mu.RLock()
	exts := make([]Extension, len(pm.extensions))
	copy(exts, pm.extensions)
	pm.mu.RUnlock()
	for _, ext := range exts {
		if err := pm.coordinator.initExtension(ctx, ext); err != nil {
			slog.Warn("extension.plugin_manager.init_failed", "extension", ext.Name(), "err", err)
		}
	}
	return nil
}

// Shutdown shuts down every loaded extension in reverse order. A shutdown
// error for one extension does not prevent the remaining extensions from being
// shut down.
func (pm *PluginManager) Shutdown(ctx context.Context) error {
	pm.mu.RLock()
	exts := make([]Extension, len(pm.extensions))
	copy(exts, pm.extensions)
	pm.mu.RUnlock()
	for i := len(exts) - 1; i >= 0; i-- {
		ext := exts[i]
		if err := pm.coordinator.shutdownExtension(ctx, ext); err != nil {
			slog.Warn("extension.plugin_manager.shutdown_failed", "extension", ext.Name(), "err", err)
		}
	}
	return nil
}

// Extensions returns the extensions loaded by Load. The slice is empty before
// Load is called.
func (pm *PluginManager) Extensions() []Extension {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	out := make([]Extension, len(pm.extensions))
	copy(out, pm.extensions)
	return out
}

// Registry returns the ExtensionRegistry that extensions register their
// building blocks into during Init.
func (pm *PluginManager) Registry() ExtensionRegistry {
	return pm.coordinator.registry()
}

// Tools returns all tools registered by extensions during Init. It is empty
// before Init is called.
func (pm *PluginManager) Tools() []tools.ToolDefinition {
	if der, ok := pm.coordinator.registry().(*DefaultExtensionRegistry); ok {
		return der.AllTools()
	}
	return nil
}

// Hooks returns all hooks registered by extensions during Init. It is empty
// before Init is called.
func (pm *PluginManager) Hooks() []Hook {
	if der, ok := pm.coordinator.registry().(*DefaultExtensionRegistry); ok {
		return der.AllHooks()
	}
	return nil
}

// Middleware returns all middleware registered by extensions during Init. It is
// empty before Init is called.
func (pm *PluginManager) Middleware() []Middleware {
	if der, ok := pm.coordinator.registry().(*DefaultExtensionRegistry); ok {
		return der.AllMiddleware()
	}
	return nil
}
