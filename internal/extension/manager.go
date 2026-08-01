package extension

import (
	"context"
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// This file defines the ExtensionCoordinator, which drives the Phase 4
// extension lifecycle (init -> run -> shutdown), emits tracing spans for each
// stage and hook invocation, and provides a process-wide plugin-loader
// registry with a safe nil-default (mirroring internal/production/registry.go).

// ExtensionState is the lifecycle state of a managed extension.
type ExtensionState string

const (
	// ExtensionStatePending is the initial state before Init succeeds.
	ExtensionStatePending ExtensionState = "pending"
	// ExtensionStateRunning is entered once Init succeeds.
	ExtensionStateRunning ExtensionState = "running"
	// ExtensionStateStopped is entered after Shutdown completes.
	ExtensionStateStopped ExtensionState = "stopped"
)

// managedExtension tracks one extension's lifecycle state.
type managedExtension struct {
	ext   Extension
	state ExtensionState
}

// ExtensionCoordinator drives the extension lifecycle and hook dispatch while
// recording tracing spans for every stage.
type ExtensionCoordinator struct {
	mu       sync.Mutex
	registry ExtensionRegistry
	managed  map[string]*managedExtension
}

// NewExtensionCoordinator creates a coordinator over the given registry. When
// registry is nil it uses a fresh NewExtensionRegistry.
func NewExtensionCoordinator(registry ExtensionRegistry) *ExtensionCoordinator {
	if registry == nil {
		registry = NewExtensionRegistry()
	}
	return &ExtensionCoordinator{
		registry: registry,
		managed:  make(map[string]*managedExtension),
	}
}

// Registry returns the underlying registry used by the coordinator.
func (c *ExtensionCoordinator) Registry() ExtensionRegistry { return c.registry }

// InitExtension initializes an extension, emitting an `extension.init` span
// with the `extension_name` attribute, and transitions it to Running.
func (c *ExtensionCoordinator) InitExtension(ctx context.Context, ext Extension) error {
	span, spanCtx := tracing.SpanFromContext(ctx, "extension.init", tracing.SpanKindInternal)
	defer span.End()
	span.SetAttributes(tracing.Attribute{Key: "extension_name", Value: ext.Name()})

	if err := ext.Init(spanCtx, c.registry); err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return err
	}
	span.SetStatus(tracing.SpanStatusOK, "")

	c.mu.Lock()
	c.managed[ext.Name()] = &managedExtension{ext: ext, state: ExtensionStateRunning}
	c.mu.Unlock()
	return nil
}

// ShutdownExtension shuts down an extension, emitting an `extension.shutdown`
// span with the `extension_name` attribute, and transitions it to Stopped.
func (c *ExtensionCoordinator) ShutdownExtension(ctx context.Context, ext Extension) error {
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
		m.state = ExtensionStateStopped
	}
	c.mu.Unlock()
	return nil
}

// RunHook invokes a hook, emitting an `extension.hook` span with the
// `hook_name`, `event` and `action` attributes, and returns its HookResult.
func (c *ExtensionCoordinator) RunHook(ctx context.Context, hook Hook, event HookEvent) HookResult {
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

// State returns the lifecycle state of the named extension, or Pending if it
// has not been initialized.
func (c *ExtensionCoordinator) State(name string) ExtensionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m, ok := c.managed[name]; ok {
		return m.state
	}
	return ExtensionStatePending
}

// process-wide plugin loader registry (nil-default, like production/registry.go).
var (
	pluginLoaderMu sync.Mutex
	defaultLoader  PluginLoader
)

// RegisterPluginLoader sets the active PluginLoader. A nil value resets to a
// fresh DefaultPluginLoader.
func RegisterPluginLoader(l PluginLoader) {
	pluginLoaderMu.Lock()
	defer pluginLoaderMu.Unlock()
	if l == nil {
		l = NewDefaultPluginLoader()
	}
	slog.Info("extension.register.plugin_loader", "name", l.Name())
	defaultLoader = l
}

// GetPluginLoader returns the active PluginLoader, lazily defaulting to a
// DefaultPluginLoader when none has been registered.
func GetPluginLoader() PluginLoader {
	pluginLoaderMu.Lock()
	defer pluginLoaderMu.Unlock()
	if defaultLoader == nil {
		defaultLoader = NewDefaultPluginLoader()
	}
	return defaultLoader
}
