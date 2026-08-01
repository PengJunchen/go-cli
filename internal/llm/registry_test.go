package llm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestProviderRegistryDefaults verifies the default EinoProvider is available.
func TestProviderRegistryDefaults(t *testing.T) {
	reg := NewProviderRegistry()
	assert.Equal(t, "eino", reg.Default().Name())

	p, err := reg.Get("eino")
	require.NoError(t, err)
	assert.Equal(t, "eino", p.Name())

	providers := reg.List()
	require.Len(t, providers, 1)
}

// TestProviderRegistryRegisterGet verifies registration by name and retrieval.
func TestProviderRegistryRegisterGet(t *testing.T) {
	reg := NewProviderRegistry()
	p := NewEinoProvider(WithProviderName("openai"))

	require.NoError(t, reg.Register(p))

	got, err := reg.Get("openai")
	require.NoError(t, err)
	assert.Same(t, p, got)
}

// TestProviderRegistryDuplicate verifies a duplicate registration returns an
// error and leaves the original intact.
func TestProviderRegistryDuplicate(t *testing.T) {
	reg := NewProviderRegistry()
	first := NewEinoProvider(WithProviderName("dup"))
	second := NewEinoProvider(WithProviderName("dup"))

	require.NoError(t, reg.Register(first))
	err := reg.Register(second)
	require.Error(t, err)
	assert.ErrorIs(t, err, errProviderAlreadyRegistered)

	got, err := reg.Get("dup")
	require.NoError(t, err)
	assert.Same(t, first, got)
}

// TestProviderRegistryNotFound verifies Get returns an error for a missing name.
func TestProviderRegistryNotFound(t *testing.T) {
	reg := NewProviderRegistry()
	_, err := reg.Get("does-not-exist")
	require.Error(t, err)
	assert.ErrorIs(t, err, errProviderNotFound)
}

// TestProviderRegistryListOrder verifies List preserves registration order.
func TestProviderRegistryListOrder(t *testing.T) {
	reg := NewProviderRegistry() // contains "eino"
	require.NoError(t, reg.Register(NewEinoProvider(WithProviderName("second"))))
	require.NoError(t, reg.Register(NewEinoProvider(WithProviderName("third"))))

	names := make([]string, 0)
	for _, p := range reg.List() {
		names = append(names, p.Name())
	}
	assert.Equal(t, []string{"eino", "second", "third"}, names)
}

// TestProviderRegistryGetModel verifies the GetModel convenience builds a model.
func TestProviderRegistryGetModel(t *testing.T) {
	reg := NewProviderRegistry()
	m, cleanup, err := reg.GetModel(context.Background(), "eino", ModelConfig{Model: "gpt-4o"})
	require.NoError(t, err)
	require.NotNil(t, m)
	require.NotNil(t, cleanup)
	assert.NotPanics(t, func() { cleanup() })

	_, _, err = reg.GetModel(context.Background(), "missing", ModelConfig{})
	require.ErrorIs(t, err, errProviderNotFound)
}

// TestProviderRegistryNilPanics verifies Register panics on nil.
func TestProviderRegistryNilPanics(t *testing.T) {
	reg := NewProviderRegistry()
	assert.Panics(t, func() {
		_ = reg.Register(nil) //nolint:errcheck // Register panics before returning
	})
}

// TestProviderRegistryEmptyName verifies a nameless provider is rejected.
func TestProviderRegistryEmptyName(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	reg := NewProviderRegistry()
	err := reg.Register(NewEinoProvider(WithProviderName("")))
	require.Error(t, err)
}
