package core

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// captureHandler is a minimal slog.Handler that stores every record it
// receives, allowing tests to verify which log messages were emitted.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler       { return h }

// disposeComponents extracts the "component" attribute from every
// "core.registry.dispose" record, in the order they were captured.
func disposeComponents(h *captureHandler) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var comps []string
	for _, rec := range h.records {
		if rec.Message != "core.registry.dispose" {
			continue
		}
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "component" {
				comps = append(comps, a.Value.String())
			}
			return true
		})
	}
	return comps
}

func TestRegisterWithDisposer(t *testing.T) {
	h := &captureHandler{}
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(prevLogger)

	r := NewRegistry().(DisposableRegistry)
	prev, d := r.RegisterAgentLoopWithDisposer(&testLoop{tag: "x"})
	assert.NotNil(t, prev, "previous implementation should be returned")
	assert.NotNil(t, d, "disposer should be non-nil")

	// Calling the disposer directly should produce a dispose log record.
	d()
	comps := disposeComponents(h)
	assert.Equal(t, []string{"AgentLoop"}, comps, "disposer should have been called once")
}

func TestDisposeAll(t *testing.T) {
	h := &captureHandler{}
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(prevLogger)

	r := NewRegistry().(DisposableRegistry)
	r.RegisterAgentLoopWithDisposer(&testLoop{tag: "1"})
	r.RegisterAgentWithDisposer(&fakeEventStreamAgent{})
	r.RegisterHarnessWithDisposer(&HarnessImpl{})
	r.RegisterToolRegistryWithDisposer(&DefaultToolRegistry{})
	r.RegisterModelProviderWithDisposer(&DefaultModelProvider{})

	r.DisposeAll()

	// Disposers must be called in reverse registration order (LIFO).
	comps := disposeComponents(h)
	assert.Equal(t,
		[]string{"ModelProvider", "ToolRegistry", "Harness", "Agent", "AgentLoop"},
		comps,
		"disposers should be called in reverse registration order",
	)
}

func TestDisposeAll_Empty(t *testing.T) {
	h := &captureHandler{}
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(prevLogger)

	r := NewRegistry().(DisposableRegistry)
	// Should be a no-op — no panic, no dispose log records.
	assert.NotPanics(t, func() { r.DisposeAll() })
	assert.Empty(t, disposeComponents(h), "no disposers should be called on empty registry")
}

func TestDisposer_Idempotent(t *testing.T) {
	r := NewRegistry().(DisposableRegistry)
	r.RegisterAgentLoopWithDisposer(&testLoop{tag: "a"})
	r.RegisterAgentWithDisposer(&fakeEventStreamAgent{})

	// First call disposes everything.
	assert.NotPanics(t, func() { r.DisposeAll() })
	// Second call must not panic (internal slice is nil/empty).
	assert.NotPanics(t, func() { r.DisposeAll() })
}
