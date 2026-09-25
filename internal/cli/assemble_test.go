package cli //exempt:scan003

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAssembleTestConfig returns a minimal Config whose provider section forces
// buildModel down the OpenAI-compatible path with an explicitly scoped key.
// EinoProvider.Build only constructs the HTTPChatModel object (no network
// calls), so assembly succeeds without a live endpoint.
func newAssembleTestConfig() *config.Config {
	return &config.Config{
		Provider: config.ProviderConfig{
			Name:    "openai",
			BaseURL: "http://127.0.0.1:1",
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

type assembleRoundTripFunc func(*http.Request) (*http.Response, error)

func (f assembleRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func withAssembleDefaultTransport(t *testing.T, transport http.RoundTripper) {
	t.Helper()
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous })
}

func searchResponse(req *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func getWebSearchTool(t *testing.T, assembly *AgentAssembly) tools.ToolDefinition {
	t.Helper()
	tool, err := assembly.ToolRegistry.Get(context.Background(), "web_search")
	require.NoError(t, err)
	return tool
}

func executeAssembledWebSearch(t *testing.T, cfg *config.Config) *tools.ToolResult {
	t.Helper()
	ctx := context.Background()
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	tool := getWebSearchTool(t, assembly)
	result, err := tool.Execute(ctx, tools.ToolCall{Name: "web_search", Args: map[string]any{"query": "provider test"}})
	require.NoError(t, err)
	require.NotNil(t, result)
	return result
}

// TestAssembleAgent_WebSearchDefaultUsesMockProvider verifies that when no
// web_search config is provided, the WebSearchTool defaults to the
// MockSearchProvider (task 19-7).
func TestAssembleAgent_WebSearchDefaultUsesMockProvider(t *testing.T) {
	var requests int
	withAssembleDefaultTransport(t, assembleRoundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, assert.AnError
	}))
	result := executeAssembledWebSearch(t, newAssembleTestConfig())
	assert.Equal(t, true, result.Metadata["mock"], "default config should use MockSearchProvider")
	assert.Zero(t, requests, "the mock provider must not send a network request")
}

// TestAssembleAgent_WebSearchFetchProvider verifies that when
// web_search.provider = "fetch", the WebSearchTool uses the
// FetchSearchProvider (task 19-7).
func TestAssembleAgent_WebSearchFetchProvider(t *testing.T) {
	cfg := newAssembleTestConfig()
	cfg.WebSearch.Provider = "fetch"
	var request *http.Request
	withAssembleDefaultTransport(t, assembleRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		request = req
		return searchResponse(req, `<div class="result"><a class="result__a" href="https://example.org">Result</a><a class="result__snippet">Snippet</a></div>`), nil
	}))
	result := executeAssembledWebSearch(t, cfg)
	assert.Equal(t, false, result.Metadata["mock"], "provider=fetch should use FetchSearchProvider")
	require.NotNil(t, request, "fetch provider should perform one localmocked request")
	assert.Equal(t, "html.duckduckgo.com", request.URL.Host)
}

// TestAssembleAgent_WebSearchBraveProvider verifies that when
// web_search.provider = "brave" with an API key, the WebSearchTool uses the
// BraveSearchProvider (task 19-7).
func TestAssembleAgent_WebSearchBraveProvider(t *testing.T) {
	cfg := newAssembleTestConfig()
	cfg.WebSearch.Provider = "brave"
	apiKey := t.TempDir()
	cfg.WebSearch.APIKey = apiKey
	var request *http.Request
	withAssembleDefaultTransport(t, assembleRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		request = req
		return searchResponse(req, `{"web":{"results":[]}}`), nil
	}))
	result := executeAssembledWebSearch(t, cfg)
	assert.Equal(t, false, result.Metadata["mock"], "provider=brave with API key should use BraveSearchProvider")
	require.NotNil(t, request, "Brave provider should perform one localmocked request")
	assert.Equal(t, "api.search.brave.com", request.URL.Host)
	assert.Equal(t, "Bearer "+apiKey, request.Header.Get("X-Subscription-Token"))
}

// TestAssembleAgent_WebSearchBraveWithoutAPIKeyFallsBackToMock verifies that
// when web_search.provider = "brave" but no API key is supplied, the tool
// falls back to the MockSearchProvider rather than a broken Brave provider.
func TestAssembleAgent_WebSearchBraveWithoutAPIKeyFallsBackToMock(t *testing.T) {
	cfg := newAssembleTestConfig()
	cfg.WebSearch.Provider = "brave"
	// No API key set.
	var requests int
	withAssembleDefaultTransport(t, assembleRoundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, assert.AnError
	}))
	result := executeAssembledWebSearch(t, cfg)
	assert.Equal(t, true, result.Metadata["mock"], "brave without API key should fall back to MockSearchProvider")
	assert.Zero(t, requests, "missing Brave credentials must not issue a network request")
}
