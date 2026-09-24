package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ProtocolID identifies the wire protocol used by a provider adapter.
type ProtocolID string

// resolverProviderSpec is implemented only by in-package adapters whose
// protocol contract is known to this resolver. External ModelProvider values
// cannot opt into credential routing by merely naming a protocol themselves.
type resolverProviderSpec interface {
	resolverSpec() ProviderSpec
}

const (
	// ProtocolOpenAICompatible is the currently enabled request protocol.
	ProtocolOpenAICompatible  ProtocolID = "openai-compatible"
	protocolAnthropicMessages ProtocolID = "anthropic-messages"
	protocolGeminiGenerate    ProtocolID = "gemini-generate-content"
)

var (
	ErrUnsupportedProviderProtocol = errors.New("llm: provider protocol is unsupported")
	ErrMissingProviderSpec         = errors.New("llm: provider has no registered protocol spec")
	ErrCredentialScopeMismatch     = errors.New("llm: credential scope does not match resolved provider origin")
	ErrProviderEndpointRequired    = errors.New("llm: provider requires an explicitly configured compatible endpoint")
	ErrProviderCredentialRequired  = errors.New("llm: provider requires a scoped credential")
)

// ProviderSpec is the immutable protocol and endpoint description used for
// request resolution. Only explicitly registered OpenAI-compatible specs are
// constructible in this checkpoint; native Claude/Gemini remain disabled.
type ProviderSpec struct {
	ProviderID              string
	Aliases                 []string
	ProtocolID              ProtocolID
	DefaultBaseURL          string
	DefaultModel            string
	DefaultEndpointExplicit bool
	RequireCredential       bool
	Enabled                 bool
}

// ModelSpec carries provider-scoped model metadata. Unknown model metadata is
// represented by Known=false; arbitrary model names remain valid requests for
// compatible endpoints and do not enable protocol capabilities.
type ModelSpec struct {
	ProviderID string
	ModelID    string
	Info       ModelInfo
	Known      bool
}

// CredentialRef binds a credential lookup to a canonical provider, protocol,
// and normalized origin. It never contains secret material.
type CredentialRef struct {
	ProviderID string
	ProtocolID ProtocolID
	Origin     string
}

// ModelRequest describes the complete identity needed to resolve one model.
// API keys are deliberately absent; callers may provide one through the
// legacy registry boundary, where it is immediately bound to this scope.
type ModelRequest struct {
	ProviderID  string
	ModelID     string
	Endpoint    string
	Credential  CredentialRef
	Temperature float64
	MaxTokens   int
}

// ScopedCredentialResolver must prove an exact provider/protocol/origin
// match. A source that only accepts provider names cannot satisfy this
// interface and must not be used by model resolution.
type ScopedCredentialResolver interface {
	ResolveCredential(ctx context.Context, providerID, protocolID, origin string) (secret string, found bool, err error)
}

type providerDefinition struct {
	spec     ProviderSpec
	provider ModelProvider
}

// ModelResolver holds one immutable provider definition snapshot and resolves
// requests against it. Registry mutations publish a new snapshot; existing
// resolver users keep their original authority.
type ModelResolver struct {
	providers   map[string]providerDefinition
	aliases     map[string]string
	credentials ScopedCredentialResolver
}

// ResolvedModel contains the validated provider/model/protocol/origin
// snapshot. Its build state, including any credential, is private so callers
// cannot accidentally log or re-scope raw secret material.
type ResolvedModel struct {
	ProviderID string
	ModelID    string
	ProtocolID ProtocolID
	Origin     string
	Provider   ProviderSpec
	Model      ModelSpec
	provider   ModelProvider
	cfg        ModelConfig
}

// Build constructs the adapter using the already resolved scope and secret.
func (m ResolvedModel) Build(ctx context.Context) (BaseChatModel, func(), error) {
	return m.provider.Build(ctx, m.cfg)
}

// SnapshotResolver returns the registry's current immutable resolution
// snapshot. Later registrations publish a different resolver and do not mutate
// this one.
func (r *ProviderRegistry) SnapshotResolver() *ModelResolver {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.resolver
}

// SetCredentialResolver installs an exact-scope source for subsequent
// resolver snapshots. Existing snapshots are unaffected.
func (r *ProviderRegistry) SetCredentialResolver(source ScopedCredentialResolver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.credentials = source
	r.publishResolverLocked()
}

// ResolveModel resolves a request against this immutable snapshot. A supplied
// CredentialRef must match the resolver's canonical provider, protocol, and
// origin exactly. A zero CredentialRef is filled from the request identity.
func (r *ModelResolver) ResolveModel(ctx context.Context, request ModelRequest) (ResolvedModel, error) {
	return r.resolveModel(ctx, request, "")
}

// CredentialRefFor returns the exact scope a later request will resolve to.
// It applies the same identity and endpoint checks as ResolveModel but does
// not read secrets or construct an adapter.
func (r *ModelResolver) CredentialRefFor(providerID, endpoint string) (CredentialRef, error) {
	providerID = strings.ToLower(strings.TrimSpace(providerID))
	definition, ok := r.lookupProvider(providerID)
	if !ok {
		return CredentialRef{}, fmt.Errorf("%w: %s", errProviderNotFound, providerID)
	}
	if isDisabledNativeIdentity(providerID) || isNativeProvider(definition.provider) || !definition.spec.Enabled || definition.spec.ProtocolID != ProtocolOpenAICompatible {
		return CredentialRef{}, fmt.Errorf("%w: %s", ErrUnsupportedProviderProtocol, providerID)
	}
	baseURL := strings.TrimSpace(endpoint)
	if baseURL == "" {
		baseURL = definition.spec.DefaultBaseURL
	}
	if baseURL == "" || (providerNeedsExplicitEndpoint(definition.spec) && strings.TrimSpace(endpoint) == "" && !definition.spec.DefaultEndpointExplicit) {
		return CredentialRef{}, fmt.Errorf("%w: %s", ErrProviderEndpointRequired, providerID)
	}
	origin, err := normalizeProviderOrigin(baseURL)
	if err != nil {
		return CredentialRef{}, fmt.Errorf("llm: invalid provider endpoint for %s: %w", definition.spec.ProviderID, err)
	}
	return CredentialRef{ProviderID: definition.spec.ProviderID, ProtocolID: definition.spec.ProtocolID, Origin: origin}, nil
}

func (r *ModelResolver) resolveModel(ctx context.Context, request ModelRequest, inlineKey string) (ResolvedModel, error) {
	providerID := strings.ToLower(strings.TrimSpace(request.ProviderID))
	if providerID == "" {
		return ResolvedModel{}, fmt.Errorf("llm: provider identity is required")
	}
	definition, ok := r.lookupProvider(providerID)
	if !ok {
		return ResolvedModel{}, fmt.Errorf("%w: %s", errProviderNotFound, providerID)
	}
	// Native identity and native adapter checks precede every secret source and
	// provider construction, even if a config supplies a compatible-looking URL.
	if isDisabledNativeIdentity(providerID) || isNativeProvider(definition.provider) {
		return ResolvedModel{}, fmt.Errorf("%w: %s", ErrUnsupportedProviderProtocol, providerID)
	}
	spec := cloneProviderSpec(definition.spec)
	if !spec.Enabled || spec.ProtocolID != ProtocolOpenAICompatible {
		return ResolvedModel{}, fmt.Errorf("%w: %s (%s)", ErrUnsupportedProviderProtocol, spec.ProviderID, spec.ProtocolID)
	}

	baseURL := strings.TrimSpace(request.Endpoint)
	if baseURL == "" {
		baseURL = spec.DefaultBaseURL
	}
	if baseURL == "" {
		return ResolvedModel{}, fmt.Errorf("%w: %s", ErrProviderEndpointRequired, spec.ProviderID)
	}
	if providerNeedsExplicitEndpoint(spec) && strings.TrimSpace(request.Endpoint) == "" && !spec.DefaultEndpointExplicit {
		return ResolvedModel{}, fmt.Errorf("%w: %s", ErrProviderEndpointRequired, spec.ProviderID)
	}
	origin, err := normalizeProviderOrigin(baseURL)
	if err != nil {
		return ResolvedModel{}, fmt.Errorf("llm: invalid provider endpoint for %s: %w", spec.ProviderID, err)
	}
	scope := CredentialRef{ProviderID: spec.ProviderID, ProtocolID: spec.ProtocolID, Origin: origin}
	if inlineKey != "" && request.Credential == (CredentialRef{}) {
		return ResolvedModel{}, fmt.Errorf("%w: inline provider credential requires an explicit reference", ErrCredentialScopeMismatch)
	}
	if request.Credential != (CredentialRef{}) {
		requested, scopeErr := normalizeCredentialRef(request.Credential)
		if scopeErr != nil || requested != scope {
			return ResolvedModel{}, fmt.Errorf("%w: %s", ErrCredentialScopeMismatch, spec.ProviderID)
		}
	}

	apiKey := inlineKey
	if apiKey == "" && r.credentials != nil {
		var found bool
		apiKey, found, err = r.credentials.ResolveCredential(ctx, scope.ProviderID, string(scope.ProtocolID), scope.Origin)
		if err != nil {
			return ResolvedModel{}, fmt.Errorf("llm: resolve scoped credential for %s: %w", spec.ProviderID, err)
		}
		if !found {
			apiKey = ""
		}
	}
	if apiKey == "" {
		apiKey = defaultCredentialForScope(definition.provider, origin)
	}
	if requiresOpenAIDefaultKey(spec.ProviderID, baseURL) && apiKey == "" {
		return ResolvedModel{}, fmt.Errorf("%w: %s (unscoped legacy credentials are not used)", ErrProviderCredentialRequired, spec.ProviderID)
	}

	modelID := strings.TrimSpace(request.ModelID)
	if modelID == "" {
		modelID = spec.DefaultModel
	}
	if modelID == "" {
		return ResolvedModel{}, fmt.Errorf("llm: model identity is required for provider %s", spec.ProviderID)
	}
	modelSpec := resolveModelSpec(definition.provider, spec.ProviderID, modelID)
	cfg := ModelConfig{
		Model:              modelID,
		Temperature:        request.Temperature,
		MaxTokens:          request.MaxTokens,
		APIKey:             apiKey,
		BaseURL:            baseURL,
		Credential:         scope,
		credentialResolved: true,
	}
	return ResolvedModel{
		ProviderID: spec.ProviderID,
		ModelID:    modelID,
		ProtocolID: spec.ProtocolID,
		Origin:     origin,
		Provider:   spec,
		Model:      modelSpec,
		provider:   definition.provider,
		cfg:        cfg,
	}, nil
}

func (r *ModelResolver) lookupProvider(id string) (providerDefinition, bool) {
	if canonical, ok := r.aliases[id]; ok {
		id = canonical
	}
	definition, ok := r.providers[id]
	return definition, ok
}

func cloneProviderSpec(spec ProviderSpec) ProviderSpec {
	spec.Aliases = append([]string(nil), spec.Aliases...)
	return spec
}

func resolveModelSpec(provider ModelProvider, providerID, modelID string) ModelSpec {
	result := ModelSpec{ProviderID: providerID, ModelID: modelID}
	for _, info := range provider.Models() {
		if info.Name == modelID {
			result.Info = info
			result.Known = true
			break
		}
	}
	return result
}

func providerNeedsExplicitEndpoint(spec ProviderSpec) bool {
	return spec.ProviderID != defaultProviderName && spec.ProviderID != openaiProviderName
}

func isDisabledNativeIdentity(id string) bool {
	switch strings.ToLower(id) {
	case "claude", "anthropic", "gemini", "google-gemini":
		return true
	default:
		return false
	}
}

func isNativeProvider(provider ModelProvider) bool {
	switch provider.(type) {
	case *ClaudeProvider, *GeminiProvider:
		return true
	default:
		return false
	}
}

func inferProviderSpec(provider ModelProvider) (ProviderSpec, bool) {
	name := strings.ToLower(strings.TrimSpace(provider.Name()))
	if isDisabledNativeIdentity(name) || isNativeProvider(provider) {
		return ProviderSpec{ProviderID: name, ProtocolID: protocolForNative(name), Enabled: false}, true
	}
	switch p := provider.(type) {
	case *EinoProvider:
		return ProviderSpec{
			ProviderID:              name,
			ProtocolID:              ProtocolOpenAICompatible,
			DefaultBaseURL:          p.defaultBaseURL,
			DefaultModel:            p.defaultModel,
			DefaultEndpointExplicit: p.baseURLConfigured,
			RequireCredential:       requiresOpenAIDefaultKey(name, p.defaultBaseURL),
			Enabled:                 true,
		}, true
	case *OpenAIProvider:
		return ProviderSpec{
			ProviderID:              name,
			ProtocolID:              ProtocolOpenAICompatible,
			DefaultBaseURL:          p.defaultBaseURL,
			DefaultModel:            p.defaultModel,
			DefaultEndpointExplicit: p.baseURLConfigured,
			RequireCredential:       requiresOpenAIDefaultKey(name, p.defaultBaseURL),
			Enabled:                 true,
		}, true
	default:
		if described, ok := provider.(resolverProviderSpec); ok {
			return described.resolverSpec(), true
		}
		return ProviderSpec{}, false
	}
}

func requiresOpenAIDefaultKey(providerID, baseURL string) bool {
	if providerID != defaultProviderName && providerID != openaiProviderName {
		return false
	}
	origin, err := normalizeProviderOrigin(baseURL)
	if err != nil {
		return false
	}
	defaultOrigin, err := normalizeProviderOrigin(defaultProviderBaseURL)
	return err == nil && origin == defaultOrigin
}

func protocolForNative(id string) ProtocolID {
	switch id {
	case "claude", "anthropic":
		return protocolAnthropicMessages
	case "gemini", "google-gemini":
		return protocolGeminiGenerate
	default:
		return "unsupported"
	}
}

func defaultCredentialForScope(provider ModelProvider, resolvedOrigin string) string {
	scoped, ok := provider.(interface{ credentialForOrigin(string) (string, bool) })
	if !ok {
		return ""
	}
	key, matches := scoped.credentialForOrigin(resolvedOrigin)
	if !matches {
		return ""
	}
	return key
}

func normalizeCredentialRef(ref CredentialRef) (CredentialRef, error) {
	providerID := strings.ToLower(strings.TrimSpace(ref.ProviderID))
	protocolID := ProtocolID(strings.ToLower(strings.TrimSpace(string(ref.ProtocolID))))
	origin, err := normalizeProviderOrigin(ref.Origin)
	if err != nil || providerID == "" || protocolID == "" {
		return CredentialRef{}, ErrCredentialScopeMismatch
	}
	return CredentialRef{ProviderID: providerID, ProtocolID: protocolID, Origin: origin}, nil
}

func normalizeProviderOrigin(baseURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Scheme == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("endpoint must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("endpoint scheme must be http or https")
	}
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	} else if value, err := strconv.Atoi(port); err != nil || value < 1 || value > 65535 {
		return "", errors.New("endpoint port must be between 1 and 65535")
	} else {
		port = strconv.Itoa(value)
	}
	return scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port), nil
}

func httpClientForResolvedScope(client *http.Client, cfg ModelConfig) (*http.Client, error) {
	if !cfg.credentialResolved {
		return client, nil
	}
	ref, err := normalizeCredentialRef(cfg.Credential)
	if err != nil {
		return nil, err
	}
	origin, err := normalizeProviderOrigin(cfg.BaseURL)
	if err != nil || origin != ref.Origin {
		return nil, ErrCredentialScopeMismatch
	}
	if client == nil {
		client = http.DefaultClient
	}
	clone := *client
	previousCheck := client.CheckRedirect
	clone.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		redirectOrigin, originErr := normalizeProviderOrigin(request.URL.String())
		if originErr != nil || redirectOrigin != ref.Origin {
			// Return the 3xx response to the caller instead of dispatching a
			// scoped credential to a different origin.
			return http.ErrUseLastResponse
		}
		if previousCheck != nil {
			return previousCheck(request, via)
		}
		if len(via) >= 10 {
			return errors.New("llm: stopped after 10 same-origin redirects")
		}
		return nil
	}
	return &clone, nil
}
