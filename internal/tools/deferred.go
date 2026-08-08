package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// DeferredToolRegistry wraps an underlying ToolRegistry and adds lazy, on-demand
// tool loading. Tools registered as "deferred" are not present in the backing
// registry until they are explicitly loaded (Load). Until then callers that
// enumerate tools (e.g. via List) see a placeholder stub so the set of available
// tool names is stable; callers that need the real implementation call Load.
type DeferredToolRegistry interface {
	// RegisterDeferred registers a loader for the named tool. The loader is
	// invoked at most once, on the first successful Load. Registering the same
	// name twice replaces the previous loader.
	RegisterDeferred(ctx context.Context, name string, loader func() (ToolDefinition, error)) error
	// Load materializes the named deferred tool into the backing registry,
	// invoking its loader exactly once (double-checked). It returns the loaded
	// ToolDefinition, or a stub when loading failed so execution neither blocks
	// nor panics. It returns an error when the loader itself returned an error
	// or the name is unknown.
	Load(ctx context.Context, name string) (ToolDefinition, error)
	// IsLoaded reports whether the named deferred tool has been materialized.
	IsLoaded(name string) bool
}

// ErrDeferredToolNotFound is returned by Load when no loader has been
// registered under the requested name.
var ErrDeferredToolNotFound = errors.New("tools: deferred tool not found")

// deferredStubDescription is the placeholder description returned by an
// unloaded (or failed-to-load) deferred tool. It is intentionally generic; the
// loader is expected to replace it with a real description on success.
const deferredStubDescription = "deferred tool (not yet loaded)"

// DefaultDeferredToolRegistry is the default DeferredToolRegistry
// implementation. It is concurrency-safe.
type DefaultDeferredToolRegistry struct {
	underlying ToolRegistry // dependency held by interface.

	mu      sync.RWMutex
	loaders map[string]func() (ToolDefinition, error)
	loaded  map[string]ToolDefinition
}

var _ DeferredToolRegistry = (*DefaultDeferredToolRegistry)(nil)

// NewDefaultDeferredToolRegistry wraps the given underlying ToolRegistry. The
// underlying registry may be nil; Load then returns a stub without registering,
// and RegisterDeferred still records the loader.
func NewDefaultDeferredToolRegistry(underlying ToolRegistry) DeferredToolRegistry {
	return &DefaultDeferredToolRegistry{
		underlying: underlying,
		loaders:    map[string]func() (ToolDefinition, error){},
		loaded:     map[string]ToolDefinition{},
	}
}

// RegisterDeferred records a loader for the named tool. Registering the same
// name again replaces the previous loader and forgets any previously loaded
// definition.
func (r *DefaultDeferredToolRegistry) RegisterDeferred(_ context.Context, name string, loader func() (ToolDefinition, error)) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("tools: cannot register a deferred tool with an empty name")
	}
	if loader == nil {
		return errors.New("tools: cannot register a nil deferred tool loader")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.loaders[name] = loader
	delete(r.loaded, name)
	return nil
}

// Load materializes the deferred tool named name. It invokes the loader at most
// once (double-checked locking): concurrent Load calls for the same name share a
// single loader invocation. On success the loaded definition is registered into
// the underlying registry (when present) and stored. When the loader returns an
// error, a stub placeholder is stored so execution neither blocks nor panics and
// the loader is retained for a later retry.
func (r *DefaultDeferredToolRegistry) Load(ctx context.Context, name string) (ToolDefinition, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())
	defer span.End()

	// Fast path: already loaded.
	r.mu.RLock()
	if def, ok := r.loaded[name]; ok {
		r.mu.RUnlock()
		span.SetAttributes(tracing.Attribute{Key: "tool_name", Value: name}, tracing.Attribute{Key: "status", Value: "already_loaded"})
		return def, nil
	}
	r.mu.RUnlock()

	// Take the loader out from under the write lock so the double-checked
	// pattern holds: concurrent callers block on r.mu.Lock, and after acquiring
	// it re-check the loaded map before invoking the loader.
	r.mu.Lock()
	defer r.mu.Unlock()

	if def, ok := r.loaded[name]; ok {
		span.SetAttributes(tracing.Attribute{Key: "tool_name", Value: name}, tracing.Attribute{Key: "status", Value: "already_loaded"})
		return def, nil
	}

	loader, ok := r.loaders[name]
	if !ok {
		span.SetAttributes(tracing.Attribute{Key: "tool_name", Value: name}, tracing.Attribute{Key: "status", Value: "not_found"}, tracing.Attribute{Key: "success", Value: false})
		logger.Error("tools.deferred.load.not_found", "tool", name)
		return nil, ErrDeferredToolNotFound
	}

	def, err := loader()
	if err != nil {
		// Fall back to a stub: execution neither blocks nor panics. The stub
		// is cached in the loaded map so subsequent Loads return it
		// immediately without re-invoking the loader.
		stub := &deferredStub{name: name}
		r.loaded[name] = stub
		span.SetAttributes(tracing.Attribute{Key: "tool_name", Value: name}, tracing.Attribute{Key: "status", Value: "stub"})
		logger.Error("tools.deferred.load.failed", "tool", name, "err", err)
		return stub, fmt.Errorf("tools: deferred load %q: %w", name, err)
	}
	if def == nil {
		stub := &deferredStub{name: name}
		r.loaded[name] = stub
		span.SetAttributes(tracing.Attribute{Key: "tool_name", Value: name}, tracing.Attribute{Key: "status", Value: "stub"})
		logger.Error("tools.deferred.load.nil", "tool", name)
		return stub, errors.New("tools: deferred loader for " + name + " returned a nil definition")
	}

	r.loaded[name] = def
	if r.underlying != nil {
		if regErr := r.underlying.Register(ctx, def); regErr != nil {
			logger.Error("tools.deferred.load.register_failed", "tool", name, "err", regErr)
			return def, fmt.Errorf("tools: deferred load %q: register: %w", name, regErr)
		}
	}

	span.SetAttributes(tracing.Attribute{Key: "tool_name", Value: name}, tracing.Attribute{Key: "status", Value: "loaded"}, tracing.Attribute{Key: "success", Value: true})
	logger.Info("tools.deferred.load.done", "tool", name)
	return def, nil
}

// IsLoaded reports whether the named deferred tool has been materialized
// (successfully loaded or failed into a stub).
func (r *DefaultDeferredToolRegistry) IsLoaded(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.loaded[name]
	return ok
}

// deferredStub is a placeholder ToolDefinition shown until a deferred tool is
// successfully loaded. Executing it returns an explanatory error so callers
// know the tool is not yet available.
type deferredStub struct{ name string }

var _ ToolDefinition = (*deferredStub)(nil)

func (s *deferredStub) Name() string { return s.name }

func (s *deferredStub) Description() string {
	return s.name + ": " + deferredStubDescription
}

func (s *deferredStub) Execute(_ context.Context, _ ToolCall) (*ToolResult, error) {
	return nil, fmt.Errorf("tools: deferred tool %q is not loaded", s.name)
}

// defaultDeferredRegistry holds the process-wide active DeferredToolRegistry.
var (
	deferredMu      sync.RWMutex
	defaultDeferred DeferredToolRegistry
)

// RegisterDeferredToolRegistry sets the active DeferredToolRegistry. A nil
// value resets to a fresh DefaultDeferredToolRegistry wrapping a new
// DefaultToolRegistry (which may be nil while deferred tools are still loading).
func RegisterDeferredToolRegistry(r DeferredToolRegistry) {
	deferredMu.Lock()
	defer deferredMu.Unlock()
	if r == nil {
		r = NewDefaultDeferredToolRegistry(NewDefaultToolRegistry())
	}
	slog.Info("tools.register.deferred_registry", "name", "deferred-tool-registry")
	defaultDeferred = r
}

// GetDeferredToolRegistry returns the active DeferredToolRegistry, lazily
// defaulting to a DefaultDeferredToolRegistry when none has been registered.
func GetDeferredToolRegistry() DeferredToolRegistry {
	deferredMu.RLock()
	defer deferredMu.RUnlock()
	if defaultDeferred == nil {
		return NewDefaultDeferredToolRegistry(NewDefaultToolRegistry())
	}
	return defaultDeferred
}

// DeferredRegistry combines ToolRegistry and DeferredToolRegistry so callers
// can use both eager Register and lazy RegisterDeferred through a single value.
type DeferredRegistry interface {
	ToolRegistry
	DeferredToolRegistry
}

// DeferredToolRegistryAdapter implements ToolRegistry by delegating to a
// DefaultDeferredToolRegistry. Register calls pass through to the underlying
// registry. Get first checks the underlying registry; if not found, it
// attempts to Load the deferred tool. List returns eager tools plus stubs
// for unloaded deferred tools.
type DeferredToolRegistryAdapter struct {
	*DefaultDeferredToolRegistry
}

var _ ToolRegistry = (*DeferredToolRegistryAdapter)(nil)

// NewDeferredToolRegistryAdapter wraps the given underlying ToolRegistry in a
// DefaultDeferredToolRegistry and returns an adapter that satisfies both
// ToolRegistry and DeferredRegistry.
func NewDeferredToolRegistryAdapter(underlying ToolRegistry) *DeferredToolRegistryAdapter {
	dtr := NewDefaultDeferredToolRegistry(underlying)
	return &DeferredToolRegistryAdapter{DefaultDeferredToolRegistry: dtr.(*DefaultDeferredToolRegistry)}
}

// Register delegates to the underlying registry (eager registration).
func (a *DeferredToolRegistryAdapter) Register(ctx context.Context, def ToolDefinition) error {
	return a.underlying.Register(ctx, def)
}

// Get returns the tool with the given name. It first checks the underlying
// registry; if not found, it attempts to Load the deferred tool (which
// registers it into the underlying registry on success). If the name is
// unknown or loading fails, ErrToolNotFound is returned.
func (a *DeferredToolRegistryAdapter) Get(ctx context.Context, name string) (ToolDefinition, error) {
	def, err := a.underlying.Get(ctx, name)
	if err == nil {
		return def, nil
	}
	if !errors.Is(err, ErrToolNotFound) {
		return nil, err
	}

	// Try to materialize a deferred tool.
	if _, loadErr := a.Load(ctx, name); loadErr != nil {
		if errors.Is(loadErr, ErrDeferredToolNotFound) {
			return nil, ErrToolNotFound
		}
		return nil, fmt.Errorf("tools: load deferred %q: %w", name, loadErr)
	}

	// Load succeeded and registered into the underlying registry.
	return a.underlying.Get(ctx, name)
}

// List returns all eagerly registered tools from the underlying registry plus
// placeholder stubs for unloaded deferred tools. Already-loaded deferred tools
// appear once (via the underlying registry); unloaded ones appear as stubs.
func (a *DeferredToolRegistryAdapter) List(ctx context.Context) ([]ToolDefinition, error) {
	eager, err := a.underlying.List(ctx)
	if err != nil {
		return nil, err
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	seen := make(map[string]bool, len(eager))
	for _, d := range eager {
		seen[d.Name()] = true
	}
	for name := range a.loaders {
		if _, loaded := a.loaded[name]; loaded {
			// Loaded tools are already in the underlying registry (or a
			// failed-load stub that we intentionally hide from List).
			continue
		}
		if !seen[name] {
			eager = append(eager, &deferredStub{name: name})
			seen[name] = true
		}
	}
	return eager, nil
}
