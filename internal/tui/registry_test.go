package tui

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRendererRegistryRegisterAndGet verifies a single renderer can register one
// or more supported content types and be retrieved by each.
func TestRendererRegistryRegisterAndGet(t *testing.T) {
	reg := NewRendererRegistry()
	multi := NewMockRenderer("multi", ContentTypeSystem, "out")
	// Register support for an additional type by wrapping Supports.
	mr := wrappedRenderer{
		Renderer: multi,
		types:    []string{ContentTypeSystem, ContentTypeUser},
	}
	reg.Register(mr)

	r, ok := reg.Get(ContentTypeSystem)
	require.True(t, ok)
	assert.Equal(t, "out", r.Render(context.Background(), "", RenderOpts{}))

	r, ok = reg.Get(ContentTypeUser)
	require.True(t, ok)
	assert.Equal(t, mr, r)

	_, ok = reg.Get(ContentTypeCode)
	assert.False(t, ok, "renderer should not be indexed for unsupported type")
}

// wrappedRenderer decorates a Renderer to support a fixed set of content types,
// overriding the delegate's Supports.
type wrappedRenderer struct {
	Renderer
	types []string
}

func (w wrappedRenderer) Supports(ct string) bool {
	for _, t := range w.types {
		if t == ct {
			return true
		}
	}
	return false
}

func (w wrappedRenderer) Name() string { return w.Renderer.Name() }

// TestRendererRegistryRegisterOverwrites verifies last-writer-wins for duplicate
// content types.
func TestRendererRegistryRegisterOverwrites(t *testing.T) {
	reg := NewRendererRegistry()
	a := NewMockRenderer("a", ContentTypeCode, "first")
	b := NewMockRenderer("b", ContentTypeCode, "second")
	reg.Register(a)
	reg.Register(b)

	r, ok := reg.Get(ContentTypeCode)
	require.True(t, ok)
	require.Equal(t, b, r, "second registration should overwrite the first")
	assert.Equal(t, "second", r.Render(context.Background(), "", RenderOpts{}))
}

// TestRendererRegistryListSnapshot verifies List returns a copy that does not
// alias the internal map.
func TestRendererRegistryListSnapshot(t *testing.T) {
	reg := NewDefaultRegistry()
	list := reg.List()
	require.Len(t, list, 25)

	// Mutating the returned snapshot must not affect the registry.
	list[ContentTypeCode] = NewMockRenderer("mutated", ContentTypeCode, "x")
	got, _ := reg.Get(ContentTypeCode)
	require.NotEqual(t, list[ContentTypeCode], got)
	assert.Equal(t, "code", got.Name())
}

// TestRendererRegistryGetUnknown verifies Get reports false for an empty
// string and for arbitrary unknown content types.
func TestRendererRegistryGetUnknown(t *testing.T) {
	reg := NewDefaultRegistry()
	for _, ct := range []string{"", "unknown", "markdown;drop"} {
		_, ok := reg.Get(ct)
		assert.False(t, ok, "expected %q to be absent", ct)
	}
}

// TestRendererRegistryConcurrent verifies the registry is safe for concurrent
// Register/Get/List under the -race detector.
func TestRendererRegistryConcurrent(t *testing.T) {
	reg := NewRendererRegistry()
	const workers = 32
	var wg sync.WaitGroup
	wg.Add(workers)

	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				name := string(rune('a' + (i+j)%26))
				reg.Register(NewMockRenderer(name, ContentTypeCode, "x"))
				_, _ = reg.Get(ContentTypeCode)
				_ = reg.List()
			}
		}(i)
	}
	wg.Wait()
}

// TestRendererRegistryConcurrentDefault verifies the pre-populated default
// registry can be read concurrently.
func TestRendererRegistryConcurrentDefault(t *testing.T) {
	reg := NewDefaultRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				r, ok := reg.Get(ContentTypeStatus)
				if ok {
					_ = r.Name()
				}
				_ = reg.List()
			}
		}()
	}
	wg.Wait()
}

// TestRegisterDefaultRenderersIdempotent verifies registering the built-ins
// twice is harmless and still yields exactly 25 entries.
func TestRegisterDefaultRenderersIdempotent(t *testing.T) {
	reg := NewRendererRegistry()
	RegisterDefaultRenderers(reg)
	RegisterDefaultRenderers(reg)
	require.Len(t, reg.List(), 25)
}

// TestNewDefaultRegistryFullyPopulated verifies the default registry indexes
// every content type constant to a non-nil renderer.
func TestNewDefaultRegistryFullyPopulated(t *testing.T) {
	reg := NewDefaultRegistry()
	for _, ct := range contentTypes {
		if ct == ContentTypeSpinner {
			continue // SpinnerRenderer is a standalone component, not a Renderer
		}
		r, ok := reg.Get(ct)
		require.True(t, ok, "%q missing", ct)
		require.NotNil(t, r)
		assert.Equal(t, ct, r.Name())
	}
}
