package extension

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tools"
)

type pluginManagerOperation struct {
	kind   string
	done   chan struct{}
	cancel context.CancelFunc
	err    error
}

// PluginManager manages the lifecycle of extensions loaded via a PluginLoader.
// It loads extensions from configured paths, initializes them against an
// ExtensionRegistry, and shuts them down in reverse load order on teardown.
type PluginManager struct {
	loader      PluginLoader
	coordinator *extensionCoordinator

	mu               sync.RWMutex
	extensions       []Extension
	extensionNames   []string
	loadOp           *pluginManagerOperation
	initOp           *pluginManagerOperation
	shutdownOp       *pluginManagerOperation
	lifecycleStarted bool
	closing          bool
	shutdownComplete bool
	shutdownResult   error
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
// from being processed; every error is returned to the caller.
func (pm *PluginManager) Load(ctx context.Context, paths []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		pm.mu.Lock()
		if pm.closing || pm.lifecycleStarted {
			pm.mu.Unlock()
			return ErrExtensionManagerClosing
		}
		if active := pm.loadOp; active != nil {
			pm.mu.Unlock()
			if err := waitPluginManagerOperationDone(ctx, active); err != nil {
				return err
			}
			continue
		}
		loadCtx, cancel := context.WithCancel(ctx)
		op := &pluginManagerOperation{kind: "load", done: make(chan struct{}), cancel: cancel}
		pm.loadOp = op
		pm.mu.Unlock()

		err := pm.loadPaths(loadCtx, paths)
		cancel()
		pm.mu.Lock()
		op.err = err
		if pm.loadOp == op {
			pm.loadOp = nil
		}
		close(op.done)
		pm.mu.Unlock()
		return err
	}
}

func (pm *PluginManager) loadPaths(ctx context.Context, paths []string) error {
	var errs []error
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("plugin %q: %w", path, err))
			break
		}
		extensions, err := pm.loader.Load(ctx, path)
		if err != nil {
			slog.Warn("extension.plugin_manager.load_failed", "path", path, "err", err)
			errs = append(errs, fmt.Errorf("plugin %q: %w", path, err))
			continue
		}
		if err := pm.appendLoaded(path, extensions); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (pm *PluginManager) appendLoaded(path string, loaded []Extension) error {
	loadedNames := make([]string, len(loaded))
	for index, ext := range loaded {
		if ext == nil {
			return fmt.Errorf("plugin %q returned a nil extension", path)
		}
		name := ext.Name()
		if name == "" {
			return fmt.Errorf("plugin %q returned an extension with an empty name", path)
		}
		loadedNames[index] = name
	}

	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.closing || pm.lifecycleStarted {
		return fmt.Errorf("plugin %q: %w", path, ErrExtensionManagerClosing)
	}
	names := make(map[string]struct{}, len(pm.extensionNames)+len(loadedNames))
	for _, name := range pm.extensionNames {
		names[name] = struct{}{}
	}
	for _, name := range loadedNames {
		if _, exists := names[name]; exists {
			return fmt.Errorf("plugin %q: %w: %s", path, ErrExtensionNameConflict, name)
		}
		names[name] = struct{}{}
	}
	pm.extensions = append(pm.extensions, loaded...)
	pm.extensionNames = append(pm.extensionNames, loadedNames...)
	return nil
}

// Init initializes each loaded extension in load order. Failures are
// aggregated, but do not prevent later extensions from initializing unless the
// caller is canceled or a callback remains cleanup_pending.
func (pm *PluginManager) Init(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		pm.mu.Lock()
		if pm.closing {
			pm.mu.Unlock()
			return ErrExtensionManagerClosing
		}
		if active := pm.loadOp; active != nil {
			pm.mu.Unlock()
			if err := waitPluginManagerOperationDone(ctx, active); err != nil {
				return err
			}
			continue
		}
		if active := pm.initOp; active != nil {
			pm.mu.Unlock()
			return waitPluginManagerOperation(ctx, active)
		}
		pm.lifecycleStarted = true
		initCtx, cancel := context.WithCancel(ctx)
		op := &pluginManagerOperation{kind: "Init", done: make(chan struct{}), cancel: cancel}
		pm.initOp = op
		extensions := append([]Extension(nil), pm.extensions...)
		pm.mu.Unlock()

		var errs []error
		for _, ext := range extensions {
			err := pm.coordinator.initExtension(initCtx, ext)
			if err != nil {
				errs = append(errs, fmt.Errorf("extension %q Init: %w", ext.Name(), err))
				if errors.Is(err, ErrCleanupPending) || initCtx.Err() != nil {
					break
				}
			}
		}
		result := errors.Join(errs...)
		cancel()
		pm.mu.Lock()
		op.err = result
		if pm.initOp == op {
			pm.initOp = nil
		}
		close(op.done)
		pm.mu.Unlock()
		return result
	}
}

// Shutdown closes admission for new manager work, cancels in-progress Init,
// and shuts down loaded extensions in reverse order. It never holds the manager
// mutex while invoking loader or extension callbacks.
func (pm *PluginManager) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	pm.mu.Lock()
	pm.closing = true
	if pm.shutdownComplete {
		err := pm.shutdownResult
		pm.mu.Unlock()
		return err
	}
	op := pm.shutdownOp
	if op == nil {
		if pm.loadOp != nil && pm.loadOp.cancel != nil {
			pm.loadOp.cancel()
		}
		if pm.initOp != nil && pm.initOp.cancel != nil {
			pm.initOp.cancel()
		}
		op = &pluginManagerOperation{kind: "Shutdown", done: make(chan struct{})}
		pm.shutdownOp = op
		// Caller cancellation only ends this wait. The owned manager operation
		// must finish reverse-order cleanup after pending extension callbacks settle.
		go pm.runShutdownOwner(context.WithoutCancel(ctx), op)
	}
	pm.mu.Unlock()
	return waitPluginManagerShutdown(ctx, op)
}

func (pm *PluginManager) runShutdownOwner(ctx context.Context, op *pluginManagerOperation) {
	for {
		pm.mu.RLock()
		loadOp := pm.loadOp
		initOp := pm.initOp
		pm.mu.RUnlock()
		if loadOp == nil && initOp == nil {
			break
		}
		if loadOp != nil {
			<-loadOp.done
		}
		if initOp != nil {
			<-initOp.done
		}
	}

	pm.mu.RLock()
	extensions := append([]Extension(nil), pm.extensions...)
	pm.mu.RUnlock()
	var errs []error
	for i := len(extensions) - 1; i >= 0; i-- {
		ext := extensions[i]
		if err := pm.coordinator.shutdownExtension(ctx, ext); err != nil {
			errs = append(errs, fmt.Errorf("extension %q Shutdown: %w", ext.Name(), err))
		}
	}

	result := errors.Join(errs...)
	pm.mu.Lock()
	op.err = result
	pm.shutdownComplete = true
	pm.shutdownResult = result
	close(op.done)
	pm.mu.Unlock()
}

func waitPluginManagerOperationDone(ctx context.Context, op *pluginManagerOperation) error {
	select {
	case <-op.done:
		return nil
	case <-ctx.Done():
	}
	select {
	case <-op.done:
		return nil
	default:
		return ctx.Err()
	}
}

func waitPluginManagerOperation(ctx context.Context, op *pluginManagerOperation) error {
	select {
	case <-op.done:
		return op.err
	case <-ctx.Done():
	}
	select {
	case <-op.done:
		return op.err
	default:
		return ctx.Err()
	}
}

func waitPluginManagerShutdown(ctx context.Context, op *pluginManagerOperation) error {
	select {
	case <-op.done:
		return op.err
	case <-ctx.Done():
	}
	select {
	case <-op.done:
		return op.err
	default:
		return cleanupPending("", "Shutdown", ctx.Err())
	}
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
