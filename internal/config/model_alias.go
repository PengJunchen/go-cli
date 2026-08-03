package config

import (
	"log/slog"
	"sync"
)

// DefaultAliases maps common short names to full model identifiers. Callers may
// use these as a base and extend them with AddAlias.
var DefaultAliases = map[string]string{
	"sonnet":      "claude-sonnet-4-20250514",
	"opus":        "claude-opus-4-20250514",
	"haiku":       "claude-haiku-3.5-20250315",
	"gpt-4o":      "gpt-4o",
	"gpt-4o-mini": "gpt-4o-mini",
	"flash":       "gemini-2.0-flash",
	"pro":         "gemini-2.5-pro",
}

// ModelAliasResolver resolves short model aliases to full model names. It is
// safe for concurrent use.
type ModelAliasResolver struct {
	mu      sync.RWMutex
	aliases map[string]string
}

// NewModelAliasResolver returns a resolver seeded with DefaultAliases.
func NewModelAliasResolver() *ModelAliasResolver {
	r := &ModelAliasResolver{aliases: make(map[string]string, len(DefaultAliases))}
	for k, v := range DefaultAliases {
		r.aliases[k] = v
	}
	slog.Info("config.alias.init", "op", "config.alias.init", "count", len(r.aliases))
	return r
}

// Resolve returns the full model name for alias, or alias unchanged when no
// mapping is registered.
func (r *ModelAliasResolver) Resolve(alias string) string {
	r.mu.RLock()
	full, ok := r.aliases[alias]
	r.mu.RUnlock()

	if ok {
		slog.Debug("config.alias.resolve",
			"op", "config.alias.resolve",
			"alias", alias,
			"full", full,
			"resolved", true,
		)
		return full
	}
	slog.Debug("config.alias.resolve",
		"op", "config.alias.resolve",
		"alias", alias,
		"resolved", false,
	)
	return alias
}

// AddAlias registers a mapping from alias to full. It overwrites any existing
// mapping for alias.
func (r *ModelAliasResolver) AddAlias(alias, full string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.aliases[alias] = full
	slog.Info("config.alias.add",
		"op", "config.alias.add",
		"alias", alias,
		"full", full,
	)
}

// ListAliases returns a copy of all registered aliases.
func (r *ModelAliasResolver) ListAliases() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.aliases))
	for k, v := range r.aliases {
		out[k] = v
	}
	return out
}
