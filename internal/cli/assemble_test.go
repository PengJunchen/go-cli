package cli //nolint:scan003

import (
	"context"
	"io"
	"testing"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"

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

// TestAssembleAgent_WiresPartialComponents verifies that AssembleAgent creates
// and exposes the 5 PARTIAL components: FileTracker (D5), DiffGenerator (D6),
// and PlanModeController (D9/19-9). These must be non-nil on the returned
// AgentAssembly so slash commands and the interactive session can use them.
func TestAssembleAgent_WiresPartialComponents(t *testing.T) {
	assembly, err := AssembleAgent(
		context.Background(),
		newAssembleTestConfig(),
		"openai", "test-model",
		io.Discard,
	)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	// D5: FileTracker must be created and exposed.
	require.NotNil(t, assembly.FileTracker, "FileTracker must be wired by AssembleAgent")

	// D6: DiffGenerator must be created and exposed.
	require.NotNil(t, assembly.DiffGenerator, "DiffGenerator must be wired by AssembleAgent")

	// 19-9: PlanModeController must be created and exposed.
	require.NotNil(t, assembly.PlanCtrl, "PlanCtrl must be wired by AssembleAgent")
}

// TestAssembleAgent_WiresPermissionModeResolver verifies that AssembleAgent
// creates a PermissionModeResolver and wires it into the ApprovalMiddleware
// (task 19-11). The resolver must be non-nil and identify itself as the
// default "permission_mode" resolver so the middleware can switch policy
// dynamically based on the current PermissionMode.
func TestAssembleAgent_WiresPermissionModeResolver(t *testing.T) {
	assembly, err := AssembleAgent(
		context.Background(),
		newAssembleTestConfig(),
		"openai", "test-model",
		io.Discard,
	)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	require.NotNil(t, assembly.ModeResolver, "ModeResolver must be wired by AssembleAgent")
	assert.Equal(t, "permission_mode", assembly.ModeResolver.Name(),
		"resolver must be the default permission_mode resolver")
}

// getWebSearchTool retrieves the original (unwrapped) *tools.WebSearchTool from
// the assembled tool registry. List returns the underlying definitions without
// middleware wrapping, so the concrete type is accessible.
func getWebSearchTool(t *testing.T, assembly *AgentAssembly) *tools.WebSearchTool {
	t.Helper()
	defs, err := assembly.ToolRegistry.List(context.Background())
	require.NoError(t, err)
	for _, d := range defs {
		if d.Name() == "web_search" {
			ws, ok := d.(*tools.WebSearchTool)
			require.True(t, ok, "web_search tool should be *tools.WebSearchTool")
			return ws
		}
	}
	t.Fatal("web_search tool not found in registry")
	return nil
}

// TestAssembleAgent_WebSearchDefaultUsesMockProvider verifies that when no
// web_search config is provided, the WebSearchTool defaults to the
// MockSearchProvider (task 19-7).
func TestAssembleAgent_WebSearchDefaultUsesMockProvider(t *testing.T) {
	assembly, err := AssembleAgent(
		context.Background(),
		newAssembleTestConfig(),
		"openai", "test-model",
		io.Discard,
	)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	ws := getWebSearchTool(t, assembly)
	assert.Equal(t, "mock", ws.ProviderName(),
		"default config should use MockSearchProvider")
}

// TestAssembleAgent_WebSearchFetchProvider verifies that when
// web_search.provider = "fetch", the WebSearchTool uses the
// FetchSearchProvider (task 19-7).
func TestAssembleAgent_WebSearchFetchProvider(t *testing.T) {
	cfg := newAssembleTestConfig()
	cfg.WebSearch.Provider = "fetch"
	assembly, err := AssembleAgent(
		context.Background(),
		cfg,
		"openai", "test-model",
		io.Discard,
	)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	ws := getWebSearchTool(t, assembly)
	assert.Equal(t, "fetch", ws.ProviderName(),
		"provider=fetch should use FetchSearchProvider")
}

// TestAssembleAgent_WebSearchBraveProvider verifies that when
// web_search.provider = "brave" with an API key, the WebSearchTool uses the
// BraveSearchProvider (task 19-7).
func TestAssembleAgent_WebSearchBraveProvider(t *testing.T) {
	cfg := newAssembleTestConfig()
	cfg.WebSearch.Provider = "brave"
	cfg.WebSearch.APIKey = "test-key"
	assembly, err := AssembleAgent(
		context.Background(),
		cfg,
		"openai", "test-model",
		io.Discard,
	)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	ws := getWebSearchTool(t, assembly)
	assert.Equal(t, "brave", ws.ProviderName(),
		"provider=brave with API key should use BraveSearchProvider")
}

// TestAssembleAgent_WebSearchBraveWithoutAPIKeyFallsBackToMock verifies that
// when web_search.provider = "brave" but no API key is supplied, the tool
// falls back to the MockSearchProvider rather than a broken Brave provider.
func TestAssembleAgent_WebSearchBraveWithoutAPIKeyFallsBackToMock(t *testing.T) {
	cfg := newAssembleTestConfig()
	cfg.WebSearch.Provider = "brave"
	// No API key set.
	assembly, err := AssembleAgent(
		context.Background(),
		cfg,
		"openai", "test-model",
		io.Discard,
	)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	ws := getWebSearchTool(t, assembly)
	assert.Equal(t, "mock", ws.ProviderName(),
		"brave without API key should fall back to MockSearchProvider")
}
