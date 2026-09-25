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

var errProviderNameNotCanonical = errors.New("llm: provider name must use its canonical lowercase spelling")

// ProviderRegistry manages ModelProvider instances by name. It is a concrete,
// thread-safe container: providers keyed by name with deterministic
// registration order. Register/Get/List are safe for concurrent use.
//
// NewProviderRegistry pre-registers a default EinoProvider so a freshly
// constructed registry is always usable.
type ProviderRegistry struct {
	mu            sync.RWMutex
	providers     map[string]ModelProvider
	order         []string
	modelRegistry ModelRegistry
	providerSpecs map[string]ProviderSpec
	credentials   ScopedCredentialResolver
	resolver      *ModelResolver
}

// NewProviderRegistry creates a ProviderRegistry that already contains a
// default EinoProvider registered under its name ("eino").
func NewProviderRegistry() *ProviderRegistry {
	reg := &ProviderRegistry{
		providers:     map[string]ModelProvider{},
		providerSpecs: map[string]ProviderSpec{},
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
	name := canonicalProviderID(p.Name())
	if name == "" {
		return fmt.Errorf("llm: provider has no name")
	}

	spec, _ := inferProviderSpec(p)
	return r.register(p, spec)
}

func (r *ProviderRegistry) register(p ModelProvider, spec ProviderSpec) error {
	rawName := p.Name()
	name := canonicalProviderID(rawName)
	if name == "" {
		return fmt.Errorf("llm: provider has no name")
	}
	if spec.ProviderID != "" {
		spec.ProviderID = canonicalProviderID(spec.ProviderID)
		if spec.ProviderID != name {
			return fmt.Errorf("llm: provider spec identity %q does not match registered name %q", spec.ProviderID, name)
		}
	} else if len(spec.Aliases) != 0 {
		return fmt.Errorf("llm: provider aliases require a canonical provider identity")
	}
	spec.ProtocolID = ProtocolID(canonicalProviderID(string(spec.ProtocolID)))
	aliases := make([]string, 0, len(spec.Aliases))
	seenAliases := make(map[string]struct{}, len(spec.Aliases))
	for _, alias := range spec.Aliases {
		alias = canonicalProviderID(alias)
		if alias == "" || alias == name || isDisabledNativeIdentity(alias) {
			return fmt.Errorf("llm: invalid provider alias %q for %q", alias, name)
		}
		if _, duplicate := seenAliases[alias]; duplicate {
			return fmt.Errorf("llm: duplicate provider alias %q for %q", alias, name)
		}
		seenAliases[alias] = struct{}{}
		aliases = append(aliases, alias)
	}
	spec.Aliases = aliases
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.providers == nil {
		r.providers = make(map[string]ModelProvider)
	}
	if r.providerSpecs == nil {
		r.providerSpecs = make(map[string]ProviderSpec)
	}
	if _, exists := r.providers[name]; exists {
		return fmt.Errorf("%w: %s", errProviderAlreadyRegistered, name)
	}
	if rawName != name {
		return fmt.Errorf("%w: %s", errProviderNameNotCanonical, name)
	}
	for _, prior := range r.providerSpecs {
		for _, priorAlias := range prior.Aliases {
			if canonicalProviderID(priorAlias) == name {
				return fmt.Errorf("llm: canonical provider name %q collides with an existing alias", name)
			}
		}
	}
	for _, alias := range spec.Aliases {
		if _, exists := r.providers[alias]; exists {
			return fmt.Errorf("llm: provider alias %q collides with canonical provider name", alias)
		}
		for priorName, prior := range r.providerSpecs {
			if priorName == alias {
				return fmt.Errorf("llm: provider alias %q collides with canonical provider name", alias)
			}
			for _, priorAlias := range prior.Aliases {
				priorAlias = canonicalProviderID(priorAlias)
				if priorAlias == alias || priorAlias == name {
					return fmt.Errorf("llm: provider alias %q collides with an existing provider identity", alias)
				}
			}
		}
	}
	r.providers[name] = p
	r.order = append(r.order, name)
	if spec.ProviderID != "" {
		r.providerSpecs[canonicalProviderID(name)] = cloneProviderSpec(spec)
	}
	r.publishResolverLocked()
	slog.Info("llm_registry_register", "provider_name", name)
	return nil
}

func (r *ProviderRegistry) publishResolverLocked() {
	resolver := &ModelResolver{
		providers:   make(map[string]providerDefinition, len(r.providers)),
		aliases:     make(map[string]string),
		credentials: r.credentials,
	}
	for _, name := range r.order {
		provider := r.providers[name]
		id := canonicalProviderID(name)
		spec, hasSpec := r.providerSpecs[id]
		if !hasSpec {
			if inferred, ok := inferProviderSpec(provider); ok {
				spec = inferred
			}
		}
		models := append([]ModelInfo(nil), provider.Models()...)
		resolver.providers[id] = providerDefinition{spec: cloneProviderSpec(spec), provider: provider, models: models}
		for _, alias := range spec.Aliases {
			resolver.aliases[canonicalProviderID(alias)] = id
		}
	}
	r.resolver = resolver
}

// Get returns the provider registered under name. It returns an error when no
// provider is registered under that name.
func (r *ProviderRegistry) Get(name string) (ModelProvider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	canonical := r.canonicalProviderNameLocked(name)
	p, ok := r.providers[canonical]
	if !ok {
		return nil, fmt.Errorf("%w: %s", errProviderNotFound, name)
	}
	return p, nil
}

func (r *ProviderRegistry) canonicalProviderNameLocked(name string) string {
	canonical := canonicalProviderID(name)
	for providerID, spec := range r.providerSpecs {
		for _, alias := range spec.Aliases {
			if canonicalProviderID(alias) == canonical {
				return providerID
			}
		}
	}
	return canonical
}

// GetModel is a convenience that resolves a provider by name and builds a chat
// model from cfg. The returned cleanup is nil-safe to call.
func (r *ProviderRegistry) GetModel(ctx context.Context, name string, cfg ModelConfig) (BaseChatModel, func(), error) {
	resolver := r.SnapshotResolver()
	if resolver == nil {
		if _, err := r.Get(name); err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("%w: %s", ErrMissingProviderSpec, name)
	}
	resolved, err := resolver.resolveModel(ctx, ModelRequest{
		ProviderID:     name,
		ModelID:        cfg.Model,
		Endpoint:       cfg.BaseURL,
		Credential:     cfg.Credential,
		Temperature:    cfg.Temperature,
		TemperatureSet: cfg.TemperatureSet,
		MaxTokens:      cfg.MaxTokens,
		MaxTokensSet:   cfg.MaxTokensSet,
	}, cfg.APIKey)
	if err != nil {
		return nil, nil, err
	}
	return resolved.Build(ctx)
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

// GetModelInfo returns the ModelInfo for the named model exposed by the named
// provider. It returns an empty ModelInfo when the provider or model is not
// found.
func (r *ProviderRegistry) GetModelInfo(ctx context.Context, provider, model string) ModelInfo {
	r.mu.RLock()
	provider = r.canonicalProviderNameLocked(provider)
	p, exists := r.providers[provider]
	modelRegistry := r.modelRegistry
	r.mu.RUnlock()
	if exists {
		for _, m := range p.Models() {
			if m.Name == model {
				return m
			}
		}
	}
	// A local miss may be enriched by the external registry, but the lookup
	// always uses the canonical provider identity selected above.
	if modelRegistry != nil {
		if info, ok := modelRegistry.Lookup(ctx, provider, model); ok {
			return info
		}
	}
	return ModelInfo{}
}
