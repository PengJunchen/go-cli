package llm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// captureExporter is a minimal in-memory tracing.TraceExporter used to assert
// on span emission inside the llm package without importing internal/mock
// (which would create an import cycle since internal/mock imports internal/llm).
type captureExporter struct {
	mu    sync.Mutex
	spans []tracing.SpanData
}

func (e *captureExporter) ExportSpan(_ context.Context, span tracing.TraceSpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, tracing.SpanToData(span))
	return nil
}

func (e *captureExporter) Shutdown(_ context.Context) error { return nil }

func (e *captureExporter) allSpans() []tracing.SpanData {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]tracing.SpanData, len(e.spans))
	copy(out, e.spans)
	return out
}

func (e *captureExporter) spanByName(name string) (tracing.SpanData, bool) {
	for _, s := range e.allSpans() {
		if s.Name == name {
			return s, true
		}
	}
	return tracing.SpanData{}, false
}

// newComposeTestCtx wires a captureExporter + Tracer into a root context so a
// parent span is present, letting us assert on parent_span_id.
func newComposeTestCtx(t *testing.T) (context.Context, *captureExporter) {
	t.Helper()
	exporter := &captureExporter{}
	tr := tracing.NewTracer("compose-trace", exporter)
	root, ctx := tr.Start(context.Background(), "cli.invocation", tracing.SpanKindInternal)
	_ = root
	return ctx, exporter
}

func findAttr(s tracing.SpanData, key string) (any, bool) {
	for _, a := range s.Attributes {
		if a.Key == key {
			return a.Value, true
		}
	}
	return nil, false
}

// Compose collects the builtin layer (Eino/OpenAI/Claude/Gemini).
func TestDefaultProviderComposer_BuiltinProviders(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	c := NewDefaultProviderComposer()
	reg, err := c.Compose(context.Background())
	require.NoError(t, err)

	want := map[string]bool{"eino": true, "openai": true, "claude": true, "gemini": true}
	for name := range want {
		p, gerr := reg.Get(name)
		require.NoError(t, gerr, "expected builtin %q", name)
		assert.Equal(t, name, p.Name())
	}
	// Exactly the four builtin providers (no config/extension supplied).
	require.Len(t, reg.List(), len(want))
}

// config providers are loaded into the composition.
func TestDefaultProviderComposer_ConfigProviders(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	cfg := NewEinoProvider(WithProviderName("custom-config"))

	sourceCalled := false
	c := NewDefaultProviderComposer(WithConfigProviderSource(func(context.Context) ([]ModelProvider, error) {
		sourceCalled = true
		return []ModelProvider{cfg}, nil
	}))

	reg, err := c.Compose(context.Background())
	require.NoError(t, err)
	require.True(t, sourceCalled, "config source should be invoked")

	got, err := reg.Get("custom-config")
	require.NoError(t, err)
	assert.Same(t, cfg, got)
}

// extension providers are registered into the composition.
func TestDefaultProviderComposer_ExtensionProviders(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ext := NewEinoProvider(WithProviderName("custom-ext"))
	c := NewDefaultProviderComposer(WithExtensionProviders([]ModelProvider{ext}))

	reg, err := c.Compose(context.Background())
	require.NoError(t, err)

	got, err := reg.Get("custom-ext")
	require.NoError(t, err)
	assert.Same(t, ext, got)
}

// Compose yields the correct priority ordering Extension > Config > Builtin,
// and GetProvider returns the winner for a name present in all three layers.
func TestDefaultProviderComposer_PriorityExtensionOverridesAll(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	// The builtin layer (via NewProviderRegistry handles "eino"/"openai"/etc.)
	// plus explicit sources supply the same name "dupe" from three layers.
	config := NewEinoProvider(WithProviderName("dupe"))
	extension := NewEinoProvider(WithProviderName("dupe"))

	c := NewDefaultProviderComposer(
		WithConfigProviders([]ModelProvider{config}),
		WithExtensionProviders([]ModelProvider{extension}),
	)

	reg, err := c.Compose(context.Background())
	require.NoError(t, err)

	got, err := reg.Get("dupe")
	require.NoError(t, err)
	// Extension wins over Config and Builtin for the same name.
	assert.Same(t, extension, got)
}

// same-name config provider overrides a builtin provider.
func TestDefaultProviderComposer_ConfigOverridesBuiltin(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	// The builtin layer provides "eino"; a config provider shadows it.
	config := NewEinoProvider(WithProviderName("eino"))

	c := NewDefaultProviderComposer(WithConfigProviders([]ModelProvider{config}))
	reg, err := c.Compose(context.Background())
	require.NoError(t, err)

	got, err := reg.Get("eino")
	require.NoError(t, err)
	assert.Same(t, config, got)
}

// Default() reflects the highest-priority source. A provider contributed
// only by the builtin layer ("gemini" via ExtProvider name) remains available,
// while Default() (the first registered) resolves to the extension-shared name.
func TestDefaultProviderComposer_DefaultReflectsPriority(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	// Extension contributes a uniquely-named provider plus shadows "eino".
	ext := NewEinoProvider(WithProviderName("shadowed-ext"))
	c := NewDefaultProviderComposer(WithExtensionProviders([]ModelProvider{ext}))

	reg, err := c.Compose(context.Background())
	require.NoError(t, err)

	// Winners are ordered high-priority first, so the extension-shared provider
	// leads the registry and appears as the default.
	got, err := reg.Get("shadowed-ext")
	require.NoError(t, err)
	assert.Same(t, ext, got)
}

// TestDefaultProviderComposer_PriorityTieBreaker verifies the winner selection
// helper honors the per-entry Priority as a tie-breaker within a source.
func TestDefaultProviderComposer_TieBreaker(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	low := NewEinoProvider(WithProviderName("tie"))
	high := NewEinoProvider(WithProviderName("tie"))

	entries := []ProviderEntry{
		{Name: "tie", Source: ProviderSourceConfig, Provider: low, Priority: 1},
		{Name: "tie", Source: ProviderSourceConfig, Provider: high, Priority: 10},
	}
	winners := selectWinningProviders(entries)
	require.Len(t, winners, 1)
	assert.Same(t, high, winners[0].Provider)
}

// GetProvider via the returned registry returns the winner for names that
// only exist in one layer, and does not leak losing providers.
func TestDefaultProviderComposer_GetProviderReturnsWinner(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	extOnly := NewEinoProvider(WithProviderName("ext-only"))
	shadowed := NewEinoProvider(WithProviderName("eino")) // overrides builtin eino

	c := NewDefaultProviderComposer(WithExtensionProviders([]ModelProvider{extOnly, shadowed}))
	reg, err := c.Compose(context.Background())
	require.NoError(t, err)

	// A provider only present in the extension layer resolves to that provider.
	got, err := reg.Get("ext-only")
	require.NoError(t, err)
	assert.Same(t, extOnly, got)

	// Unshadowed builtin providers remain reachable by name.
	for _, name := range []string{"openai", "claude", "gemini"} {
		p, gerr := reg.Get(name)
		require.NoError(t, gerr, "expected builtin %q", name)
		assert.Equal(t, name, p.Name())
	}

	// The winning provider for "eino" is the extension, not the builtin eino.
	got, err = reg.Get("eino")
	require.NoError(t, err)
	assert.Same(t, shadowed, got)
}

// Compose emits an "llm.provider_compose" span with the four count
// attributes and a traceable parent_span_id.
func TestDefaultProviderComposer_EmitsComposeSpan(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	exporter := &captureExporter{}
	tr := tracing.NewTracer("compose-trace", exporter)
	root, ctx := tr.Start(context.Background(), "cli.invocation", tracing.SpanKindInternal)

	config := NewEinoProvider(WithProviderName("cfg-1"))
	ext := NewEinoProvider(WithProviderName("ext-1"))

	c := NewDefaultProviderComposer(
		WithConfigProviders([]ModelProvider{config}),
		WithExtensionProviders([]ModelProvider{ext}),
	)
	reg, err := c.Compose(ctx)
	require.NoError(t, err)
	root.End()

	require.Eventually(t, func() bool {
		_, ok := exporter.spanByName("llm.provider_compose")
		return ok
	}, 2*time.Second, 5*time.Millisecond)

	span, ok := exporter.spanByName("llm.provider_compose")
	require.True(t, ok, "expected llm.provider_compose span")

	assert.Equal(t, "compose-trace", span.TraceID)
	assert.Equal(t, root.SpanID(), span.ParentSpanID)
	assert.Equal(t, string(tracing.SpanKindInternal), string(span.SpanKind))

	cases := map[string]int{
		"builtin_count":   4,
		"config_count":    1,
		"extension_count": 1,
		"total_count":     len(reg.List()),
	}
	for key, want := range cases {
		v, found := findAttr(span, key)
		require.True(t, found, "missing attribute %q", key)
		assert.Equal(t, want, v, "attribute %q mismatch", key)
	}
}

// context cancellation propagates through Compose and surfaces as an
// error, and the compose span is marked as an error without a registry result.
func TestDefaultProviderComposer_ContextCancellation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled

	boom := errors.New("llm: provider source canceled")
	c := NewDefaultProviderComposer(WithConfigProviderSource(func(sctx context.Context) ([]ModelProvider, error) {
		select {
		case <-sctx.Done():
			return nil, boom
		default:
			return nil, errors.New("llm: source should have observed cancellation")
		}
	}))

	reg, err := c.Compose(ctx)
	require.ErrorIs(t, err, boom)
	assert.Nil(t, reg)
}

// RegisterProviderComposer/GetProviderComposer: process-wide lazy nil-default
// + slog registry pattern.
func TestProviderComposerRegistry(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	RegisterProviderComposer(NewDefaultProviderComposer())
	got := GetProviderComposer()
	require.NotNil(t, got)
	assert.Equal(t, "default-provider-composer", got.Name())

	// Registering nil resets to a fresh default composer.
	RegisterProviderComposer(nil)
	got = GetProviderComposer()
	require.NotNil(t, got)
	assert.Equal(t, "default-provider-composer", got.Name())

	// A registered custom composer is returned as-is.
	custom := NewDefaultProviderComposer()
	RegisterProviderComposer(custom)
	assert.Same(t, custom, GetProviderComposer())
}

// ProviderSource.String is stable and descriptive.
func TestProviderSourceString(t *testing.T) {
	assert.Equal(t, "builtin", ProviderSourceBuiltin.String())
	assert.Equal(t, "config", ProviderSourceConfig.String())
	assert.Equal(t, "extension", ProviderSourceExtension.String())
	assert.Equal(t, "unknown", ProviderSource(99).String())
}
