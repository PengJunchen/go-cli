package extension

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// This file defines the ExtensionRegistry contract a Phase 4 extension uses to
// register its building blocks, plus a concurrency-safe default implementation
// with getters.

var (
	ErrExtensionRegistrySealed          = errors.New("extension: registry staging is sealed")
	ErrExtensionAtomicCommitUnsupported = errors.New("extension: registry does not support atomic staging commit")
)

// ExtensionRegistry is the registry an extension uses to register its building
// blocks (tools, commands, providers, hooks, middleware). Duplicate
// registrations return an error.
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

// DefaultExtensionRegistry is the default in-memory ExtensionRegistry. Its
// enumeration methods preserve successful registration order. A staging
// registry is sealed before it is atomically committed or discarded.
type DefaultExtensionRegistry struct {
	mu              sync.Mutex
	tools           map[string]tools.ToolDefinition
	toolOrder       []string
	commands        map[string]func(args []string) error
	commandOrder    []string
	providers       map[string]llm.ModelProvider
	providerOrder   []string
	hooks           map[string]Hook
	hookOwners      map[string]string
	hookOrder       []string
	hookOwner       string
	middlewares     map[string]Middleware
	middlewareOrder []string
	sealed          bool
}

var _ ExtensionRegistry = (*DefaultExtensionRegistry)(nil)

// NewExtensionRegistry creates an empty DefaultExtensionRegistry.
func NewExtensionRegistry() ExtensionRegistry {
	return &DefaultExtensionRegistry{
		tools:       make(map[string]tools.ToolDefinition),
		commands:    make(map[string]func(args []string) error),
		providers:   make(map[string]llm.ModelProvider),
		hooks:       make(map[string]Hook),
		hookOwners:  make(map[string]string),
		middlewares: make(map[string]Middleware),
	}
}

// RegisterTool stores a tool by name. It returns an error if a tool with the
// same name is already registered.
func (r *DefaultExtensionRegistry) RegisterTool(_ context.Context, t tools.ToolDefinition) error {
	if t == nil {
		return errors.New("extension: tool is nil")
	}
	name := t.Name()
	slog.Info("extension.register.tool", "tool", name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return ErrExtensionRegistrySealed
	}
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("extension already registered: %s", name)
	}
	r.tools[name] = t
	r.toolOrder = append(r.toolOrder, name)
	return nil
}

// RegisterCommand stores a command by name. It returns an error if a command
// with the same name is already registered.
func (r *DefaultExtensionRegistry) RegisterCommand(name string, fn func(args []string) error) error {
	slog.Info("extension.register.command", "name", name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return ErrExtensionRegistrySealed
	}
	if _, exists := r.commands[name]; exists {
		return fmt.Errorf("extension already registered: %s", name)
	}
	r.commands[name] = fn
	r.commandOrder = append(r.commandOrder, name)
	return nil
}

// RegisterProvider stores a provider by name. It returns an error if a provider
// with the same name is already registered.
func (r *DefaultExtensionRegistry) RegisterProvider(p llm.ModelProvider) error {
	if p == nil {
		return errors.New("extension: provider is nil")
	}
	name := p.Name()
	slog.Info("extension.register.provider", "name", name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return ErrExtensionRegistrySealed
	}
	if _, exists := r.providers[name]; exists {
		return fmt.Errorf("extension already registered: %s", name)
	}
	r.providers[name] = p
	r.providerOrder = append(r.providerOrder, name)
	return nil
}

// RegisterHook stores a hook by name. It returns an error if a hook with the
// same name is already registered.
func (r *DefaultExtensionRegistry) RegisterHook(_ context.Context, h Hook) error {
	if h == nil {
		return errors.New("extension: hook is nil")
	}
	name := h.Name()
	slog.Info("extension.register.hook", "name", name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return ErrExtensionRegistrySealed
	}
	if _, exists := r.hooks[name]; exists {
		return fmt.Errorf("extension already registered: %s", name)
	}
	r.hooks[name] = h
	r.hookOwners[name] = r.hookOwner
	r.hookOrder = append(r.hookOrder, name)
	return nil
}

// RegisterMiddleware stores a middleware by name. It returns an error if a
// middleware with the same name is already registered.
func (r *DefaultExtensionRegistry) RegisterMiddleware(_ context.Context, m Middleware) error {
	if m == nil {
		return errors.New("extension: middleware is nil")
	}
	name := m.Name()
	slog.Info("extension.register.middleware", "name", name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return ErrExtensionRegistrySealed
	}
	if _, exists := r.middlewares[name]; exists {
		return fmt.Errorf("extension already registered: %s", name)
	}
	r.middlewares[name] = m
	r.middlewareOrder = append(r.middlewareOrder, name)
	return nil
}

// seal prevents an extension from mutating its staging registry after Init
// returns. It is safe to call more than once.
func (r *DefaultExtensionRegistry) seal() {
	r.mu.Lock()
	r.sealed = true
	r.mu.Unlock()
}

// commitFromLocked seals staging and publishes it atomically. The caller must
// hold staging.mu and r.mu in that order.
func (r *DefaultExtensionRegistry) commitFromLocked(staging *DefaultExtensionRegistry) error {
	if staging == nil || staging == r {
		return ErrExtensionAtomicCommitUnsupported
	}
	staging.sealed = true
	if r.sealed {
		return ErrExtensionRegistrySealed
	}
	var conflicts []string
	for _, name := range staging.toolOrder {
		if _, exists := r.tools[name]; exists {
			conflicts = append(conflicts, "tool "+name)
		}
	}
	for _, name := range staging.commandOrder {
		if _, exists := r.commands[name]; exists {
			conflicts = append(conflicts, "command "+name)
		}
	}
	for _, name := range staging.providerOrder {
		if _, exists := r.providers[name]; exists {
			conflicts = append(conflicts, "provider "+name)
		}
	}
	for _, name := range staging.hookOrder {
		if _, exists := r.hooks[name]; exists {
			conflicts = append(conflicts, "hook "+name)
		}
	}
	for _, name := range staging.middlewareOrder {
		if _, exists := r.middlewares[name]; exists {
			conflicts = append(conflicts, "middleware "+name)
		}
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("extension already registered: %s", strings.Join(conflicts, ", "))
	}
	for _, name := range staging.toolOrder {
		r.tools[name] = staging.tools[name]
	}
	r.toolOrder = append(r.toolOrder, staging.toolOrder...)
	for _, name := range staging.commandOrder {
		r.commands[name] = staging.commands[name]
	}
	r.commandOrder = append(r.commandOrder, staging.commandOrder...)
	for _, name := range staging.providerOrder {
		r.providers[name] = staging.providers[name]
	}
	r.providerOrder = append(r.providerOrder, staging.providerOrder...)
	for _, name := range staging.hookOrder {
		r.hooks[name] = staging.hooks[name]
		r.hookOwners[name] = staging.hookOwners[name]
	}
	r.hookOrder = append(r.hookOrder, staging.hookOrder...)
	for _, name := range staging.middlewareOrder {
		r.middlewares[name] = staging.middlewares[name]
	}
	r.middlewareOrder = append(r.middlewareOrder, staging.middlewareOrder...)
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

// AllTools returns tools in successful registration order.
func (r *DefaultExtensionRegistry) AllTools() []tools.ToolDefinition {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]tools.ToolDefinition, 0, len(r.toolOrder))
	for _, name := range r.toolOrder {
		result = append(result, r.tools[name])
	}
	return result
}

// AllCommands returns commands in successful registration order.
func (r *DefaultExtensionRegistry) AllCommands() []func(args []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]func(args []string) error, 0, len(r.commandOrder))
	for _, name := range r.commandOrder {
		result = append(result, r.commands[name])
	}
	return result
}

// AllProviders returns providers in successful registration order.
func (r *DefaultExtensionRegistry) AllProviders() []llm.ModelProvider {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]llm.ModelProvider, 0, len(r.providerOrder))
	for _, name := range r.providerOrder {
		result = append(result, r.providers[name])
	}
	return result
}

// AllHooks returns hooks in successful registration order.
func (r *DefaultExtensionRegistry) AllHooks() []Hook {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]Hook, 0, len(r.hookOrder))
	for _, name := range r.hookOrder {
		result = append(result, r.hooks[name])
	}
	return result
}

// AllHookRegistrations returns ordered, owner-bearing Hook registrations.
// The returned slice is a snapshot; the Hook values themselves remain the
// registered extension objects.
func (r *DefaultExtensionRegistry) AllHookRegistrations() []HookRegistration {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]HookRegistration, 0, len(r.hookOrder))
	for _, name := range r.hookOrder {
		result = append(result, HookRegistration{Owner: r.hookOwners[name], Hook: r.hooks[name]})
	}
	return result
}

// AllMiddleware returns middleware in successful registration order.
func (r *DefaultExtensionRegistry) AllMiddleware() []Middleware {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]Middleware, 0, len(r.middlewareOrder))
	for _, name := range r.middlewareOrder {
		result = append(result, r.middlewares[name])
	}
	return result
}
