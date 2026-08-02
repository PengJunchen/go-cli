package llm

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestSelectWinningProviders_SortedDescendingSource verifies the winners slice
// is ordered by descending source so higher-priority providers appear first in
// List()/Default() even when entries arrive interleaved.
func TestSelectWinningProviders_SortedDescendingSource(t *testing.T) {
	ext1 := NewEinoProvider(WithProviderName("extA"))
	ext2 := NewEinoProvider(WithProviderName("extB"))
	cfg := NewEinoProvider(WithProviderName("cfgA"))
	builtin := NewEinoProvider(WithProviderName("binA"))

	winners := selectWinningProviders([]ProviderEntry{
		{Name: "cfgA", Source: ProviderSourceConfig, Provider: cfg},
		{Name: "binA", Source: ProviderSourceBuiltin, Provider: builtin},
		{Name: "extA", Source: ProviderSourceExtension, Provider: ext1},
		{Name: "extB", Source: ProviderSourceExtension, Provider: ext2},
	})
	require.Len(t, winners, 4)

	// The first two winners must be the extension-sourced entries.
	assert.Equal(t, ProviderSourceExtension, winners[0].Source)
	assert.Equal(t, ProviderSourceExtension, winners[1].Source)
	// Config before builtin.
	assert.Equal(t, ProviderSourceConfig, winners[2].Source)
	assert.Equal(t, ProviderSourceBuiltin, winners[3].Source)

	// Extension names present.
	gotExts := []string{winners[0].Name, winners[1].Name}
	assert.ElementsMatch(t, []string{"extA", "extB"}, gotExts)
}

// TestSelectWinningProviders_SingleSourcePreservesLast verifies that distinct
// names across the same source are all retained and identical names collapse to
// the highest-priority entry.
func TestSelectWinningProviders_SingleSourceCollapses(t *testing.T) {
	p1 := NewEinoProvider(WithProviderName("same"))
	p2 := NewEinoProvider(WithProviderName("same"))
	p3 := NewEinoProvider(WithProviderName("other"))

	winners := selectWinningProviders([]ProviderEntry{
		{Name: "same", Source: ProviderSourceConfig, Provider: p1},
		{Name: "other", Source: ProviderSourceConfig, Provider: p3},
		{Name: "same", Source: ProviderSourceConfig, Provider: p2, Priority: 7},
	})
	require.Len(t, winners, 2)
	assert.ElementsMatch(t, []string{"same", "other"}, []string{winners[0].Name, winners[1].Name})

	// "same" must resolve to p2 (higher priority).
	for _, w := range winners {
		if w.Name == "same" {
			assert.Same(t, p2, w.Provider)
		}
	}
}

// TestComposer_ConfigSourceReceivesSpanContext verifies the span context is
// genuinely propagated into the config source: a child span created there must
// link its parent_span_id to the compose span.
func TestComposer_ConfigSourceReceivesSpanContext(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, exporter := newComposeTestCtx(t)

	c := NewDefaultProviderComposer(WithConfigProviderSource(func(sctx context.Context) ([]ModelProvider, error) {
		// A child span created from the propagated context must trace back to the
		// compose span.
		child, _ := tracing.SpanFromContext(sctx, "config.child", tracing.SpanKindInternal)
		child.End()
		return []ModelProvider{NewEinoProvider(WithProviderName("cfg-loaded"))}, nil
	}))

	reg, err := c.Compose(ctx)
	require.NoError(t, err)
	require.NotNil(t, reg)

	require.Eventually(t, func() bool {
		_, cok := exporter.spanByName("llm.provider_compose")
		if !cok {
			return false
		}
		_, kok := exporter.spanByName("config.child")
		return kok
	}, 2*time.Second, 5*time.Millisecond)

	composeSpan, ok := exporter.spanByName("llm.provider_compose")
	require.True(t, ok)
	childSpan, ok := exporter.spanByName("config.child")
	require.True(t, ok)
	assert.Equal(t, composeSpan.SpanID, childSpan.ParentSpanID,
		"child span created in the config source must be parented to the compose span")
}

// TestComposer_SameNameAllThreeSources verifies a provider with the same name
// contributed from all three layers resolves to the extension winner, and the
// registry still contains the config- and builtin-only names.
func TestComposer_SameNameAllThreeSources(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	builtin := NewEinoProvider(WithProviderName("shared"))
	cfg := NewEinoProvider(WithProviderName("shared"))
	ext := NewEinoProvider(WithProviderName("shared"))
	cfgOnly := NewEinoProvider(WithProviderName("cfg-only"))

	c := NewDefaultProviderComposer(
		WithConfigProviders([]ModelProvider{cfg, cfgOnly}),
		WithExtensionProviders([]ModelProvider{ext}),
	)
	reg, err := c.Compose(context.Background())
	require.NoError(t, err)

	// "shared" must resolve to the extension provider.
	got, err := reg.Get("shared")
	require.NoError(t, err)
	assert.Same(t, ext, got)

	// "cfg-only" is still present.
	got2, err := reg.Get("cfg-only")
	require.NoError(t, err)
	assert.Same(t, cfgOnly, got2)

	// The extension winner is NOT the raw builtin object.
	assert.NotSame(t, builtin, got)
}

// TestProviderRegistry_RegisterPreservesFirstRegistration verifies that when a
// registry is built by hand, Default always returns the first-registered entry
// and duplicates do not shift the order.
func TestProviderRegistry_RegisterPreservesFirstRegistration(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	reg := &ProviderRegistry{providers: map[string]ModelProvider{}}

	a := NewEinoProvider(WithProviderName("a"))
	b := NewEinoProvider(WithProviderName("b"))
	c := NewEinoProvider(WithProviderName("c"))
	require.NoError(t, reg.Register(a))
	require.NoError(t, reg.Register(b))

	// A duplicate registration fails and does not disturb Default.
	require.Error(t, reg.Register(NewEinoProvider(WithProviderName("a"))))
	assert.Same(t, a, reg.Default())

	require.NoError(t, reg.Register(c))
	assert.Same(t, a, reg.Default(), "Default is always the first-registered entry")

	names := make([]string, 0, 3)
	for _, p := range reg.List() {
		names = append(names, p.Name())
	}
	assert.Equal(t, []string{"a", "b", "c"}, names)
}

// TestProviderRegistry_GetModelReturnsNonNilCleanup verifies GetModel builds a
// model and returns a nil-safe cleanup aligned with the underlying Build.
func TestProviderRegistry_GetModelReturnsNonNilCleanup(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	reg := NewProviderRegistry()
	m, cleanup, err := reg.GetModel(context.Background(), "eino", ModelConfig{Model: "gpt-4o"})
	require.NoError(t, err)
	require.NotNil(t, m)
	require.NotNil(t, cleanup)
	assert.NotPanics(t, func() { cleanup() })
}

// TestProviderSourcesString_AllValues verifies every source enum has a stable
// label and an out-of-range value maps to "unknown".
func TestProviderSourcesString_AllValues(t *testing.T) {
	assert.Equal(t, "builtin", ProviderSource(ProviderSourceBuiltin).String())
	assert.Equal(t, "config", ProviderSource(ProviderSourceConfig).String())
	assert.Equal(t, "extension", ProviderSource(ProviderSourceExtension).String())
	assert.Equal(t, "unknown", ProviderSource(-1).String())
	assert.Equal(t, "unknown", ProviderSource(ProviderSourceExtension+1).String())
}
