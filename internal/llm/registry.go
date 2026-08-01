package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// errProviderNotFound is returned by Get when no provider is registered under a
// name.
var errProviderNotFound = errors.New("llm: provider not found")

// errProviderAlreadyRegistered is returned by Register when a provider is
// already registered under the same name.
var errProviderAlreadyRegistered = errors.New("llm: provider already registered")

// ProviderRegistry manages ModelProvider instances by name. It is a concrete,
// thread-safe container: providers keyed by name with deterministic
// registration order. Register/Get/List are safe for concurrent use.
//
// NewProviderRegistry pre-registers a default EinoProvider so a freshly
// constructed registry is always usable.
type ProviderRegistry struct {
	mu        sync.RWMutex
	providers map[string]ModelProvider
	order     []string
}

// NewProviderRegistry creates a ProviderRegistry that already contains a
// default EinoProvider registered under its name ("eino").
func NewProviderRegistry() *ProviderRegistry {
	reg := &ProviderRegistry{
		providers: map[string]ModelProvider{},
	}
	// Pre-register a default provider so the registry is always usable.
	def := NewEinoProvider()
	if err := reg.Register(def); err != nil {
		// Cannot happen: name "eino" is unoccupied in a fresh registry.
		slog.Error("llm_registry_register_default_failed", "err", err)
	}
	return reg
}

// Register adds a ModelProvider under its Name(). It panics if p is nil. It
// returns an error if a provider with the same name is already registered,
// leaving the existing registration untouched. Emits slog on success.
func (r *ProviderRegistry) Register(p ModelProvider) error {
	if p == nil {
		panic("llm: cannot register a nil ModelProvider")
	}
	name := p.Name()
	if name == "" {
		return fmt.Errorf("llm: provider has no name")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.providers[name]; exists {
		return fmt.Errorf("%w: %s", errProviderAlreadyRegistered, name)
	}
	r.providers[name] = p
	r.order = append(r.order, name)

	slog.Info("llm_registry_register", "provider_name", name)
	return nil
}

// Get returns the provider registered under name. It returns an error when no
// provider is registered under that name.
func (r *ProviderRegistry) Get(name string) (ModelProvider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", errProviderNotFound, name)
	}
	return p, nil
}

// GetModel is a convenience that resolves a provider by name and builds a chat
// model from cfg. The returned cleanup is nil-safe to call.
func (r *ProviderRegistry) GetModel(ctx context.Context, name string, cfg ModelConfig) (BaseChatModel, func(), error) {
	p, err := r.Get(name)
	if err != nil {
		return nil, nil, err
	}
	return p.Build(ctx, cfg)
}

// List returns the registered providers in registration order. The returned
// slice is a copy; mutating it does not affect the registry.
func (r *ProviderRegistry) List() []ModelProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ModelProvider, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.providers[name])
	}
	return out
}

// Default returns the first registered provider, or nil when the registry is
// empty.
func (r *ProviderRegistry) Default() ModelProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.order) == 0 {
		return nil
	}
	return r.providers[r.order[0]]
}
