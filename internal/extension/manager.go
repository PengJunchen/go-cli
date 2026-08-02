package extension

import (
	"context"
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// This file defines the extensionCoordinator, which drives the Phase 4
// extension lifecycle (init -> run -> shutdown), emits tracing spans for each
// stage and hook invocation, and provides a process-wide plugin-loader
// registry with a safe nil-default (mirroring internal/production/registry.go).

// extensionState is the lifecycle state of a managed extension.
type extensionState string

const (
	// extensionStatePending is the initial state before Init succeeds.
	extensionStatePending extensionState = "pending"
	// extensionStateRunning is entered once Init succeeds.
	extensionStateRunning extensionState = "running"
	// extensionStateStopped is entered after Shutdown completes.
	extensionStateStopped extensionState = "stopped"
)

// managedExtension tracks one extension's lifecycle state.
type managedExtension struct {
	ext   Extension
	state extensionState
}

// extensionCoordinator drives the extension lifecycle and hook dispatch while
// recording tracing spans for every stage. It is package-internal; the Phase 4
// lifecycle is exercised entirely by the package's own tests.
type extensionCoordinator struct {
	mu      sync.Mutex
	reg     ExtensionRegistry
	managed map[string]*managedExtension
}

// newExtensionCoordinator creates a coordinator over the given registry. When
// registry is nil it uses a fresh NewExtensionRegistry.
func newExtensionCoordinator(registry ExtensionRegistry) *extensionCoordinator {
	if registry == nil {
		registry = NewExtensionRegistry()
	}
	return &extensionCoordinator{
		reg:     registry,
		managed: make(map[string]*managedExtension),
	}
}

// registry returns the underlying registry used by the coordinator.
func (c *extensionCoordinator) registry() ExtensionRegistry { return c.reg }

// initExtension initializes an extension, emitting an `extension.init` span
// with the `extension_name` attribute, and transitions it to Running.
func (c *extensionCoordinator) initExtension(ctx context.Context, ext Extension) error {
	span, spanCtx := tracing.SpanFromContext(ctx, "extension.init", tracing.SpanKindInternal)
	defer span.End()
	span.SetAttributes(tracing.Attribute{Key: "extension_name", Value: ext.Name()})

	if err := ext.Init(spanCtx, c.reg); err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return err
	}
	span.SetStatus(tracing.SpanStatusOK, "")

	c.mu.Lock()
	c.managed[ext.Name()] = &managedExtension{ext: ext, state: extensionStateRunning}
	c.mu.Unlock()
	return nil
}

// shutdownExtension shuts down an extension, emitting an `extension.shutdown`
// span with the `extension_name` attribute, and transitions it to Stopped.
func (c *extensionCoordinator) shutdownExtension(ctx context.Context, ext Extension) error {
	span, spanCtx := tracing.SpanFromContext(ctx, "extension.shutdown", tracing.SpanKindInternal)
	defer span.End()
	span.SetAttributes(tracing.Attribute{Key: "extension_name", Value: ext.Name()})

	if err := ext.Shutdown(spanCtx); err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return err
	}
	span.SetStatus(tracing.SpanStatusOK, "")

	c.mu.Lock()
	if m, ok := c.managed[ext.Name()]; ok {
		m.state = extensionStateStopped
	}
	c.mu.Unlock()
	return nil
}

// runHook invokes a hook, emitting an `extension.hook` span with the
// `hook_name`, `event` and `action` attributes, and returns its HookResult.
func (c *extensionCoordinator) runHook(ctx context.Context, hook Hook, event HookEvent) HookResult {
	span, spanCtx := tracing.SpanFromContext(ctx, "extension.hook", tracing.SpanKindInternal)
	defer span.End()
	span.SetAttributes(
		tracing.Attribute{Key: "hook_name", Value: hook.Name()},
		tracing.Attribute{Key: "event", Value: event.Name},
	)

	result := hook.Handle(spanCtx, event)
	span.SetAttributes(tracing.Attribute{Key: "action", Value: string(result.Action)})
	span.SetStatus(tracing.SpanStatusOK, "")
	return result
}

// state returns the lifecycle state of the named extension, or Pending if it
// has not been initialized.
func (c *extensionCoordinator) state(name string) extensionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m, ok := c.managed[name]; ok {
		return m.state
	}
	return extensionStatePending
}

// process-wide plugin loader registry (nil-default, like production/registry.go).
var (
	pluginLoaderMu sync.Mutex
	defaultLoader  PluginLoader
)

// registerPluginLoader sets the active PluginLoader. A nil value resets to a
// fresh DefaultPluginLoader.
func registerPluginLoader(l PluginLoader) {
	pluginLoaderMu.Lock()
	defer pluginLoaderMu.Unlock()
	if l == nil {
		l = NewDefaultPluginLoader()
	}
	slog.Info("extension.register.plugin_loader", "name", l.Name())
	defaultLoader = l
}

// getPluginLoader returns the active PluginLoader, lazily defaulting to a
// DefaultPluginLoader when none has been registered.
func getPluginLoader() PluginLoader {
	pluginLoaderMu.Lock()
	defer pluginLoaderMu.Unlock()
	if defaultLoader == nil {
		defaultLoader = NewDefaultPluginLoader()
	}
	return defaultLoader
}
