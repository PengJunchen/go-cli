package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newModelResolverCanary(t *testing.T) string {
	t.Helper()
	tempDir := t.TempDir()
	return "runtime-canary-" + filepath.Base(filepath.Dir(tempDir)) + "-" + filepath.Base(tempDir)
}

func TestProviderRegistryRejectsNativeIdentityBeforeBuild(t *testing.T) {
	provider := &countingModelProvider{name: "claude"}
	registry := NewProviderRegistry()
	require.NoError(t, registry.Register(provider))
	credentialCanary := newModelResolverCanary(t)
	credentials := &countingCredentialResolver{key: credentialCanary}
	registry.SetCredentialResolver(credentials)
	inlineCanary := newModelResolverCanary(t)

	_, cleanup, err := registry.GetModel(context.Background(), "claude", ModelConfig{
		Model:   "claude-test",
		APIKey:  inlineCanary,
		BaseURL: "https://example.invalid/v1",
	})

	require.Error(t, err)
	assert.Nil(t, cleanup)
	assert.Zero(t, provider.buildCalls, "native identity must be rejected before provider construction")
	assert.Zero(t, credentials.lookupCalls, "native identity must be rejected before secret lookup")
	assert.NotContains(t, err.Error(), inlineCanary)
	assert.True(t, strings.Contains(strings.ToLower(err.Error()), "unsupported"), "error should explain that the protocol is unsupported")
}

func TestModelResolverPreservesSamplingPresence(t *testing.T) {
	registry := NewProviderRegistry()
	require.NoError(t, registry.Register(NewEinoProvider(WithProviderName("sampling-provider"))))
	registry.SetCredentialResolver(&countingCredentialResolver{key: newModelResolverCanary(t)})
	resolver := registry.SnapshotResolver()
	endpoint := "https://sampling.example.test/v1"
	ref, err := resolver.CredentialRefFor("sampling-provider", endpoint)
	require.NoError(t, err)

	resolved, err := resolver.ResolveModel(context.Background(), ModelRequest{
		ProviderID:     "sampling-provider",
		ModelID:        "sampling-model",
		Endpoint:       endpoint,
		Credential:     ref,
		Temperature:    0,
		TemperatureSet: true,
		MaxTokens:      0,
		MaxTokensSet:   true,
	})
	require.NoError(t, err)
	assert.True(t, resolved.cfg.TemperatureSet)
	assert.Zero(t, resolved.cfg.Temperature)
	assert.True(t, resolved.cfg.MaxTokensSet)
	assert.Zero(t, resolved.cfg.MaxTokens)

	legacy, err := resolver.ResolveModel(context.Background(), ModelRequest{
		ProviderID:  "sampling-provider",
		ModelID:     "legacy-sampling-model",
		Endpoint:    endpoint,
		Credential:  ref,
		Temperature: 0.25,
		MaxTokens:   128,
	})
	require.NoError(t, err)
	assert.True(t, legacy.cfg.TemperatureSet, "legacy non-zero scalar values remain explicit")
	assert.True(t, legacy.cfg.MaxTokensSet, "legacy non-zero scalar values remain explicit")
}

func TestProviderRegistryUsesMatchingScopedCredentialAtRealOrigin(t *testing.T) {
	secret := newModelResolverCanary(t)
	var gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer server.Close()

	registry := NewProviderRegistry()
	registry.SetCredentialResolver(&countingCredentialResolver{key: secret, wantProvider: "local-compatible", wantProtocol: string(ProtocolOpenAICompatible), wantOrigin: normalizedTestOrigin(t, server.URL)})
	require.NoError(t, registry.Register(NewEinoProvider(WithProviderName("local-compatible"))))

	model, cleanup, err := registry.GetModel(context.Background(), "local-compatible", ModelConfig{Model: "custom-model", BaseURL: server.URL})
	require.NoError(t, err)
	defer cleanup()
	_, err = model.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hello"}})
	require.NoError(t, err)
	assert.Equal(t, "Bearer "+secret, gotAuthorization)
}

func TestProviderRegistryScopeMismatchPrecedesCredentialLookup(t *testing.T) {
	credentials := &countingCredentialResolver{key: "never-return-this", wantProvider: "local-compatible", wantProtocol: string(ProtocolOpenAICompatible)}
	registry := NewProviderRegistry()
	registry.SetCredentialResolver(credentials)
	require.NoError(t, registry.Register(NewEinoProvider(WithProviderName("local-compatible"))))
	resolver := registry.SnapshotResolver()

	_, err := resolver.ResolveModel(context.Background(), ModelRequest{
		ProviderID: "local-compatible",
		ModelID:    "m",
		Endpoint:   "http://127.0.0.1:8081/v1",
		Credential: CredentialRef{
			ProviderID: "local-compatible",
			ProtocolID: ProtocolOpenAICompatible,
			Origin:     "http://127.0.0.1:8080",
		},
	})
	require.ErrorIs(t, err, ErrCredentialScopeMismatch)
	assert.Zero(t, credentials.lookupCalls)
}

func TestProviderOriginNormalizesEffectivePort(t *testing.T) {
	withoutPort, err := normalizeProviderOrigin("https://api.example.test/v1")
	require.NoError(t, err)
	withDefaultPort, err := normalizeProviderOrigin("https://api.example.test:0443/other")
	require.NoError(t, err)
	assert.Equal(t, "https://api.example.test:443", withoutPort)
	assert.Equal(t, withoutPort, withDefaultPort)
}

func TestProviderRegistryRequiresCredentialForOpenAIDefaultOrigin(t *testing.T) {
	registry := NewProviderRegistry()
	_, _, err := registry.GetModel(context.Background(), "eino", ModelConfig{Model: "gpt-test"})
	require.ErrorIs(t, err, ErrProviderCredentialRequired)
	assert.Contains(t, err.Error(), "unscoped legacy credentials are not used")
}

func TestProviderRegistryAllowsConstructorCredentialOnlyOnSameOrigin(t *testing.T) {
	secret := newModelResolverCanary(t)
	var gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer server.Close()

	provider := NewEinoProvider(
		WithProviderName("local-compatible"),
		WithBaseURL(server.URL+"/configured-path"),
		WithAPIKey(secret),
	)
	registry := NewProviderRegistry()
	require.NoError(t, registry.Register(provider))
	model, cleanup, err := registry.GetModel(context.Background(), "local-compatible", ModelConfig{
		Model:   "m",
		BaseURL: server.URL + "/request-path",
	})
	require.NoError(t, err)
	defer cleanup()
	_, err = model.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hello"}})
	require.NoError(t, err)
	assert.Equal(t, "Bearer "+secret, gotAuthorization)
}

func TestProviderRegistryRejectsCrossOriginRedirectBeforeSecondDispatch(t *testing.T) {
	secret := newModelResolverCanary(t)
	secondRequests := 0
	secondAuthorization := ""
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondRequests++
		secondAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"must-not-be-used"}}]}`)
	}))
	defer second.Close()

	firstAuthorization := ""
	firstRequests := 0
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstRequests++
		firstAuthorization = r.Header.Get("Authorization")
		http.Redirect(w, r, second.URL+"/v1/chat/completions", http.StatusTemporaryRedirect)
	}))
	defer first.Close()

	registry := NewProviderRegistry()
	require.NoError(t, registry.Register(NewEinoProvider(
		WithProviderName("local-compatible"),
		WithBaseURL(first.URL),
		WithAPIKey(secret),
	)))
	model, cleanup, err := registry.GetModel(context.Background(), "local-compatible", ModelConfig{Model: "m", BaseURL: first.URL})
	require.NoError(t, err)
	defer cleanup()
	_, err = model.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hello"}})
	require.Error(t, err, "a cross-origin redirect is not a successful provider response")
	assert.Equal(t, 1, firstRequests)
	assert.Equal(t, "Bearer "+secret, firstAuthorization)
	assert.Zero(t, secondRequests, "scoped credentials must not authorize a second-origin dispatch")
	assert.Empty(t, secondAuthorization)
}

func TestProviderRegistryRejectsCustomRedirectCallbackOriginRewrite(t *testing.T) {
	secret := newModelResolverCanary(t)
	secondRequests := 0
	secondAuthorization := ""
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondRequests++
		secondAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"must-not-be-used"}}]}`)
	}))
	defer second.Close()

	firstRequests := 0
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstRequests++
		http.Redirect(w, r, "/same-origin-redirect", http.StatusTemporaryRedirect)
	}))
	defer first.Close()

	secondEndpoint, err := url.Parse(second.URL + chatCompletionsPath)
	require.NoError(t, err)
	client := &http.Client{CheckRedirect: func(request *http.Request, _ []*http.Request) error {
		request.Header.Set("Authorization", "Bearer "+secret) //nolint:gosec // Deliberately reinjects the canary to test redirect-origin enforcement.
		request.URL = secondEndpoint
		return nil
	}}
	registry := NewProviderRegistry()
	require.NoError(t, registry.Register(NewEinoProvider(
		WithProviderName("local-compatible"),
		WithBaseURL(first.URL),
		WithAPIKey(secret),
		WithHTTPClient(client),
	)))
	model, cleanup, err := registry.GetModel(context.Background(), "local-compatible", ModelConfig{Model: "m", BaseURL: first.URL})
	require.NoError(t, err)
	defer cleanup()

	_, err = model.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hello"}})
	require.Error(t, err, "callback-rewritten cross-origin redirects are not valid provider responses")
	assert.Equal(t, 1, firstRequests)
	assert.Zero(t, secondRequests, "callback must not redirect a scoped credential to another origin")
	assert.Empty(t, secondAuthorization)
}

func TestProviderRegistryRejectsCustomRedirectCallbackHostRewrite(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Redirect(w, r, "/same-origin-redirect", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client := &http.Client{CheckRedirect: func(request *http.Request, _ []*http.Request) error {
		request.Host = "foreign.example.test"
		return nil
	}}
	registry := NewProviderRegistry()
	require.NoError(t, registry.Register(NewEinoProvider(
		WithProviderName("local-compatible"),
		WithBaseURL(server.URL),
		WithAPIKey(newModelResolverCanary(t)),
		WithHTTPClient(client),
	)))
	model, cleanup, err := registry.GetModel(context.Background(), "local-compatible", ModelConfig{Model: "m", BaseURL: server.URL})
	require.NoError(t, err)
	defer cleanup()

	_, err = model.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hello"}})
	require.Error(t, err, "a redirected HTTP authority must remain inside the credential scope")
	assert.Equal(t, 1, requests, "callback-mutated Host must be rejected before a second request")
}

func TestProviderRegistryRejectsCanonicalProviderNameCollision(t *testing.T) {
	registry := NewProviderRegistry()
	canonical, err := registry.Get("eino")
	require.NoError(t, err)

	err = registry.Register(NewEinoProvider(WithProviderName("EINO")))
	require.ErrorIs(t, err, errProviderAlreadyRegistered)
	upper, err := registry.Get("EINO")
	require.NoError(t, err)
	lower, err := registry.Get("eino")
	require.NoError(t, err)
	assert.Same(t, canonical, upper)
	assert.Same(t, canonical, lower)
	assert.Len(t, registry.List(), 1)

	empty := &ProviderRegistry{}
	err = empty.Register(NewEinoProvider(WithProviderName("EINO")))
	assert.ErrorIs(t, err, errProviderNameNotCanonical, "first registration must also use the canonical spelling")
}

func TestProviderRegistryRejectsAliasThatShadowsCanonicalName(t *testing.T) {
	registry := NewProviderRegistry()
	provider := &resolverSpecFixtureProvider{
		name: "local-compatible",
		spec: ProviderSpec{
			ProviderID:              "local-compatible",
			Aliases:                 []string{"EINO"},
			ProtocolID:              ProtocolOpenAICompatible,
			DefaultBaseURL:          "https://fixture.invalid/v1",
			DefaultEndpointExplicit: true,
			Enabled:                 true,
		},
	}
	require.Error(t, registry.Register(provider), "aliases must not shadow canonical provider IDs")
	_, err := registry.Get("local-compatible")
	assert.ErrorIs(t, err, errProviderNotFound)
}

func TestProviderRegistryRejectsDisabledNativeIdentityAlias(t *testing.T) {
	registry := NewProviderRegistry()
	provider := &resolverSpecFixtureProvider{
		name: "local-compatible",
		spec: ProviderSpec{
			ProviderID:              "local-compatible",
			Aliases:                 []string{"Anthropic"},
			ProtocolID:              ProtocolOpenAICompatible,
			DefaultBaseURL:          "https://fixture.invalid/v1",
			DefaultEndpointExplicit: true,
			Enabled:                 true,
		},
	}
	assert.Error(t, registry.Register(provider), "native identities stay reserved even when an alias would target compatible wire")
}

func TestProviderRegistryRejectsCanonicalNameThatShadowsAlias(t *testing.T) {
	registry := NewProviderRegistry()
	first := &resolverSpecFixtureProvider{
		name: "first-compatible",
		spec: ProviderSpec{
			ProviderID:              "first-compatible",
			Aliases:                 []string{"second-compatible"},
			ProtocolID:              ProtocolOpenAICompatible,
			DefaultBaseURL:          "https://fixture.invalid/v1",
			DefaultEndpointExplicit: true,
			Enabled:                 true,
		},
	}
	require.NoError(t, registry.Register(first))
	second := &resolverSpecFixtureProvider{
		name: "second-compatible",
		spec: ProviderSpec{
			ProviderID:              "second-compatible",
			ProtocolID:              ProtocolOpenAICompatible,
			DefaultBaseURL:          "https://fixture.invalid/v1",
			DefaultEndpointExplicit: true,
			Enabled:                 true,
		},
	}
	require.Error(t, registry.Register(second), "canonical names must not shadow an existing alias")
	got, err := registry.Get("SECOND-COMPATIBLE")
	require.NoError(t, err)
	assert.Same(t, first, got)
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), ModelRequest{ProviderID: "SECOND-COMPATIBLE", ModelID: "alias-model"})
	require.NoError(t, err)
	assert.Equal(t, "first-compatible", resolved.ProviderID)
}

func TestProviderRegistryGetModelInfoUsesCanonicalIDForExternalLookup(t *testing.T) {
	provider := &resolverSpecFixtureProvider{
		name: "first-compatible",
		spec: ProviderSpec{
			ProviderID:              "first-compatible",
			Aliases:                 []string{"second-compatible"},
			ProtocolID:              ProtocolOpenAICompatible,
			DefaultBaseURL:          "https://fixture.invalid/v1",
			DefaultEndpointExplicit: true,
			Enabled:                 true,
		},
	}
	registry := NewProviderRegistry()
	require.NoError(t, registry.Register(provider))
	external := &recordingModelRegistry{info: ModelInfo{Name: "remote-model", ContextWindow: 1234}, found: true}
	registry.modelRegistry = external

	for _, providerID := range []string{"FIRST-COMPATIBLE", "SECOND-COMPATIBLE"} {
		info := registry.GetModelInfo(context.Background(), providerID, "remote-model")
		assert.Equal(t, "remote-model", info.Name)
	}
	assert.Equal(t, []string{"first-compatible", "first-compatible"}, external.providers,
		"canonical names and aliases must query the same external metadata key")

	external.found = false
	missing := registry.GetModelInfo(context.Background(), "SECOND-COMPATIBLE", "unknown-model")
	assert.Equal(t, ModelInfo{}, missing, "missing metadata must remain unknown")
	assert.Equal(t, "first-compatible", external.providers[len(external.providers)-1])
}

func TestWithModelsCopiesCallerSlice(t *testing.T) {
	models := []ModelInfo{{Name: "original", ContextWindow: 100}}
	provider := NewEinoProvider(WithModels(models))
	models[0].Name = "mutated"

	assert.Equal(t, "original", provider.Models()[0].Name)
}

func TestWithNativeModelsCopiesCallerSlice(t *testing.T) {
	models := []ModelInfo{{Name: "original", ContextWindow: 100}}
	provider := NewOpenAIProvider(WithNativeModels(models))
	models[0].Name = "mutated"

	assert.Equal(t, "original", provider.Models()[0].Name)
}

func TestModelResolverSnapshotsProviderModelMetadata(t *testing.T) {
	provider := &resolverSpecFixtureProvider{
		name:   "local-compatible",
		models: []ModelInfo{{Name: "original", ContextWindow: 100}},
		spec: ProviderSpec{
			ProviderID:              "local-compatible",
			ProtocolID:              ProtocolOpenAICompatible,
			DefaultBaseURL:          "https://fixture.invalid/v1",
			DefaultEndpointExplicit: true,
			Enabled:                 true,
		},
	}
	registry := NewProviderRegistry()
	require.NoError(t, registry.Register(provider))
	resolver := registry.SnapshotResolver()
	provider.models[0].Name = "mutated"

	resolved, err := resolver.ResolveModel(context.Background(), ModelRequest{ProviderID: provider.name, ModelID: "original"})
	require.NoError(t, err)
	assert.True(t, resolved.Model.Known)
	assert.Equal(t, "original", resolved.Model.Info.Name)
}

func TestProviderSpecRequireCredentialAppliesToItsDefaultOrigin(t *testing.T) {
	provider := NewEinoProvider(WithProviderName("secured-compatible"), WithBaseURL("https://credentials.example.test/v1"))
	resolver := &ModelResolver{providers: map[string]providerDefinition{
		"secured-compatible": {
			provider: provider,
			spec: ProviderSpec{
				ProviderID:              "secured-compatible",
				ProtocolID:              ProtocolOpenAICompatible,
				DefaultBaseURL:          "https://credentials.example.test/v1",
				DefaultEndpointExplicit: true,
				RequireCredential:       true,
				Enabled:                 true,
			},
		},
	}}

	_, err := resolver.ResolveModel(context.Background(), ModelRequest{ProviderID: "secured-compatible", ModelID: "m"})
	assert.ErrorIs(t, err, ErrProviderCredentialRequired)
}

func TestProviderRegistryUnknownAliasNeedsExplicitEndpoint(t *testing.T) {
	credentials := &countingCredentialResolver{key: newModelResolverCanary(t)}
	registry := NewProviderRegistry()
	registry.SetCredentialResolver(credentials)
	require.NoError(t, registry.Register(NewEinoProvider(WithProviderName("unknown-alias"))))

	requestCanary := newModelResolverCanary(t)
	_, _, err := registry.GetModel(context.Background(), "unknown-alias", ModelConfig{Model: "m", APIKey: requestCanary})
	require.ErrorIs(t, err, ErrProviderEndpointRequired)
	assert.Zero(t, credentials.lookupCalls)
	assert.NotContains(t, err.Error(), requestCanary)
}

func TestProviderRegistryPreservesExplicitOpenAICompatibleCustomEndpoint(t *testing.T) {
	secret := newModelResolverCanary(t)
	var gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer server.Close()

	registry := NewProviderRegistry()
	require.NoError(t, registry.Register(NewEinoProvider(WithProviderName("local-compatible"))))
	model, cleanup, err := registry.GetModel(context.Background(), "local-compatible", ModelConfig{
		Model:      "custom-model",
		APIKey:     secret,
		BaseURL:    server.URL,
		Credential: CredentialRef{ProviderID: "local-compatible", ProtocolID: ProtocolOpenAICompatible, Origin: normalizedTestOrigin(t, server.URL)},
	})
	require.NoError(t, err)
	defer cleanup()

	message, err := model.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hello"}})
	require.NoError(t, err)
	assert.Equal(t, "ok", message.Content)
	assert.Equal(t, "Bearer "+secret, gotAuthorization)
}

func TestProviderRegistryAllowsExplicitCredentiallessCompatibleEndpoint(t *testing.T) {
	var gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer server.Close()

	registry := NewProviderRegistry()
	require.NoError(t, registry.Register(NewEinoProvider(WithProviderName("local-compatible"))))
	model, cleanup, err := registry.GetModel(context.Background(), "local-compatible", ModelConfig{Model: "m", BaseURL: server.URL})
	require.NoError(t, err)
	defer cleanup()

	message, err := model.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hello"}})
	require.NoError(t, err)
	assert.Equal(t, "ok", message.Content)
	assert.Empty(t, gotAuthorization, "explicit local compatible endpoints may use anonymous access")
}

func TestProviderRegistryRejectsInlineCredentialWithMismatchedScope(t *testing.T) {
	registry := NewProviderRegistry()
	require.NoError(t, registry.Register(NewEinoProvider(WithProviderName("local-compatible"))))
	inlineCanary := newModelResolverCanary(t)
	_, _, err := registry.GetModel(context.Background(), "local-compatible", ModelConfig{
		Model:      "m",
		BaseURL:    "http://127.0.0.1:8080/v1",
		APIKey:     inlineCanary,
		Credential: CredentialRef{ProviderID: "eino", ProtocolID: ProtocolOpenAICompatible, Origin: "http://127.0.0.1:8080"},
	})
	require.ErrorIs(t, err, ErrCredentialScopeMismatch)
	assert.NotContains(t, err.Error(), inlineCanary)
}

func TestProviderRegistryDoesNotSendProviderDefaultKeyToOverrideOrigin(t *testing.T) {
	secret := newModelResolverCanary(t)
	var gotAuthorization string
	override := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer override.Close()
	defaultOrigin := httptest.NewServer(http.NotFoundHandler())
	defer defaultOrigin.Close()

	provider := NewEinoProvider(
		WithProviderName("eino"),
		WithBaseURL(defaultOrigin.URL),
		WithAPIKey(secret),
	)
	registry := NewProviderRegistry()
	// Replace the default registry provider to model a configured provider whose
	// key belongs to its default origin.
	registry = &ProviderRegistry{providers: map[string]ModelProvider{}}
	require.NoError(t, registry.Register(provider))

	model, cleanup, err := registry.GetModel(context.Background(), "eino", ModelConfig{
		Model:   "gpt-test",
		BaseURL: override.URL,
	})
	require.NoError(t, err)
	defer cleanup()

	_, err = model.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hello"}})
	require.NoError(t, err)
	assert.Empty(t, gotAuthorization, "a provider default credential must not follow a different origin")
}

type countingModelProvider struct {
	name       string
	buildCalls int
}

type countingCredentialResolver struct {
	lookupCalls  int
	key          string
	wantProvider string
	wantProtocol string
	wantOrigin   string
}

type resolverSpecFixtureProvider struct {
	name   string
	spec   ProviderSpec
	models []ModelInfo
}

type recordingModelRegistry struct {
	providers []string
	info      ModelInfo
	found     bool
}

func (r *recordingModelRegistry) Lookup(_ context.Context, provider, _ string) (ModelInfo, bool) {
	r.providers = append(r.providers, provider)
	return r.info, r.found
}
func (*recordingModelRegistry) Providers() []ProviderMetadata        { return nil }
func (*recordingModelRegistry) ModelsForProvider(string) []ModelInfo { return nil }
func (*recordingModelRegistry) Refresh(context.Context) error        { return nil }

func (p *resolverSpecFixtureProvider) Name() string { return p.name }
func (p *resolverSpecFixtureProvider) Models() []ModelInfo {
	return p.models
}
func (p *resolverSpecFixtureProvider) resolverSpec() ProviderSpec { return p.spec }
func (p *resolverSpecFixtureProvider) Build(context.Context, ModelConfig) (BaseChatModel, func(), error) {
	return &stubModel{}, func() {}, nil
}

func (c *countingCredentialResolver) ResolveCredential(_ context.Context, provider, protocol, origin string) (string, bool, error) {
	c.lookupCalls++
	if (c.wantProvider != "" && provider != c.wantProvider) ||
		(c.wantProtocol != "" && protocol != c.wantProtocol) ||
		(c.wantOrigin != "" && origin != c.wantOrigin) {
		return "", false, nil
	}
	return c.key, c.key != "", nil
}

func normalizedTestOrigin(t *testing.T, endpoint string) string {
	t.Helper()
	origin, err := normalizeProviderOrigin(endpoint)
	require.NoError(t, err)
	return origin
}

func (p *countingModelProvider) Name() string { return p.name }

func (p *countingModelProvider) Build(context.Context, ModelConfig) (BaseChatModel, func(), error) {
	p.buildCalls++
	return &stubModel{}, func() {}, nil
}

func (p *countingModelProvider) Models() []ModelInfo { return nil }
