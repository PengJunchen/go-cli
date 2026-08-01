package core

import (
	"context"
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// Extension is the smallest contract a user-supplied extension satisfies.
type Extension interface {
	// Name returns the extension identifier.
	Name() string
}

// Hook is an event hook registered by an extension.
type Hook interface {
	// Name returns the hook identifier.
	Name() string
}

// Middleware wraps an AgentLoop to intercept and augment its behavior
// (onion model).
type Middleware interface {
	// Name returns the middleware identifier.
	Name() string
	// Wrap returns a wrapped view of the given AgentLoop.
	Wrap(n AgentLoop) AgentLoop
}

// ExtensionRegistry is the registry an extension uses to register its
// building blocks (tools, commands, providers, hooks, middleware). Duplicate
// registrations overwrite the previous entry (last writer wins).
type ExtensionRegistry interface {
	// RegisterTool registers a tool definition.
	RegisterTool(ctx context.Context, t tools.ToolDefinition) error
	// RegisterCommand registers a command invoked by name.
	RegisterCommand(name string, fn func(args []string) error) error
	// RegisterProvider registers an LLM model provider.
	RegisterProvider(p llm.ModelProvider) error
	// RegisterHook registers a Hook.
	RegisterHook(ctx context.Context, h Hook) error
	// RegisterMiddleware registers a Middleware.
	RegisterMiddleware(ctx context.Context, m Middleware) error
}

// ExtensionRegistryImpl is the default in-memory ExtensionRegistry.
type ExtensionRegistryImpl struct {
	mu          sync.Mutex
	tools       map[string]tools.ToolDefinition
	commands    map[string]func(args []string) error
	providers   map[string]llm.ModelProvider
	hooks       map[string]Hook
	middlewares map[string]Middleware
}

var _ ExtensionRegistry = (*ExtensionRegistryImpl)(nil)

// NewExtensionRegistry creates an empty ExtensionRegistryImpl.
func NewExtensionRegistry() *ExtensionRegistryImpl {
	return &ExtensionRegistryImpl{
		tools:       make(map[string]tools.ToolDefinition),
		commands:    make(map[string]func(args []string) error),
		providers:   make(map[string]llm.ModelProvider),
		hooks:       make(map[string]Hook),
		middlewares: make(map[string]Middleware),
	}
}

// RegisterTool stores a tool by name, overwriting any previous entry.
func (r *ExtensionRegistryImpl) RegisterTool(_ context.Context, t tools.ToolDefinition) error {
	slog.Info("core.extension.register.tool", "tool", t.Name())
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name()] = t
	return nil
}

// RegisterCommand stores a command by name, overwriting any previous entry.
func (r *ExtensionRegistryImpl) RegisterCommand(name string, fn func(args []string) error) error {
	slog.Info("core.extension.register.command", "name", name)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands[name] = fn
	return nil
}

// RegisterProvider stores a provider by name, overwriting any previous entry.
func (r *ExtensionRegistryImpl) RegisterProvider(p llm.ModelProvider) error {
	slog.Info("core.extension.register.provider", "name", p.Name())
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[p.Name()] = p
	return nil
}

// RegisterHook stores a hook by name, overwriting any previous entry.
func (r *ExtensionRegistryImpl) RegisterHook(_ context.Context, h Hook) error {
	slog.Info("core.extension.register.hook", "name", h.Name())
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks[h.Name()] = h
	return nil
}

// RegisterMiddleware stores a middleware by name, overwriting any previous
// entry.
func (r *ExtensionRegistryImpl) RegisterMiddleware(_ context.Context, m Middleware) error {
	slog.Info("core.extension.register.middleware", "name", m.Name())
	r.mu.Lock()
	defer r.mu.Unlock()
	r.middlewares[m.Name()] = m
	return nil
}

// Tool returns the tool registered under name, or nil if unknown.
func (r *ExtensionRegistryImpl) Tool(name string) tools.ToolDefinition {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tools[name]
}

// Command returns the command registered under name, or nil if unknown.
func (r *ExtensionRegistryImpl) Command(name string) func(args []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.commands[name]
}

// Provider returns the provider registered under name, or nil if unknown.
func (r *ExtensionRegistryImpl) Provider(name string) llm.ModelProvider {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.providers[name]
}

// Hook returns the hook registered under name, or nil if unknown.
func (r *ExtensionRegistryImpl) Hook(name string) Hook {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hooks[name]
}

// Middleware returns the middleware registered under name, or nil if unknown.
func (r *ExtensionRegistryImpl) Middleware(name string) Middleware {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.middlewares[name]
}

// ExtensionImpl is the default Extension stub.
type ExtensionImpl struct{}

var _ Extension = (*ExtensionImpl)(nil)

// Name returns the stub name.
func (ExtensionImpl) Name() string { return "default-extension" }

// HookImpl is the default Hook stub.
type HookImpl struct{ name string }

var _ Hook = (*HookImpl)(nil)

// Name returns the hook name.
func (h *HookImpl) Name() string {
	if h.name == "" {
		return "default-hook"
	}
	return h.name
}

// MiddlewareImpl is the default Middleware stub. It is a pass-through that
// returns the underlying AgentLoop unchanged.
type MiddlewareImpl struct{ name string }

var _ Middleware = (*MiddlewareImpl)(nil)

// Name returns the middleware name.
func (m *MiddlewareImpl) Name() string {
	if m.name == "" {
		return "default-middleware"
	}
	return m.name
}

// Wrap returns n unchanged (pass-through).
func (m *MiddlewareImpl) Wrap(n AgentLoop) AgentLoop {
	slog.Info("core.middleware.wrap", "name", m.Name())
	return n
}
