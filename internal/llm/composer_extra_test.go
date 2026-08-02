package llm

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestDefaultProviderComposer_ExtensionSourceError verifies an extension-source
// failure aborts Compose with the error and a nil registry.
func TestDefaultProviderComposer_ExtensionSourceError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	boom := errors.New("llm: extension source boom")
	c := NewDefaultProviderComposer(WithExtensionProviderSource(func(context.Context) ([]ModelProvider, error) {
		return nil, boom
	}))
	reg, err := c.Compose(context.Background())
	require.ErrorIs(t, err, boom)
	assert.Nil(t, reg)
}

// TestDefaultProviderComposer_ConfigSourceError verifies a config-source
// failure aborts Compose (before the extension source is consulted).
func TestDefaultProviderComposer_ConfigSourceError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	boom := errors.New("llm: config source boom")
	extCalled := false
	c := NewDefaultProviderComposer(
		WithConfigProviderSource(func(context.Context) ([]ModelProvider, error) {
			return nil, boom
		}),
		WithExtensionProviderSource(func(context.Context) ([]ModelProvider, error) {
			extCalled = true
			return nil, nil
		}),
	)
	reg, err := c.Compose(context.Background())
	require.ErrorIs(t, err, boom)
	assert.Nil(t, reg)
	assert.False(t, extCalled, "extension source must not be consulted after config failure")
}

// TestSelectWinningProviders_Empty verifies the empty-entry and all-nil cases.
func TestSelectWinningProviders_Empty(t *testing.T) {
	assert.Empty(t, selectWinningProviders(nil))
	assert.Empty(t, selectWinningProviders([]ProviderEntry{}))
	// Entries with nil providers are skipped entirely.
	assert.Empty(t, selectWinningProviders([]ProviderEntry{
		{Name: "a", Source: ProviderSourceConfig},
	}))
}

// TestSelectWinningProviders_NameResolution verifies an empty entry Name falls
// back to the provider's own Name() for the conflict-resolution key.
func TestSelectWinningProviders_NameResolution(t *testing.T) {
	builtin := NewEinoProvider(WithProviderName("shared"))
	ext := NewEinoProvider(WithProviderName("shared"))
	winners := selectWinningProviders([]ProviderEntry{
		{Name: "", Source: ProviderSourceBuiltin, Provider: builtin},
		{Name: "", Source: ProviderSourceExtension, Provider: ext},
	})
	require.Len(t, winners, 1)
	// Both entries are keyed under "shared" by provider name; extension wins.
	assert.Same(t, ext, winners[0].Provider)
}

// TestSelectWinningProviders_LaterRegisteredWins verifies a full tie (same
// source and priority) is broken by the later-registered entry.
func TestSelectWinningProviders_LaterRegisteredWins(t *testing.T) {
	first := NewEinoProvider(WithProviderName("tie"))
	second := NewEinoProvider(WithProviderName("tie"))
	winners := selectWinningProviders([]ProviderEntry{
		{Name: "tie", Source: ProviderSourceBuiltin, Provider: first, Priority: 0},
		{Name: "tie", Source: ProviderSourceBuiltin, Provider: second, Priority: 0},
	})
	require.Len(t, winners, 1)
	assert.Same(t, second, winners[0].Provider)
}

// TestSelectWinningProviders_AcrossSourceTies verifies the source outranks the
// per-entry priority: a lower source with a higher Priority still loses.
func TestSelectWinningProviders_AcrossSourceTies(t *testing.T) {
	lowSourceHighPrio := NewEinoProvider(WithProviderName("x"))
	highSourceLowPrio := NewEinoProvider(WithProviderName("x"))
	winners := selectWinningProviders([]ProviderEntry{
		{Name: "x", Source: ProviderSourceConfig, Provider: lowSourceHighPrio, Priority: 100},
		{Name: "x", Source: ProviderSourceExtension, Provider: highSourceLowPrio, Priority: 1},
	})
	require.Len(t, winners, 1)
	assert.Same(t, highSourceLowPrio, winners[0].Provider, "extension source wins regardless of priority")
}

// TestProviderComposerRegistryConcurrent stress-tests Register/Get under -race.
func TestProviderComposerRegistryConcurrent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				RegisterProviderComposer(NewDefaultProviderComposer())
			} else {
				RegisterProviderComposer(nil)
			}
			c := GetProviderComposer()
			if c == nil || c.Name() == "" {
				t.Errorf("composer name empty: %+v", c)
			}
		}(i)
	}
	wg.Wait()

	// After the stress, a registered custom composer must be returned as-is.
	custom := NewDefaultProviderComposer()
	RegisterProviderComposer(custom)
	assert.Same(t, custom, GetProviderComposer())
}
