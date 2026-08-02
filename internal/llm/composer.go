package llm

// This file implements the "Provider three-layer composition" feature.
//
// == Design (produced inline, per the designer/builder split) ==
//
// ProviderComposer is the contract for building a *ProviderRegistry from three
// independent provider sources:
//
//	Layer 1 (builtin)    : EinoProvider, OpenAIProvider, ClaudeProvider,
//	                       GeminiProvider — always contributed.
//	Layer 2 (config)     : providers supplied by configuration, wired via a
//	                       config provider source callback.
//	Layer 3 (extension)  : providers contributed by extensions (e.g. read from
//	                       an extension.ExtensionRegistry), wired via an
//	                       extension provider source callback.
//
// The three layers are *not* merged blindly. Priority ordering is
//
//	Extension > Config > Builtin
//
// so, for two providers registered under the same name, the one coming from the
// higher-priority source wins: an extension overrides a config provider, and a
// config provider overrides a builtin one. Within a single source the per-entry
// Priority field acts as a tie-breaker (a higher Priority, or on a tie the
// later-registered entry, wins). Compose therefore returns a *ProviderRegistry
// whose Get("name") yields exactly the winning provider for every name.
//
// To keep really coupling minimal and to avoid any import cycle, configuration
// and extension providers enter through provider-source callbacks
//
//	func(context.Context) ([]ModelProvider, error)
//
// rather than by importing internal/config / internal/extension from this
// package. internal/extension already imports internal/llm, so importing it
// here would create a cycle; internal/config exposes only a single settings
// struct (ProviderConfig), not a list of ModelProvider. Callers that hold real
// config or an ExtensionRegistry wrap their lookup in a callback. The callbacks
// also receive the propagated (span) context, which makes context cancellation
// genuinely flow through Compose.
//
// == Tracing ==
//
// Compose emits an "llm.provider_compose" INTERNAL span carrying the attributes
//
//	builtin_count    (number of builtin providers)
//	config_count     (number of config-sourced providers)
//	extension_count  (number of extension-sourced providers)
//	total_count      (number of providers registered in the returned registry)
//
// The child-span context returned by tracing.SpanFromContext is passed into the
// config/extension sources so any span they emit traces back to this one via
// parent_span_id. Results are also recorded with slog.

import (
	"context"
	"log/slog"
	"sort"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// spanNameProviderCompose is the trace span name emitted around Compose.
const spanNameProviderCompose = "llm.provider_compose"

// ProviderSource identifies the origin layer of a ProviderEntry. Its numeric
// value doubles as its priority rank: a higher value always wins, so
// ProviderSourceExtension overrides ProviderSourceConfig overrides
// ProviderSourceBuiltin.
type ProviderSource int

const (
	// ProviderSourceBuiltin marks a provider contributed by the builtin layer.
	ProviderSourceBuiltin ProviderSource = iota
	// ProviderSourceConfig marks a provider loaded from configuration.
	ProviderSourceConfig
	// ProviderSourceExtension marks a provider contributed by an extension.
	ProviderSourceExtension
)

// String returns a stable, lower-case label for the source.
func (s ProviderSource) String() string {
	switch s {
	case ProviderSourceBuiltin:
		return "builtin"
	case ProviderSourceConfig:
		return "config"
	case ProviderSourceExtension:
		return "extension"
	default:
		return "unknown"
	}
}

// ProviderEntry describes a provider candidate collected during composition,
// together with its origin source and an intra-source priority tie-breaker.
type ProviderEntry struct {
	// Name is the provider identifier (equal to Provider.Name()).
	Name string
	// Source is the layer that contributed this entry.
	Source ProviderSource
	// Provider is the ModelProvider candidate.
	Provider ModelProvider
	// Priority breaks ties between entries from the same source.
	Priority int
}

// ProviderComposer builds a *ProviderRegistry from the builtin, config and
// extension provider layers, applying the Extension > Config > Builtin priority
// ordering.
type ProviderComposer interface {
	// Compose constructs a fully merged *ProviderRegistry. The returned
	// registry's Get(name) returns the winning provider for every registered
	// name. It returns an error if any provider-source callback fails.
	Compose(ctx context.Context) (*ProviderRegistry, error)
	// Name returns a stable identifier for this composer.
	Name() string
}

// ProviderSourceFunc supplies one layer's providers from outside the llm
// package. It receives the propagated (span) context so callers can honor
// cancellation while loading config or extension providers, and so any spans
// they emit stay linked in the trace.
type ProviderSourceFunc func(context.Context) ([]ModelProvider, error)

// ProviderComposerOption configures a DefaultProviderComposer.
type ProviderComposerOption func(*DefaultProviderComposer)

// WithConfigProviderSource sets the callback that yields the config layer.
func WithConfigProviderSource(fn ProviderSourceFunc) ProviderComposerOption {
	return func(d *DefaultProviderComposer) { d.configSource = fn }
}

// WithExtensionProviderSource sets the callback that yields the extension layer.
func WithExtensionProviderSource(fn ProviderSourceFunc) ProviderComposerOption {
	return func(d *DefaultProviderComposer) { d.extensionSource = fn }
}

// WithConfigProviders wraps a static slice of providers into the config layer.
// It is a convenience over WithConfigProviderSource for callers that already
// hold the configured providers in memory.
func WithConfigProviders(providers []ModelProvider) ProviderComposerOption {
	return WithConfigProviderSource(func(context.Context) ([]ModelProvider, error) {
		return providers, nil
	})
}

// WithExtensionProviders wraps a static slice of providers into the extension
// layer, mirroring WithConfigProviders.
func WithExtensionProviders(providers []ModelProvider) ProviderComposerOption {
	return WithExtensionProviderSource(func(context.Context) ([]ModelProvider, error) {
		return providers, nil
	})
}

// DefaultProviderComposer is the default ProviderComposer. It owns the
// three-layer collection, the Extension > Config > Builtin priority resolution
// and the "llm.provider_compose" trace span.
type DefaultProviderComposer struct {
	name            string
	configSource    ProviderSourceFunc
	extensionSource ProviderSourceFunc
}

// Compile-time assertion that DefaultProviderComposer satisfies ProviderComposer.
var _ ProviderComposer = (*DefaultProviderComposer)(nil)

// NewDefaultProviderComposer builds a DefaultProviderComposer that contributes
// the builtin providers and, when configured, the config and extension layers.
// Any source left nil contributes nothing.
func NewDefaultProviderComposer(opts ...ProviderComposerOption) ProviderComposer {
	d := &DefaultProviderComposer{
		name:            "default-provider-composer",
		configSource:    staticSources(nil),
		extensionSource: staticSources(nil),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(d)
		}
	}
	return d
}

// staticSources returns a ProviderSourceFunc that always yields the given
// providers with no error, used for the nil-source default.
func staticSources(providers []ModelProvider) ProviderSourceFunc {
	return func(context.Context) ([]ModelProvider, error) { return providers, nil }
}

// Name returns the identifier of this composer.
func (d *DefaultProviderComposer) Name() string { return d.name }

// builtinEntries returns the fixed builtin layer as entries.
func (d *DefaultProviderComposer) builtinEntries() []ProviderEntry {
	return []ProviderEntry{
		{Name: "eino", Source: ProviderSourceBuiltin, Provider: NewEinoProvider()},
		{Name: "openai", Source: ProviderSourceBuiltin, Provider: NewOpenAIProvider()},
		{Name: "claude", Source: ProviderSourceBuiltin, Provider: NewClaudeProvider()},
		{Name: "gemini", Source: ProviderSourceBuiltin, Provider: NewGeminiProvider()},
	}
}

// Compose collects the three layers, resolves same-name conflicts by the
// Extension > Config > Builtin priority, and returns a *ProviderRegistry whose
// Get(name) is the winning provider for every name. It emits the
// "llm.provider_compose" INTERNAL span with the four count attributes and
// propagates the span context into the config/extension sources.
func (d *DefaultProviderComposer) Compose(ctx context.Context) (*ProviderRegistry, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, spanNameProviderCompose, tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())

	builtins := d.builtinEntries()
	entries := make([]ProviderEntry, 0, len(builtins))
	entries = append(entries, builtins...)

	configProviders, err := d.configSource(spanCtx)
	if err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		logger.Error("llm_provider_compose_config_failed", "op", spanNameProviderCompose, "err", err)
		return nil, err
	}
	for _, p := range configProviders {
		entries = append(entries, ProviderEntry{Name: p.Name(), Source: ProviderSourceConfig, Provider: p})
	}

	extensionProviders, err := d.extensionSource(spanCtx)
	if err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		logger.Error("llm_provider_compose_extension_failed", "op", spanNameProviderCompose, "err", err)
		return nil, err
	}
	for _, p := range extensionProviders {
		entries = append(entries, ProviderEntry{Name: p.Name(), Source: ProviderSourceExtension, Provider: p})
	}

	winners := selectWinningProviders(entries)

	reg := &ProviderRegistry{providers: map[string]ModelProvider{}}
	for _, winner := range winners {
		// Winners are unique by name, so Register cannot fail; a failed
		// registration is logged defensively.
		if rerr := reg.Register(winner.Provider); rerr != nil {
			logger.Warn("llm_provider_compose_register_failed", "name", winner.Name, "err", rerr)
			//nolint:errcheck // best-effort; the registry remains usable
		}
	}

	span.SetAttributes(
		tracing.Attribute{Key: "builtin_count", Value: len(builtins)},
		tracing.Attribute{Key: "config_count", Value: len(configProviders)},
		tracing.Attribute{Key: "extension_count", Value: len(extensionProviders)},
		tracing.Attribute{Key: "total_count", Value: len(winners)},
	)
	span.SetStatus(tracing.SpanStatusOK, "")

	logger.Info("llm_provider_compose",
		"op", spanNameProviderCompose,
		"composer", d.name,
		"builtin_count", len(builtins),
		"config_count", len(configProviders),
		"extension_count", len(extensionProviders),
		"total_count", len(winners),
	)
	return reg, nil
}

// selectWinningProviders resolves one winning ProviderEntry per distinct name:
// a higher source beats a lower one, then a higher per-entry Priority, then the
// later-registered entry. The winners are returned ordered by descending source
// so higher-priority providers appear first in List()/Default().
func selectWinningProviders(entries []ProviderEntry) []ProviderEntry {
	type candidate struct {
		entry ProviderEntry
		seq   int
	}
	best := make(map[string]candidate)
	for seq, e := range entries {
		if e.Provider == nil {
			continue
		}
		name := e.Name
		if name == "" {
			name = e.Provider.Name()
		}
		cur, ok := best[name]
		if !ok || beats(e, cur.entry, seq, cur.seq) {
			best[name] = candidate{entry: e, seq: seq}
		}
	}

	winners := make([]ProviderEntry, 0, len(best))
	for _, c := range best {
		winners = append(winners, c.entry)
	}
	// Stable sort by descending source so higher-priority providers come first.
	sort.SliceStable(winners, func(i, j int) bool {
		return winners[i].Source > winners[j].Source
	})
	return winners
}

// beats reports whether the new entry outranks the current best. A higher
// source always wins; on equal sources a higher Priority wins; on a full tie the
// later-registered entry (larger seq) wins.
func beats(newEntry ProviderEntry, cur ProviderEntry, newSeq, curSeq int) bool {
	if newEntry.Source != cur.Source {
		return newEntry.Source > cur.Source
	}
	if newEntry.Priority != cur.Priority {
		return newEntry.Priority > cur.Priority
	}
	return newSeq > curSeq
}

// Process-wide registry for the active ProviderComposer, mirroring the lazy
// nil-default + slog registry pattern used by internal/session and
// internal/production.
var (
	providerComposerMu          sync.RWMutex
	defaultProviderComposerInst ProviderComposer
)

// RegisterProviderComposer sets the active ProviderComposer used by
// GetProviderComposer. A nil value resets to a fresh DefaultProviderComposer.
func RegisterProviderComposer(c ProviderComposer) {
	providerComposerMu.Lock()
	defer providerComposerMu.Unlock()
	if c == nil {
		c = NewDefaultProviderComposer()
	}
	slog.Info("llm.register.provider_composer", "name", c.Name())
	defaultProviderComposerInst = c
}

// GetProviderComposer returns the active ProviderComposer, lazily defaulting to
// a fresh DefaultProviderComposer when none has been registered.
func GetProviderComposer() ProviderComposer {
	providerComposerMu.RLock()
	defer providerComposerMu.RUnlock()
	if defaultProviderComposerInst == nil {
		return NewDefaultProviderComposer()
	}
	return defaultProviderComposerInst
}
