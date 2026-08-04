package cli

import (
	"context"
	"io"
	"testing"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAssembleTestConfig returns a minimal Config whose provider section forces
// buildModel down the EinoProvider path. EinoProvider.Build only constructs the
// HTTPChatModel object (no network calls), so assembly succeeds without a live
// endpoint.
func newAssembleTestConfig() *config.Config {
	return &config.Config{
		Provider: config.ProviderConfig{
			Name:    "openai",
			BaseURL: "http://127.0.0.1:0",
			APIKey:  "test-key",
		},
	}
}

// TestAssembleAgent_RegistryNotNil verifies that AssembleAgent populates the
// Registry field on the returned AgentAssembly.
func TestAssembleAgent_RegistryNotNil(t *testing.T) {
	assembly, err := AssembleAgent(
		context.Background(),
		newAssembleTestConfig(),
		"openai", "test-model",
		io.Discard,
	)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	require.NotNil(t, assembly.Registry, "Registry must be populated by AssembleAgent")
}

// TestAssembleAgent_RegistryRetrieval verifies that every component created
// during assembly is registered and retrievable through the Registry getters.
func TestAssembleAgent_RegistryRetrieval(t *testing.T) {
	assembly, err := AssembleAgent(
		context.Background(),
		newAssembleTestConfig(),
		"openai", "test-model",
		io.Discard,
	)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	reg := assembly.Registry
	require.NotNil(t, reg)

	// ModelProvider: the chatModelProvider adapter reports the assembled model name.
	mp := reg.ModelProvider()
	require.NotNil(t, mp)
	assert.Equal(t, "test-model", mp.Name())

	// ToolRegistry: the registered registry is the same wrapped registry exposed
	// on the assembly.
	tr := reg.ToolRegistry()
	require.NotNil(t, tr)
	assert.Same(t, assembly.ToolRegistry, tr)

	// Approval subsystem adapters are registered and non-nil.
	assert.NotNil(t, reg.ApprovalClassifier())
	assert.NotNil(t, reg.ApprovalStore())

	// Compaction subsystem adapters are registered and non-nil.
	assert.NotNil(t, reg.Compactor())
	assert.NotNil(t, reg.TokenEstimator())
}

// TestAssembleAgent_RegistryOverride verifies that a caller can replace a
// registered component via RegisterXxx after assembly, proving the Registry
// enables dependency-injection overrides.
func TestAssembleAgent_RegistryOverride(t *testing.T) {
	assembly, err := AssembleAgent(
		context.Background(),
		newAssembleTestConfig(),
		"openai", "test-model",
		io.Discard,
	)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	reg := assembly.Registry
	require.NotNil(t, reg)

	// The provider registered during assembly reports the assembled model name.
	require.Equal(t, "test-model", reg.ModelProvider().Name())

	// Override the ModelProvider with a fake and confirm the getter returns it.
	fake := &fakeModelProvider{name: "overridden"}
	prev := reg.RegisterModelProvider(fake)
	require.NotNil(t, prev)
	assert.Equal(t, "test-model", prev.Name(), "previous provider should be the assembled one")

	got := reg.ModelProvider()
	assert.Same(t, fake, got, "RegisterModelProvider must replace the bound provider")
	assert.Equal(t, "overridden", got.Name())
}

// fakeModelProvider is a minimal llm.ModelProvider used to verify RegisterXxx
// overrides.
type fakeModelProvider struct {
	name string
}

func (f *fakeModelProvider) Name() string { return f.name }

func (f *fakeModelProvider) Build(_ context.Context, _ llm.ModelConfig) (llm.BaseChatModel, func(), error) {
	return nil, func() {}, nil
}

func (f *fakeModelProvider) Models() []llm.ModelInfo { return nil }

var _ llm.ModelProvider = (*fakeModelProvider)(nil)
