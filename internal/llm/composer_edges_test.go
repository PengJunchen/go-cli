package llm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestBeats_UnitVerifiesPrecedence tests the three-way comparison precedence of
// beats directly: a higher source wins, then a higher Priority, then the
// later-registered (larger sequence) entry. This is the unit-level contract that
// selectWinningProviders builds on.
func TestBeats_UnitVerifiesPrecedence(t *testing.T) {
	cases := []struct {
		name     string
		newEntry ProviderEntry
		cur      ProviderEntry
		newSeq   int
		curSeq   int
		want     bool
	}{
		{
			name:     "higher source wins",
			newEntry: ProviderEntry{Name: "x", Source: ProviderSourceExtension},
			cur:      ProviderEntry{Name: "x", Source: ProviderSourceConfig},
			want:     true,
		},
		{
			name:     "lower source loses",
			newEntry: ProviderEntry{Name: "x", Source: ProviderSourceBuiltin},
			cur:      ProviderEntry{Name: "x", Source: ProviderSourceConfig},
			want:     false,
		},
		{
			name:     "equal source higher priority wins",
			newEntry: ProviderEntry{Name: "x", Source: ProviderSourceConfig, Priority: 5},
			cur:      ProviderEntry{Name: "x", Source: ProviderSourceConfig, Priority: 2},
			want:     true,
		},
		{
			name:     "equal source lower priority loses",
			newEntry: ProviderEntry{Name: "x", Source: ProviderSourceConfig, Priority: 1},
			cur:      ProviderEntry{Name: "x", Source: ProviderSourceConfig, Priority: 9},
			want:     false,
		},
		{
			name:     "full tie later seq wins",
			newEntry: ProviderEntry{Name: "x", Source: ProviderSourceConfig, Priority: 3},
			cur:      ProviderEntry{Name: "x", Source: ProviderSourceConfig, Priority: 3},
			newSeq:   10,
			curSeq:   2,
			want:     true,
		},
		{
			name:     "full tie earlier seq loses",
			newEntry: ProviderEntry{Name: "x", Source: ProviderSourceConfig, Priority: 3},
			cur:      ProviderEntry{Name: "x", Source: ProviderSourceConfig, Priority: 3},
			newSeq:   1,
			curSeq:   7,
			want:     false,
		},
		{
			name:     "source beats priority across layers",
			newEntry: ProviderEntry{Name: "x", Source: ProviderSourceExtension, Priority: 0},
			cur:      ProviderEntry{Name: "x", Source: ProviderSourceConfig, Priority: 100},
			want:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := beats(tc.newEntry, tc.cur, tc.newSeq, tc.curSeq)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestDefaultProviderComposer_AllSourcesNil verifies the fully-default composer
// (no config/extension source) contributes only the builtin providers with no
// error, and every builtin name resolves.
func TestDefaultProviderComposer_AllSourcesNil(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	c := NewDefaultProviderComposer()
	reg, err := c.Compose(context.Background())
	require.NoError(t, err)
	require.NotNil(t, reg)
	got := reg.List()
	require.Len(t, got, 4)
	names := make(map[string]bool, len(got))
	for _, p := range got {
		names[p.Name()] = true
	}
	for _, want := range []string{"eino", "openai", "claude", "gemini"} {
		assert.True(t, names[want], "missing builtin provider %q", want)
	}
}

// TestNewDefaultProviderComposer_NilOptions verifies that explicit nil options
// are skipped without panicking.
func TestNewDefaultProviderComposer_NilOptions(t *testing.T) {
	c := NewDefaultProviderComposer(nil, nil)
	require.NotNil(t, c)
	assert.Equal(t, "default-provider-composer", c.Name())
}

// TestDefaultProviderComposer_MixedSourceMerge verifies that config and
// extension providers that do not collide with any builtin name are all
// present in the resulting registry, and that the descending-source sort puts
// extension winners first in List() order.
func TestDefaultProviderComposer_MixedSourceMerge(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	cfg := NewEinoProvider(WithProviderName("cfg-only"))
	ext := NewEinoProvider(WithProviderName("ext-only"))
	c := NewDefaultProviderComposer(
		WithConfigProviders([]ModelProvider{cfg}),
		WithExtensionProviders([]ModelProvider{ext}),
	)
	reg, err := c.Compose(context.Background())
	require.NoError(t, err)

	list := reg.List()
	require.Len(t, list, 6) // 4 builtin + cfg + ext

	// The first entries must be the extension-sourced one, because winners are
	// sorted by descending source.
	assert.Equal(t, "ext-only", list[0].Name())

	// Every expected name is resolvable via Get.
	for _, name := range []string{"eino", "openai", "claude", "gemini", "cfg-only", "ext-only"} {
		p, gerr := reg.Get(name)
		require.NoError(t, gerr, "expected provider %q", name)
		assert.Equal(t, name, p.Name())
	}
}
