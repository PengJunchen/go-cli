package llm

import (
	"context"
	"errors"
	"testing"
)

type extensionProviderFixture struct {
	name                        string
	builds, generates, cleanups int
	config                      ModelConfig
}

func (p *extensionProviderFixture) Name() string { return p.name }
func (p *extensionProviderFixture) Models() []ModelInfo {
	return []ModelInfo{{Name: "declared", ContextWindow: 8192, MaxOutputTokens: 1024, ToolCall: true}}
}
func (p *extensionProviderFixture) Build(_ context.Context, cfg ModelConfig) (BaseChatModel, func(), error) {
	p.builds++
	p.config = cfg
	return &extensionModelFixture{provider: p}, func() { p.cleanups++ }, nil
}

type extensionModelFixture struct{ provider *extensionProviderFixture }

func (m *extensionModelFixture) Generate(context.Context, []Message, ...Option) (*Message, error) {
	m.provider.generates++
	return &Message{Role: RoleAssistant, Content: "extension-result"}, nil
}
func (m *extensionModelFixture) Stream(ctx context.Context, messages []Message, options ...Option) (<-chan MessageChunk, error) {
	message, err := m.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	out := make(chan MessageChunk, 1)
	out <- MessageChunk{Role: message.Role, Content: message.Content, Final: true}
	close(out)
	return out, nil
}

func TestExtensionProviderRequiresExplicitRegistrationAndNeverReadsCredentials(t *testing.T) {
	provider := &extensionProviderFixture{name: "local-extension"}
	registry := NewProviderRegistry()
	credentials := &countingCredentialResolver{key: "must-not-be-read"}
	registry.SetCredentialResolver(credentials)
	if err := registry.RegisterExtensionProvider(provider); err != nil {
		t.Fatal(err)
	}
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), ModelRequest{ProviderID: provider.name, ModelID: "declared"})
	if err != nil {
		t.Fatal(err)
	}
	model, cleanup, err := resolved.Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if provider.builds != 0 || credentials.lookupCalls != 0 {
		t.Fatal("resolution constructed extension or read credentials before authorization")
	}
	if _, err := model.Generate(context.Background(), nil); !errors.Is(err, ErrOperationAuthorityRequired) {
		t.Fatalf("unguarded call = %v", err)
	}
	if provider.builds != 0 {
		t.Fatal("unauthorized call built extension")
	}
	authority := &attemptTestAuthority{id: "extension-operation"}
	message, err := model.Generate(WithOperationDispatchAuthority(context.Background(), authority), []Message{{Role: RoleUser, Content: "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if message.Content != "extension-result" || provider.builds != 1 || provider.cleanups != 1 || len(authority.attempts) != 1 {
		t.Fatal("extension did not execute and finish exactly once")
	}
	if provider.config.APIKey != "" || provider.config.BaseURL != "" || provider.config.Credential != (CredentialRef{}) || credentials.lookupCalls != 0 {
		t.Fatal("host credential or endpoint entered extension provider")
	}
	if resolved.ProtocolID != ProtocolExtensionRPC || resolved.CandidateHandle().Identity().OriginKind != OriginExtensionProcess {
		t.Fatal("extension candidate mislabeled as HTTP")
	}
}

func TestExtensionProviderRejectsUndeclaredModelsAndScopeOverrides(t *testing.T) {
	registry := NewProviderRegistry()
	provider := &extensionProviderFixture{name: "extension-models"}
	if err := registry.RegisterExtensionProvider(provider); err != nil {
		t.Fatal(err)
	}
	for _, request := range []ModelRequest{
		{ProviderID: provider.name, ModelID: "undeclared"},
		{ProviderID: provider.name, ModelID: "declared", Endpoint: "https://host.example"},
		{ProviderID: provider.name, ModelID: "declared", Credential: CredentialRef{ProviderID: "openai"}},
	} {
		if _, err := registry.SnapshotResolver().ResolveModel(context.Background(), request); err == nil {
			t.Fatalf("accepted invalid extension request: %+v", request)
		}
	}
	for _, name := range []string{"openai", "eino", "anthropic", "gemini"} {
		if _, err := NewExtensionProvider(&extensionProviderFixture{name: name}); err == nil {
			t.Fatalf("accepted reserved identity %s", name)
		}
	}
}

func TestExtensionProviderDeniedDispatchCannotConstructOrCall(t *testing.T) {
	provider := &extensionProviderFixture{name: "extension-denied"}
	registry := NewProviderRegistry()
	if err := registry.RegisterExtensionProvider(provider); err != nil {
		t.Fatal(err)
	}
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), ModelRequest{ProviderID: provider.name})
	if err != nil {
		t.Fatal(err)
	}
	model, cleanup, err := resolved.Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	authority := &attemptTestAuthority{id: "denied-extension", denial: &DispatchDenial{Code: DeniedSafetyFact}}
	if _, err := model.Stream(WithOperationDispatchAuthority(context.Background(), authority), nil); !errors.Is(err, ErrDispatchPermitDenied) {
		t.Fatalf("denial = %v", err)
	}
	if provider.builds != 0 || provider.generates != 0 || len(authority.attempts) != 0 {
		t.Fatal("extension crossed denied dispatch fence")
	}
}

func TestExtensionProviderStreamJoinsCleanupBeforeFinishedAttempt(t *testing.T) {
	provider := &extensionProviderFixture{name: "extension-stream"}
	registry := NewProviderRegistry()
	if err := registry.RegisterExtensionProvider(provider); err != nil {
		t.Fatal(err)
	}
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), ModelRequest{ProviderID: provider.name})
	if err != nil {
		t.Fatal(err)
	}
	model, cleanup, err := resolved.Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	authority := &attemptTestAuthority{id: "stream-extension"}
	authority.onFinish = func() {
		if provider.cleanups != 1 {
			t.Error("attempt finished before provider cleanup")
		}
	}
	chunks, err := model.Stream(WithOperationDispatchAuthority(context.Background(), authority), []Message{{Role: RoleUser, Content: "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	got := ""
	for chunk := range chunks {
		if chunk.Error != nil {
			t.Fatal(chunk.Error)
		}
		got += chunk.Content
	}
	if got != "extension-result" || provider.cleanups != 1 || len(authority.attempts) != 1 {
		t.Fatal("stream did not finish the extension attempt")
	}
}
