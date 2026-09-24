package llm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProviderRegistry_RegisterErrors verifies the Register error contracts:
// nil providers panic, unnamed providers are rejected, and duplicate names fail
// while leaving the existing registration untouched.
func TestProviderRegistry_RegisterErrors(t *testing.T) {
	reg := NewProviderRegistry()

	// A nil provider panics.
	assert.Panics(t, func() {
		_ = reg.Register(nil) //nolint:errcheck // panics before returning; return value irrelevant
	})

	// A provider with an empty name is rejected.
	assert.Error(t, reg.Register(&stubProvider{}))

	// Registering the default "eino" name again fails.
	err := reg.Register(NewEinoProvider())
	require.ErrorIs(t, err, errProviderAlreadyRegistered)

	// The original registration is untouched and still usable.
	p, gerr := reg.Get("eino")
	require.NoError(t, gerr)
	assert.NotNil(t, p)
}

// TestProviderRegistry_DefaultOnFresh verifies a freshly constructed registry
// defaults to the pre-registered Eino provider and lists exactly one entry.
func TestProviderRegistry_DefaultOnFresh(t *testing.T) {
	reg := NewProviderRegistry()
	def := reg.Default()
	require.NotNil(t, def)
	assert.Equal(t, "eino", def.Name())
	assert.Len(t, reg.List(), 1)
}

// TestProviderRegistryRejectsUnspecifiedCustomProvider verifies that registry
// presence alone cannot enable an unknown wire protocol.
func TestProviderRegistryRejectsUnspecifiedCustomProvider(t *testing.T) {
	reg := NewProviderRegistry()
	failing := &countingModelProvider{name: "boom"}
	require.NoError(t, reg.Register(failing))
	_, cleanup, err := reg.GetModel(context.Background(), "boom", ModelConfig{Model: "m", BaseURL: "http://127.0.0.1:8080/v1"})
	require.ErrorIs(t, err, ErrUnsupportedProviderProtocol)
	assert.Nil(t, cleanup)
	assert.Zero(t, failing.buildCalls)
}

// stubProvider is a minimal ModelProvider used to exercise registry
// error/registration paths without depending on a concrete provider.
type stubProvider struct {
	name string
}

func (s *stubProvider) Name() string {
	if s.name == "" {
		return ""
	}
	return s.name
}

func (s *stubProvider) Build(context.Context, ModelConfig) (BaseChatModel, func(), error) {
	return &stubModel{}, func() {}, nil
}

func (s *stubProvider) Models() []ModelInfo { return nil }

// stubModel is a no-op BaseChatModel used by stubProvider.
type stubModel struct{}

func (m *stubModel) Generate(_ context.Context, msgs []Message, _ ...Option) (*Message, error) {
	return &Message{Role: RoleAssistant}, nil
}

func (m *stubModel) Stream(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
	ch := make(chan MessageChunk)
	close(ch)
	return ch, nil
}
