// Package extension defines the configuration-provider contract that both the
// mock framework and real providers satisfy. It declares the configuration
// change model and the provider interface used to load, watch and apply
// configuration.
package extension

//nolint:scan008 // pure type definitions, no executable logic

import (
	"context"
	"time"
)

// ConfigChange describes a single configuration value transition.
type ConfigChange struct {
	// Key is the dotted configuration key that changed.
	Key string `json:"key"`
	// OldValue is the previous value, or nil if unreported.
	OldValue any `json:"old_value,omitempty"`
	// NewValue is the new value, or nil if unreported.
	NewValue any `json:"new_value,omitempty"`
	// Timestamp records when the change happened.
	Timestamp time.Time `json:"timestamp,omitempty"`
}

// ConfigProvider is the contract a configuration source satisfies. It supports
// loading typed values and watching for changes.
type ConfigProvider interface {
	// Name returns the provider identifier.
	Name() string
	// Load reads the value for key and unmarshals it into target.
	Load(ctx context.Context, key string, target any) error
	// Watch returns a channel that emits ConfigChange events for key. The
	// channel is closed when the context is canceled or the provider shuts
	// down.
	Watch(ctx context.Context, key string) (<-chan ConfigChange, error)
}
