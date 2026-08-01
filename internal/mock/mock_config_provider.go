package mock

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/extension"
)

// MockConfigProvider simulates a configuration provider. It supports loading
// typed values via JSON round-tripping and notifying watchers of changes.
type MockConfigProvider struct {
	mu       sync.Mutex
	configs  map[string]any
	watchers map[string][]chan extension.ConfigChange
}

// Compile-time assertion that the mock provider satisfies the config contract.
var _ extension.ConfigProvider = (*MockConfigProvider)(nil)

// NewMockConfigProvider creates an empty mock config provider.
func NewMockConfigProvider() *MockConfigProvider {
	return &MockConfigProvider{
		configs:  make(map[string]any),
		watchers: make(map[string][]chan extension.ConfigChange),
	}
}

// SetConfig presets a configuration value for the given key.
func (p *MockConfigProvider) SetConfig(key string, value any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.configs[key] = value
}

// GetConfig returns the raw value stored for the key, if present.
func (p *MockConfigProvider) GetConfig(key string) (any, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.configs[key]
	return v, ok
}

// Name implements extension.ConfigProvider.
func (p *MockConfigProvider) Name() string { return "mock" }

// Load implements extension.ConfigProvider by JSON round-tripping the stored
// value into target.
func (p *MockConfigProvider) Load(_ context.Context, key string, target any) error {
	p.mu.Lock()
	value, ok := p.configs[key]
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("config not found: %s", key)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return json.Unmarshal(data, target)
}

// Watch implements extension.ConfigProvider by returning a buffered channel
// that receives NotifyChange events for the key.
func (p *MockConfigProvider) Watch(_ context.Context, key string) (<-chan extension.ConfigChange, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch := make(chan extension.ConfigChange, 16)
	p.watchers[key] = append(p.watchers[key], ch)
	return ch, nil
}

// NotifyChange broadcasts a config change to all watchers of the key without
// blocking. If a watcher's buffer is full the event is dropped.
func (p *MockConfigProvider) NotifyChange(key string, oldValue, newValue any) {
	p.mu.Lock()
	watchers := append([]chan extension.ConfigChange(nil), p.watchers[key]...)
	p.mu.Unlock()

	change := extension.ConfigChange{
		Key:       key,
		OldValue:  oldValue,
		NewValue:  newValue,
		Timestamp: time.Now(),
	}
	for _, ch := range watchers {
		select {
		case ch <- change:
		default:
			// Buffer full: drop the event to avoid blocking the test.
		}
	}
}
