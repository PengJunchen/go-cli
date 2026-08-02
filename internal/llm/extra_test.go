package llm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// ProviderRegistry edge cases
// ---------------------------------------------------------------------------

// TestProviderRegistryEmpty verifies an empty registry: Default returns nil,
// Get returns not-found, List is empty.
func TestProviderRegistryEmpty(t *testing.T) {
	reg := &ProviderRegistry{providers: map[string]ModelProvider{}}
	assert.Nil(t, reg.Default())
	assert.Empty(t, reg.List())
	_, err := reg.Get("any")
	require.ErrorIs(t, err, errProviderNotFound)

	// GetModel on a missing provider returns the not-found error.
	_, _, err = reg.GetModel(context.Background(), "missing", ModelConfig{Model: "m"})
	require.ErrorIs(t, err, errProviderNotFound)

	// Registering a valid provider into an empty registry works.
	require.NoError(t, reg.Register(NewEinoProvider(WithProviderName("p1"))))
	got, err := reg.Get("p1")
	require.NoError(t, err)
	assert.Equal(t, "p1", got.Name())
	assert.NotNil(t, reg.Default())
}

// TestProviderRegistryListIsCopy verifies the returned slice is a copy.
func TestProviderRegistryListIsCopy(t *testing.T) {
	reg := NewProviderRegistry()
	providers := reg.List()
	require.Len(t, providers, 1)
	// Mutating the returned slice must not affect the registry.
	providers[0] = nil
	require.Len(t, reg.List(), 1)
	assert.NotNil(t, reg.List()[0])
}

// ---------------------------------------------------------------------------
// Conversion helper edge cases
// ---------------------------------------------------------------------------

// TestCanonicalToolCallID_BarePrefix verifies bare prefix values are left
// unchanged rather than producing a dangling prefix.
func TestCanonicalToolCallID_BarePrefix(t *testing.T) {
	assert.Equal(t, "", canonicalToolCallID(""))
	assert.Equal(t, "call_", canonicalToolCallID("call_"))
	assert.Equal(t, "toolu_", canonicalToolCallID("toolu_"))
	// A bare "call_x" is stripped then re-prefixed.
	assert.Equal(t, "call_x", canonicalToolCallID("call_x"))
}

// TestNormalizeToolCallID_NoChangeReusesSlice verifies that when no ID changes,
// normalizeToolCalls returns the original slice (aliasing, not a copy).
func TestNormalizeToolCallID_NoChangeReusesSlice(t *testing.T) {
	in := []Message{{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_keep"}}}}
	out := NormalizeToolCallID(in)
	// The tool-calls backing array must be shared when nothing changed.
	assert.True(t, &in[0].ToolCalls[0] == &out[0].ToolCalls[0])
}

// TestNormalizeToolCallID_ChangedCreatesNewSlice verifies a change produces a
// fresh slice and leaves the input untouched.
func TestNormalizeToolCallID_ChangedCreatesNewSlice(t *testing.T) {
	in := []Message{{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "toolu_9"}}}}
	out := NormalizeToolCallID(in)
	require.Len(t, out, 1)
	assert.Equal(t, "call_9", out[0].ToolCalls[0].ID)
	// Input stays unchanged.
	assert.Equal(t, "toolu_9", in[0].ToolCalls[0].ID)
}

// TestDowngradeImages_EmptyContentPlaceholder verifies messages without an
// image marker are returned unchanged (aliased), and messages with a marker
// are rewritten without touching the input.
func TestDowngradeImages_NoMarkerMiddle(t *testing.T) {
	in := []Message{{Role: RoleUser, Content: "before [image:a] after"}}
	out := DowngradeImages(in)
	require.Len(t, out, 1)
	assert.Equal(t, "before [image omitted: not supported by this provider] after", out[0].Content)
	assert.Equal(t, "before [image:a] after", in[0].Content)
}

// TestAdaptThinking_NoMarkerKeepsContent verifies content without thinking
// markers returns a message whose Content is unchanged.
func TestAdaptThinking_NoMarkerKeepsContent(t *testing.T) {
	in := []Message{{Role: RoleUser, Content: "plain"}}
	out := AdaptThinking(in)
	require.Len(t, out, 1)
	assert.Equal(t, "plain", out[0].Content)
}

// TestStripThinking verifies the internal helper strips both marker styles.
func TestStripThinking(t *testing.T) {
	out := stripThinking("inline [thinking:x] done [thinking_start]hid[thinking_end] rest")
	assert.Equal(t, "inline  done  rest", out)
	// Unbalanced end marker with no start is left as-is.
	assert.Equal(t, "a [thinking_end] b", stripThinking("a [thinking_end] b"))
}
