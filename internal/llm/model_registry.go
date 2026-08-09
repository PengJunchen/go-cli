package llm

import "context"

// ProviderMetadata holds display and connection info for a model provider
// sourced from an external model registry (e.g. models.dev).
type ProviderMetadata struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	APIBase string   `json:"api_base,omitempty"`
	Doc     string   `json:"doc,omitempty"`
	Env     []string `json:"env,omitempty"`
}

// ModelRegistry is a read-only registry of provider and model metadata
// sourced from an external service. Implementations are expected to cache
// results and degrade gracefully when the upstream is unavailable.
type ModelRegistry interface {
	// Lookup returns enriched ModelInfo for a model from a specific provider.
	// Returns ok=false when the model is not in the registry.
	Lookup(ctx context.Context, provider, model string) (ModelInfo, bool)
	// Providers returns all known provider metadata.
	Providers() []ProviderMetadata
	// ModelsForProvider returns the enriched ModelInfo entries for every model
	// exposed by the given provider. It returns nil when the provider is
	// unknown.
	ModelsForProvider(providerID string) []ModelInfo
	// Refresh fetches the latest data from the source.
	Refresh(ctx context.Context) error
}

// NoopModelRegistry is a ModelRegistry that always reports an empty registry.
// It is the default when no registry is configured.
type NoopModelRegistry struct{}

// Lookup always returns false.
func (NoopModelRegistry) Lookup(_ context.Context, _, _ string) (ModelInfo, bool) { return ModelInfo{}, false }

// Providers always returns nil.
func (NoopModelRegistry) Providers() []ProviderMetadata { return nil }

// ModelsForProvider always returns nil.
func (NoopModelRegistry) ModelsForProvider(_ string) []ModelInfo { return nil }

// Refresh always returns nil.
func (NoopModelRegistry) Refresh(_ context.Context) error { return nil }

var _ ModelRegistry = NoopModelRegistry{}
