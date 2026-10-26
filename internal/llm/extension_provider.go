package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ProtocolExtensionRPC identifies an explicitly composed extension provider.
// It has no HTTP endpoint and never participates in host credential routing.
const ProtocolExtensionRPC ProtocolID = "extension-rpc"

// NewExtensionProvider gives a trusted application-composed provider a sealed
// resolver spec and dispatch adapter. Unlike ordinary Register, this explicit
// seam permits non-HTTP providers without permitting them to claim host keys.
// Provider construction is deferred until the operation owner grants dispatch.
func NewExtensionProvider(provider ModelProvider) (ModelProvider, error) {
	if provider == nil {
		return nil, errors.New("llm: extension provider is nil")
	}
	if wrapped, ok := provider.(*extensionProvider); ok {
		return wrapped, nil
	}
	name := canonicalProviderID(provider.Name())
	if name == "" || name != provider.Name() {
		return nil, errors.New("llm: extension provider requires a canonical name")
	}
	if name == defaultProviderName || name == openaiProviderName || isDisabledNativeIdentity(name) || isNativeProvider(provider) {
		return nil, fmt.Errorf("llm: extension provider cannot replace reserved identity %q", name)
	}
	models := append([]ModelInfo(nil), provider.Models()...)
	if len(models) == 0 {
		return nil, fmt.Errorf("llm: extension provider %q must declare its models", name)
	}
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if strings.TrimSpace(model.Name) == "" || strings.TrimSpace(model.Name) != model.Name {
			return nil, fmt.Errorf("llm: extension provider %q has an invalid model name", name)
		}
		if _, duplicate := seen[model.Name]; duplicate {
			return nil, fmt.Errorf("llm: extension provider %q repeats model %q", name, model.Name)
		}
		seen[model.Name] = struct{}{}
	}
	return &extensionProvider{inner: provider, name: name, models: models}, nil
}

// RegisterExtensionProvider registers a provider through the explicit,
// credential-free extension contract. Registration remains collision checked.
func (r *ProviderRegistry) RegisterExtensionProvider(provider ModelProvider) error {
	wrapped, err := NewExtensionProvider(provider)
	if err != nil {
		return err
	}
	return r.Register(wrapped)
}

type extensionProvider struct {
	inner  ModelProvider
	name   string
	models []ModelInfo
}

var (
	_ ModelProvider               = (*extensionProvider)(nil)
	_ guardedResolvedModelBuilder = (*extensionProvider)(nil)
	_ ProtocolEnvelopeEstimator   = (*extensionProvider)(nil)
	_ BaseChatModel               = (*extensionChatModel)(nil)
)

func (p *extensionProvider) Name() string        { return p.name }
func (p *extensionProvider) Models() []ModelInfo { return append([]ModelInfo(nil), p.models...) }
func (p *extensionProvider) resolverSpec() ProviderSpec {
	return ProviderSpec{ProviderID: p.name, ProtocolID: ProtocolExtensionRPC, DefaultModel: p.models[0].Name, Enabled: true}
}

// Build intentionally offers no unguarded shortcut for a registered extension.
func (p *extensionProvider) Build(context.Context, ModelConfig) (BaseChatModel, func(), error) {
	return nil, nil, ErrGuardedCandidateUnavailable
}

func (p *extensionProvider) buildGuarded(_ context.Context, cfg ModelConfig, candidate *resolvedCandidateHandle) (BaseChatModel, func(), error) {
	if cfg.APIKey != "" || cfg.BaseURL != "" || cfg.Credential != (CredentialRef{}) {
		return nil, nil, ErrCredentialScopeMismatch
	}
	return &extensionChatModel{provider: p, config: cfg, candidate: candidate}, func() {}, nil
}

func (p *extensionProvider) EstimateProtocolFraming(messages []Message, options GenerationOptions) (int, error) {
	if estimator, ok := p.inner.(ProtocolEnvelopeEstimator); ok {
		return estimator.EstimateProtocolFraming(messages, options)
	}
	// Use one token per structural JSON byte as a conservative local RPC
	// framing reserve. Message content and schemas are budgeted separately by
	// the caller. Remote provider-specific transformations remain its contract.
	type frameMessage struct {
		Role          string   `json:"role"`
		ToolCallID    string   `json:"tool_call_id,omitempty"`
		ToolCallNames []string `json:"tool_call_names,omitempty"`
	}
	frames := make([]frameMessage, len(messages))
	for index, message := range messages {
		frames[index] = frameMessage{Role: string(message.Role), ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			frames[index].ToolCallNames = append(frames[index].ToolCallNames, call.Name)
		}
	}
	payload := struct {
		Provider string         `json:"provider"`
		Messages []frameMessage `json:"messages"`
		Tools    int            `json:"tools"`
	}{Provider: p.name, Messages: frames, Tools: len(options.Tools)}
	bytes, err := json.Marshal(payload)
	return len(bytes), err
}

func (r *ModelResolver) resolveExtensionModel(ctx context.Context, definition providerDefinition, request ModelRequest, inlineKey string) (ResolvedModel, error) {
	if err := ctx.Err(); err != nil {
		return ResolvedModel{}, err
	}
	if request.Endpoint != "" || request.Credential != (CredentialRef{}) || inlineKey != "" {
		return ResolvedModel{}, fmt.Errorf("%w: extension providers do not accept host endpoint or credential overrides", ErrCredentialScopeMismatch)
	}
	provider, ok := definition.provider.(*extensionProvider)
	if !ok {
		return ResolvedModel{}, ErrMissingProviderSpec
	}
	modelID := strings.TrimSpace(request.ModelID)
	if modelID == "" {
		modelID = definition.spec.DefaultModel
	}
	model := resolveModelSpec(definition.models, provider.name, modelID)
	if !model.Known {
		return ResolvedModel{}, fmt.Errorf("llm: extension provider %q has no declared model %q", provider.name, modelID)
	}
	cfg := ModelConfig{Model: modelID, Temperature: request.Temperature, TemperatureSet: request.TemperatureSet || request.Temperature != 0,
		MaxTokens: request.MaxTokens, MaxTokensSet: request.MaxTokensSet || request.MaxTokens != 0, credentialResolved: true}
	id, err := newCandidateID()
	if err != nil {
		return ResolvedModel{}, err
	}
	candidate := &resolvedCandidateHandle{id: id, provider: provider, model: model, cfg: cfg,
		identity: AttemptIdentity{ProviderID: provider.name, ModelID: modelID, ProtocolID: ProtocolExtensionRPC, OriginKind: OriginExtensionProcess, Candidate: id}}
	return ResolvedModel{ProviderID: provider.name, ModelID: modelID, ProtocolID: ProtocolExtensionRPC,
		Provider: cloneProviderSpec(definition.spec), Model: model, provider: provider, cfg: cfg, candidate: candidate}, nil
}

type extensionChatModel struct {
	provider  *extensionProvider
	config    ModelConfig
	candidate *resolvedCandidateHandle
}

func (m *extensionChatModel) start(ctx context.Context) (BaseChatModel, func(), *ProviderAttempt, error) {
	attempt, err := (providerDispatchBinding{mode: GuardedCandidateBuild, candidate: m.candidate}).begin(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	model, cleanup, err := m.provider.inner.Build(ctx, m.config)
	if cleanup == nil {
		cleanup = func() {}
	}
	if err == nil && model == nil {
		err = ErrGuardedCandidateUnavailable
	}
	if err != nil {
		cleanup()
		return nil, nil, nil, finishProviderAttempt(ctx, attempt, err, nil)
	}
	return model, cleanup, attempt, nil
}

func (m *extensionChatModel) Generate(ctx context.Context, messages []Message, options ...Option) (*Message, error) {
	messages, option, err := m.prepare(messages, options...)
	if err != nil {
		return nil, err
	}
	options = []Option{option}
	model, cleanup, attempt, err := m.start(ctx)
	if err != nil {
		return nil, err
	}
	message, err := model.Generate(ctx, messages, options...)
	cleanup()
	var usage *Usage
	if message != nil {
		usage = message.Usage
	}
	return message, finishProviderAttempt(ctx, attempt, err, usage)
}

func (m *extensionChatModel) Stream(ctx context.Context, messages []Message, options ...Option) (<-chan MessageChunk, error) {
	messages, option, err := m.prepare(messages, options...)
	if err != nil {
		return nil, err
	}
	options = []Option{option}
	model, cleanup, attempt, err := m.start(ctx)
	if err != nil {
		return nil, err
	}
	source, err := model.Stream(ctx, messages, options...)
	if err == nil && source == nil {
		err = errors.New("llm: extension provider returned a nil stream")
	}
	if err != nil {
		cleanup()
		return nil, finishProviderAttempt(ctx, attempt, err, nil)
	}
	joined := make(chan MessageChunk)
	go func() {
		defer close(joined)
		defer cleanup()
		for chunk := range source {
			select {
			case joined <- chunk:
			case <-ctx.Done():
				// The inner stream owns its request resources and must close
				// cooperatively; drain it before cleanup and attempt finish.
				for range source {
				}
				return
			}
		}
	}()
	return finishProviderAttemptStream(ctx, joined, attempt), nil
}

func (m *extensionChatModel) prepare(messages []Message, options ...Option) ([]Message, Option, error) {
	frozen, err := cloneMessages(messages)
	if err != nil {
		return nil, nil, preDispatchCandidateError(m.candidate, PreDispatchInvalidRequest, err)
	}
	settings, err := MaterializeGenerationOptions(options...)
	if err != nil {
		return nil, nil, preDispatchCandidateError(m.candidate, PreDispatchInvalidRequest, err)
	}
	option, err := OptionFromGenerationOptions(settings)
	if err != nil {
		return nil, nil, preDispatchCandidateError(m.candidate, PreDispatchInvalidRequest, err)
	}
	return frozen, option, nil
}
