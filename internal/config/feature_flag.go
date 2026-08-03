package config

import (
	"errors"
	"log/slog"
	"sort"
	"sync"
)

// FeatureFlag represents a runtime feature toggle.
type FeatureFlag struct {
	Name        string
	Enabled     bool
	Description string
}

// ErrFlagNotFound is returned by Set when the named flag has not been
// registered.
var ErrFlagNotFound = errors.New("feature flag not found")

// FeatureFlagRegistry manages feature flags at runtime. It is safe for
// concurrent use.
type FeatureFlagRegistry struct {
	mu    sync.RWMutex
	flags map[string]*FeatureFlag
}

// NewFeatureFlagRegistry returns an empty registry.
func NewFeatureFlagRegistry() *FeatureFlagRegistry {
	return &FeatureFlagRegistry{flags: make(map[string]*FeatureFlag)}
}

// Register adds a new flag. If a flag with the same name exists its value and
// description are overwritten.
func (r *FeatureFlagRegistry) Register(flag FeatureFlag) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flags[flag.Name] = &FeatureFlag{
		Name:        flag.Name,
		Enabled:     flag.Enabled,
		Description: flag.Description,
	}
	slog.Info("config.feature_flag.register",
		"op", "config.feature_flag.register",
		"name", flag.Name,
		"enabled", flag.Enabled,
	)
}

// IsEnabled returns the state of the named flag, or false when it is not
// registered.
func (r *FeatureFlagRegistry) IsEnabled(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if f, ok := r.flags[name]; ok {
		return f.Enabled
	}
	return false
}

// Set updates the enabled state of an existing flag. It returns
// ErrFlagNotFound when the flag has not been registered.
func (r *FeatureFlagRegistry) Set(name string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.flags[name]
	if !ok {
		return ErrFlagNotFound
	}
	f.Enabled = enabled
	slog.Info("config.feature_flag.set",
		"op", "config.feature_flag.set",
		"name", name,
		"enabled", enabled,
	)
	return nil
}

// List returns all registered flags sorted by name.
func (r *FeatureFlagRegistry) List() []FeatureFlag {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]FeatureFlag, 0, len(r.flags))
	for _, f := range r.flags {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// LoadFromConfig bulk-loads enabled states from cfg. Only flags that have
// already been registered are updated; unknown keys are ignored.
func (r *FeatureFlagRegistry) LoadFromConfig(cfg map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, enabled := range cfg {
		if f, ok := r.flags[name]; ok {
			f.Enabled = enabled
		}
	}
	slog.Info("config.feature_flag.load",
		"op", "config.feature_flag.load",
		"entries", len(cfg),
	)
}
