package extension

import (
	"context"
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// This file defines the ExtensionRegistry contract a Phase 4 extension uses to
// register its building blocks, plus a concurrency-safe default implementation
// with getters.

// ExtensionRegistry is the registry an extension uses to register its building
// blocks (tools, commands, providers, hooks, middleware). Duplicate
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

// DefaultExtensionRegistry is the default in-memory ExtensionRegistry.
type DefaultExtensionRegistry struct {
	mu          sync.Mutex
	tools       map[string]tools.ToolDefinition
	commands    map[string]func(args []string) error
	providers   map[string]llm.ModelProvider
	hooks       map[string]Hook
	middlewares map[string]Middleware
}

var _ ExtensionRegistry = (*DefaultExtensionRegistry)(nil)

// NewExtensionRegistry creates an empty DefaultExtensionRegistry.
func NewExtensionRegistry() ExtensionRegistry {
	return &DefaultExtensionRegistry{
		tools:       make(map[string]tools.ToolDefinition),
		commands:    make(map[string]func(args []string) error),
		providers:   make(map[string]llm.ModelProvider),
		hooks:       make(map[string]Hook),
		middlewares: make(map[string]Middleware),
	}
}

// RegisterTool stores a tool by name, overwriting any previous entry.
func (r *DefaultExtensionRegistry) RegisterTool(_ context.Context, t tools.ToolDefinition) error {
	slog.Info("extension.register.tool", "tool", t.Name())
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name()] = t
	return nil
}

// RegisterCommand stores a command by name, overwriting any previous entry.
func (r *DefaultExtensionRegistry) RegisterCommand(name string, fn func(args []string) error) error {
	slog.Info("extension.register.command", "name", name)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands[name] = fn
	return nil
}

// RegisterProvider stores a provider by name, overwriting any previous entry.
func (r *DefaultExtensionRegistry) RegisterProvider(p llm.ModelProvider) error {
	slog.Info("extension.register.provider", "name", p.Name())
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[p.Name()] = p
	return nil
}

// RegisterHook stores a hook by name, overwriting any previous entry.
func (r *DefaultExtensionRegistry) RegisterHook(_ context.Context, h Hook) error {
	slog.Info("extension.register.hook", "name", h.Name())
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks[h.Name()] = h
	return nil
}

// RegisterMiddleware stores a middleware by name, overwriting any previous
// entry.
func (r *DefaultExtensionRegistry) RegisterMiddleware(_ context.Context, m Middleware) error {
	slog.Info("extension.register.middleware", "name", m.Name())
	r.mu.Lock()
	defer r.mu.Unlock()
	r.middlewares[m.Name()] = m
	return nil
}

// tool returns the tool registered under name, or nil if unknown.
func (r *DefaultExtensionRegistry) tool(name string) tools.ToolDefinition {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tools[name]
}

// command returns the command registered under name, or nil if unknown.
func (r *DefaultExtensionRegistry) command(name string) func(args []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.commands[name]
}

// provider returns the provider registered under name, or nil if unknown.
func (r *DefaultExtensionRegistry) provider(name string) llm.ModelProvider {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.providers[name]
}

// Hook returns the hook registered under name, or nil if unknown.
func (r *DefaultExtensionRegistry) Hook(name string) Hook {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hooks[name]
}

// Middleware returns the middleware registered under name, or nil if unknown.
func (r *DefaultExtensionRegistry) Middleware(name string) Middleware {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.middlewares[name]
}

// AllTools returns a slice of all registered tools.
func (r *DefaultExtensionRegistry) AllTools() []tools.ToolDefinition {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]tools.ToolDefinition, 0, len(r.tools))
	for _, t := range r.tools {
		result = append(result, t)
	}
	return result
}

// AllHooks returns a slice of all registered hooks.
func (r *DefaultExtensionRegistry) AllHooks() []Hook {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]Hook, 0, len(r.hooks))
	for _, h := range r.hooks {
		result = append(result, h)
	}
	return result
}

// AllMiddleware returns a slice of all registered middleware.
func (r *DefaultExtensionRegistry) AllMiddleware() []Middleware {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]Middleware, 0, len(r.middlewares))
	for _, m := range r.middlewares {
		result = append(result, m)
	}
	return result
}
