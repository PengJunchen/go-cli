package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderRegistryRejectsNativeIdentityBeforeBuild(t *testing.T) {
	provider := &countingModelProvider{name: "claude"}
	registry := NewProviderRegistry()
	require.NoError(t, registry.Register(provider))
	credentials := &countingCredentialResolver{key: "scoped-secret-canary"}
	registry.SetCredentialResolver(credentials)

	_, cleanup, err := registry.GetModel(context.Background(), "claude", ModelConfig{
		Model:   "claude-test",
		APIKey:  "anthropic-secret-canary",
		BaseURL: "https://example.invalid/v1",
	})

	require.Error(t, err)
	assert.Nil(t, cleanup)
	assert.Zero(t, provider.buildCalls, "native identity must be rejected before provider construction")
	assert.Zero(t, credentials.lookupCalls, "native identity must be rejected before secret lookup")
	assert.NotContains(t, err.Error(), "anthropic-secret-canary")
	assert.True(t, strings.Contains(strings.ToLower(err.Error()), "unsupported"), "error should explain that the protocol is unsupported")
}

func TestProviderRegistryUsesMatchingScopedCredentialAtRealOrigin(t *testing.T) {
	const secret = "scoped-origin-secret-canary"
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
	const secret = "same-origin-provider-key"
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
	const secret = "redirect-source-secret-canary"
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

func TestProviderRegistryUnknownAliasNeedsExplicitEndpoint(t *testing.T) {
	credentials := &countingCredentialResolver{key: "provider-only-secret-canary"}
	registry := NewProviderRegistry()
	registry.SetCredentialResolver(credentials)
	require.NoError(t, registry.Register(NewEinoProvider(WithProviderName("unknown-alias"))))

	_, _, err := registry.GetModel(context.Background(), "unknown-alias", ModelConfig{Model: "m", APIKey: "request-secret-canary"})
	require.ErrorIs(t, err, ErrProviderEndpointRequired)
	assert.Zero(t, credentials.lookupCalls)
}

func TestProviderRegistryPreservesExplicitOpenAICompatibleCustomEndpoint(t *testing.T) {
	const secret = "custom-origin-secret-canary"
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
	_, _, err := registry.GetModel(context.Background(), "local-compatible", ModelConfig{
		Model:      "m",
		BaseURL:    "http://127.0.0.1:8080/v1",
		APIKey:     "must-not-be-rebound",
		Credential: CredentialRef{ProviderID: "eino", ProtocolID: ProtocolOpenAICompatible, Origin: "http://127.0.0.1:8080"},
	})
	require.ErrorIs(t, err, ErrCredentialScopeMismatch)
	assert.NotContains(t, err.Error(), "must-not-be-rebound")
}

func TestProviderRegistryDoesNotSendProviderDefaultKeyToOverrideOrigin(t *testing.T) {
	const secret = "default-origin-secret-canary"
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
