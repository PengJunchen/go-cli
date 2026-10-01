package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssembledPrimaryUsesConfiguredSamplingAndCredentialScope(t *testing.T) {
	capture := &providerRequestCapture{}
	srv := httptest.NewServer(http.HandlerFunc(capture.handler))
	t.Cleanup(srv.Close)

	const providerName = "primary-assembly-gateway"
	apiKey := newTestCredentialCanary(t)
	rc := &config.Config{
		Provider: config.ProviderConfig{
			Name:        providerName,
			BaseURL:     srv.URL + "/primary",
			APIKey:      apiKey,
			Temperature: 0.8,
			MaxTokens:   512,
		},
		Model: config.ModelConfig{
			Temperature: 0.25,
			MaxTokens:   128,
		},
	}

	assembly, err := AssembleAgent(context.Background(), rc, providerName, "selected-model", io.Discard)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	_, missingOwnerErr := assembly.ModelConfig.Model.Generate(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "hello"}})
	require.ErrorIs(t, missingOwnerErr, llm.ErrOperationAuthorityRequired)
	count, _, _, _ := capture.snapshot()
	assert.Zero(t, count, "a resolver-built model must not dispatch without the Core Loop operation owner")
	response, err := assembly.ModelConfig.Model.Generate(withCLITestOperation(context.Background()), []llm.Message{{Role: llm.RoleUser, Content: "hello"}})
	require.NoError(t, err)
	require.NotNil(t, response)

	count, path, authorization, body := capture.snapshot()
	assert.Equal(t, 1, count)
	assert.Equal(t, "/primary/chat/completions", path)
	assert.Equal(t, "Bearer "+apiKey, authorization)
	var request struct {
		Model       string  `json:"model"`
		Temperature float64 `json:"temperature"`
		MaxTokens   int     `json:"max_tokens"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &request))
	assert.Equal(t, "selected-model", request.Model)
	assert.Equal(t, 0.25, request.Temperature, "model sampling config takes precedence over provider defaults")
	assert.Equal(t, 128, request.MaxTokens, "model token limit takes precedence over provider defaults")
}

func TestAssembledModelCyclerUsesResolvedCandidatesAndAttemptAccounting(t *testing.T) {
	var mu sync.Mutex
	var requestModels []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		requestModels = append(requestModels, request.Model)
		mu.Unlock()
		if request.Model == "cycle-selected-model" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"temporary failure","type":"server_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"primary response\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":3}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	enabled := true
	const providerName = "cycler-assembly-provider"
	rc := &config.Config{
		Provider: config.ProviderConfig{
			Name:    providerName,
			BaseURL: server.URL,
			APIKey:  newTestCredentialCanary(t),
		},
		ModelCycler: config.ModelCyclerConfig{
			Enabled:  &enabled,
			Strategy: llm.StrategyRoundRobin,
			Models:   []config.ModelEntry{{Provider: providerName, Model: "cycle-selected-model"}},
		},
	}

	assembly, err := AssembleAgent(context.Background(), rc, providerName, "cycle-primary-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	result, err := assembly.TurnRunner.RunTurn(context.Background(), core.Submission{Content: "route through the cycler"})
	require.NoError(t, err)
	assert.Equal(t, "primary response", result.Message)

	mu.Lock()
	require.Equal(t, []string{"cycle-selected-model", "cycle-primary-model"}, requestModels)
	mu.Unlock()
	_, summary := assembly.CostTracker.AttemptCostSnapshot()
	assert.Equal(t, 2, summary.Attempts)
	assert.Equal(t, 1, summary.UnknownUsageAttempts, "the failed selected provider attempt has no authoritative usage")
	assert.Equal(t, 4, summary.KnownInputTokens)
	assert.Equal(t, 3, summary.KnownOutputTokens)

	var costOutput bytes.Buffer
	costDeps := &slashContext{out: &costOutput, costTracker: assembly.CostTracker}
	_, err = (&CostHandler{}).Handle(context.Background(), nil, costDeps)
	require.NoError(t, err)
	assert.Same(t, assembly.CostTracker, costDeps.CostTracker(), "the report reads the Assembly's live tracker")

	report := costOutput.String()
	assert.Contains(t, report, "Known cost subtotal: $0.0000", "both rows are unpriced, so the known subtotal stays explicitly zero")
	assert.Contains(t, report, "Attempt known cost subtotal: $0.0000")
	assert.Contains(t, report, "Provider attempts: 2")
	assert.Contains(t, report, "Known provider tokens: 4 in / 3 out", "known usage is counted once from the successful physical attempt")
	assert.Contains(t, report, "Unknown usage attempts: 1")
	assert.Contains(t, report, "Unknown cost attempts: 2", "the default cost tiers do not price these fixture model IDs")
	assert.Equal(t, 2, strings.Count(report, "attempt_id="), "each physical attempt should render once")
	assert.Contains(t, report, `model="cycle-selected-model" usage=unknown cost=unknown price_source=unknown`)
	assert.Contains(t, report, `model="cycle-primary-model" usage=reported (4 in / 3 out) cost=unknown price_source=unknown`)
	assert.NotContains(t, report, "Known provider tokens: 8 in / 6 out", "rendering must not add request tokens a second time")

	_, summaryAfterReport := assembly.CostTracker.AttemptCostSnapshot()
	assert.Equal(t, summary, summaryAfterReport, "rendering the report must not record attempts or add their usage again")
}

func TestReviewProviderFlagOverrideDoesNotRebindProviderConfigKey(t *testing.T) {
	capture := &providerRequestCapture{}
	srv := httptest.NewServer(http.HandlerFunc(capture.handler))
	t.Cleanup(srv.Close)

	const configProvider = "config-provider"
	const overrideProvider = "override-provider"
	apiKey := newTestCredentialCanary(t)
	rc := &config.Config{Provider: config.ProviderConfig{
		Name:    configProvider,
		BaseURL: srv.URL,
		APIKey:  apiKey,
	}}
	providerName := resolveProviderName(overrideProvider, rc)
	assert.Equal(t, overrideProvider, providerName)

	assembly, err := AssembleAgent(context.Background(), rc, providerName, "override-model", io.Discard)
	if err == nil && assembly != nil {
		t.Cleanup(assembly.Cleanup)
		_, requestErr := assembly.ModelConfig.Model.Generate(withCLITestOperation(context.Background()), []llm.Message{{Role: llm.RoleUser, Content: "override"}})
		assert.NoError(t, requestErr)
	}

	count, _, authorization, body := capture.snapshot()
	assert.Zero(t, count, "the config provider endpoint must not be treated as the override provider endpoint")
	assert.NotContains(t, authorization, apiKey)
	assert.NotContains(t, body, apiKey)
	require.ErrorIs(t, err, llm.ErrProviderEndpointRequired, "an unconfigured custom override must fail closed")
	assert.NotContains(t, err.Error(), apiKey)
}

func TestAssembledProviderFlagCanonicalAliasKeepsProviderConfig(t *testing.T) {
	capture := &providerRequestCapture{}
	srv := httptest.NewServer(http.HandlerFunc(capture.handler))
	t.Cleanup(srv.Close)

	apiKey := newTestCredentialCanary(t)
	rc := &config.Config{Provider: config.ProviderConfig{
		Name:    " OPENAI ",
		BaseURL: srv.URL + "/configured-openai",
		APIKey:  apiKey,
	}}
	providerName := resolveProviderName("openai", rc)

	assembly, err := AssembleAgent(context.Background(), rc, providerName, "selected-model", io.Discard)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	response, err := assembly.ModelConfig.Model.Generate(withCLITestOperation(context.Background()), []llm.Message{{Role: llm.RoleUser, Content: "alias"}})
	require.NoError(t, err)
	require.NotNil(t, response)
	count, path, authorization, _ := capture.snapshot()
	assert.Equal(t, 1, count)
	assert.Equal(t, "/configured-openai/chat/completions", path)
	assert.Equal(t, "Bearer "+apiKey, authorization)
}

func TestSmallModelOverrideDoesNotInheritDifferentProviderConfig(t *testing.T) {
	const configuredProvider = "config-provider"
	const overrideProvider = "openai"
	rc := &config.Config{
		Provider: config.ProviderConfig{
			Name:    configuredProvider,
			BaseURL: "https://config-provider.example/v1",
			APIKey:  newTestCredentialCanary(t),
		},
		SmallModel: config.SmallModelConfig{Model: "small-model"},
	}

	resolver, err := newCLIModelResolver(rc, overrideProvider, true)
	require.NoError(t, err)
	assert.Empty(t, resolver.primaryEndpoint, "the provider block endpoint is not owned by the CLI override")

	endpoint, apiKey, err := resolveSmallModelProviderSettings(overrideProvider, overrideProvider, rc)
	require.NoError(t, err)
	assert.Empty(t, endpoint, "small model without its own endpoint must keep the override's descriptor")
	assert.Empty(t, apiKey, "small model without its own credential must not inherit another provider's key")

	ref, err := resolver.resolver.CredentialRefFor(overrideProvider, endpoint)
	require.NoError(t, err)
	assert.Equal(t, overrideProvider, ref.ProviderID)
	assert.Equal(t, "https://api.openai.com:443", ref.Origin, "the canonical override retains its own default origin")
}

func TestAssembledSmallModelSharesCredentialOnlyWithinSameOrigin(t *testing.T) {
	tests := []struct {
		name              string
		primaryProvider   string
		smallProvider     string
		primaryPath       string
		smallPath         string
		wantAuthorization bool
	}{
		{
			name:              "same provider with different paths",
			primaryProvider:   "shared-assembly-gateway",
			smallProvider:     "shared-assembly-gateway",
			primaryPath:       "/primary",
			smallPath:         "/small",
			wantAuthorization: true,
		},
		{
			name:              "same provider with different origin",
			primaryProvider:   "same-identity-primary",
			smallProvider:     "same-identity-primary",
			primaryPath:       "/primary",
			wantAuthorization: false,
		},
		{
			name:              "different provider",
			primaryProvider:   "different-identity-primary",
			smallProvider:     "different-identity-small",
			primaryPath:       "/primary",
			wantAuthorization: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			primaryCapture := &providerRequestCapture{}
			primaryServer := httptest.NewServer(http.HandlerFunc(primaryCapture.handler))
			t.Cleanup(primaryServer.Close)
			smallCapture := primaryCapture
			smallURL := primaryServer.URL + tt.smallPath
			if tt.name != "same provider with different paths" {
				smallCapture = &providerRequestCapture{}
				smallServer := httptest.NewServer(http.HandlerFunc(smallCapture.handler))
				t.Cleanup(smallServer.Close)
				smallURL = smallServer.URL + tt.smallPath
			}

			primaryKey := newTestCredentialCanary(t)
			rc := &config.Config{
				Provider: config.ProviderConfig{
					Name:    tt.primaryProvider,
					BaseURL: primaryServer.URL + tt.primaryPath,
					APIKey:  primaryKey,
				},
				SmallModel: config.SmallModelConfig{
					Provider: tt.smallProvider,
					BaseURL:  smallURL,
					Model:    "small-model",
				},
			}

			assembly, err := AssembleAgent(context.Background(), rc, tt.primaryProvider, "primary-model", io.Discard)
			require.NoError(t, err)
			t.Cleanup(assembly.Cleanup)
			smallModel := assembly.ModelConfig.ModelSelector.SmallModel()
			require.NotNil(t, smallModel)

			response, err := smallModel.Generate(withCLITestOperation(context.Background()), []llm.Message{{Role: llm.RoleUser, Content: "summarize"}})
			require.NoError(t, err)
			require.NotNil(t, response)

			_, _, authorization, body := smallCapture.snapshot()
			if tt.wantAuthorization {
				assert.Equal(t, "Bearer "+primaryKey, authorization)
				assert.NotContains(t, body, primaryKey)
			} else {
				assert.Empty(t, authorization, "primary credential must not cross provider or origin scope")
				assert.NotContains(t, body, primaryKey)
			}
			primaryCount, _, _, _ := primaryCapture.snapshot()
			if tt.name != "same provider with different paths" {
				assert.Zero(t, primaryCount, "only the selected small-model endpoint should receive this request")
			}
		})
	}
}

func TestAssembledConflictingSameScopeSmallCredentialFailsWithoutLeak(t *testing.T) {
	capture := &providerRequestCapture{}
	srv := httptest.NewServer(http.HandlerFunc(capture.handler))
	t.Cleanup(srv.Close)

	const providerName = "conflicting-assembly-gateway"
	primaryKey := newTestCredentialCanary(t)
	smallKey := newTestCredentialCanary(t)
	rc := &config.Config{
		Provider: config.ProviderConfig{
			Name:    providerName,
			BaseURL: srv.URL + "/primary",
			APIKey:  primaryKey,
		},
		SmallModel: config.SmallModelConfig{
			Provider: providerName,
			BaseURL:  srv.URL + "/small",
			APIKey:   smallKey,
			Model:    "small-model",
		},
	}

	resolver, err := newCLIModelResolver(rc, providerName, true)
	require.NoError(t, err)
	require.Error(t, resolver.smallErr)
	assert.Contains(t, resolver.smallErr.Error(), "conflicting primary/small credentials")
	assert.NotContains(t, resolver.smallErr.Error(), primaryKey)
	assert.NotContains(t, resolver.smallErr.Error(), smallKey)

	assembly, err := AssembleAgent(context.Background(), rc, providerName, "primary-model", io.Discard)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	assert.Nil(t, assembly.ModelConfig.ModelSelector.SmallModel(), "ambiguous same-scope credentials must disable only the small model")

	response, err := assembly.ModelConfig.Model.Generate(withCLITestOperation(context.Background()), []llm.Message{{Role: llm.RoleUser, Content: "primary"}})
	require.NoError(t, err)
	require.NotNil(t, response)

	count, path, authorization, body := capture.snapshot()
	assert.Equal(t, 1, count)
	assert.Equal(t, "/primary/chat/completions", path)
	assert.Equal(t, "Bearer "+primaryKey, authorization)
	assert.NotContains(t, body, primaryKey)
	assert.NotContains(t, body, smallKey)
}

func TestAssembledModelSwitchUsesConfiguredResolverRequest(t *testing.T) {
	capture := &providerRequestCapture{}
	srv := httptest.NewServer(http.HandlerFunc(capture.handler))
	t.Cleanup(srv.Close)

	const providerName = "switch-assembly-gateway"
	apiKey := newTestCredentialCanary(t)
	rc := &config.Config{
		Provider: config.ProviderConfig{
			Name:        providerName,
			BaseURL:     srv.URL,
			APIKey:      apiKey,
			Temperature: 0.7,
			MaxTokens:   256,
		},
		Model: config.ModelConfig{Temperature: 0.3, MaxTokens: 96},
	}
	assembly, err := AssembleAgent(context.Background(), rc, providerName, "initial-model", io.Discard)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	initialModel := assembly.ModelConfig.ModelSelector.PrimaryModel()
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	err = assembly.ModelConfig.ModelSelector.SwitchModel(canceledCtx, "rejected-model")
	require.ErrorIs(t, err, context.Canceled)
	assert.Same(t, initialModel, assembly.ModelConfig.ModelSelector.PrimaryModel(), "failed model construction must leave the selected model unchanged")
	assert.Equal(t, "initial-model", assembly.ModelConfig.ModelSelector.PrimaryModelName())

	require.NoError(t, assembly.ModelConfig.ModelSelector.SwitchModel(context.Background(), "switched-model"))
	count, _, _, _ := capture.snapshot()
	assert.Zero(t, count, "switching constructs a scoped model without dispatching a provider request")

	response, err := assembly.ModelConfig.ModelSelector.PrimaryModel().Generate(withCLITestOperation(context.Background()), []llm.Message{{Role: llm.RoleUser, Content: "after switch"}})
	require.NoError(t, err)
	require.NotNil(t, response)

	count, _, authorization, body := capture.snapshot()
	assert.Equal(t, 1, count)
	assert.Equal(t, "Bearer "+apiKey, authorization)
	var request struct {
		Model       string  `json:"model"`
		Temperature float64 `json:"temperature"`
		MaxTokens   int     `json:"max_tokens"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &request))
	assert.Equal(t, "switched-model", request.Model)
	assert.Equal(t, 0.3, request.Temperature)
	assert.Equal(t, 96, request.MaxTokens)
}

func TestAssembledNativeProviderRejectsBeforeConfiguredEndpoint(t *testing.T) {
	capture := &providerRequestCapture{}
	srv := httptest.NewServer(http.HandlerFunc(capture.handler))
	t.Cleanup(srv.Close)

	rc := &config.Config{Provider: config.ProviderConfig{
		Name:    "anthropic",
		BaseURL: srv.URL,
		APIKey:  newTestCredentialCanary(t),
	}}
	assembly, err := AssembleAgent(context.Background(), rc, "anthropic", "native-model", io.Discard)
	if assembly != nil {
		t.Cleanup(assembly.Cleanup)
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported")
	count, _, _, _ := capture.snapshot()
	assert.Zero(t, count, "native identity must fail before dispatch to an OpenAI-compatible endpoint")
}

func TestAssembledModelConfigExplicitZeroSamplingOverridesProviderDefaults(t *testing.T) {
	capture := &providerRequestCapture{}
	srv := httptest.NewServer(http.HandlerFunc(capture.handler))
	t.Cleanup(srv.Close)

	const providerName = "zero-sampling-gateway"
	rc := &config.Config{
		Provider: config.ProviderConfig{
			Name:        providerName,
			BaseURL:     srv.URL,
			APIKey:      newTestCredentialCanary(t),
			Temperature: 0.8,
			MaxTokens:   512,
		},
	}
	rc.Model.SetTemperature(0)
	rc.Model.SetMaxTokens(0)
	assembly, err := AssembleAgent(context.Background(), rc, providerName, "zero-model", io.Discard)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	_, err = assembly.ModelConfig.Model.Generate(withCLITestOperation(context.Background()), []llm.Message{{Role: llm.RoleUser, Content: "hello"}})
	require.NoError(t, err)
	_, _, _, body := capture.snapshot()
	assert.Contains(t, body, `"temperature":0`)
	assert.Contains(t, body, `"max_tokens":0`)
}

func TestAssembledResolverDoesNotUseUnscopedOpenAIEnvironmentCredential(t *testing.T) {
	globalCredential := newTestCredentialCanary(t)
	t.Setenv("OPENAI_API_KEY", globalCredential)

	model, _, cleanup, err := buildModel(context.Background(), &config.Config{}, "openai", "scoped-model")
	if cleanup != nil {
		cleanup()
	}
	require.Error(t, err)
	require.ErrorIs(t, err, llm.ErrProviderCredentialRequired)
	assert.Nil(t, model)
	assert.NotContains(t, err.Error(), globalCredential)
}
