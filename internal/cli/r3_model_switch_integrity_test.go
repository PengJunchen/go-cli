package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssembledModelSwitchUsesSharedRunAdmission(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	var requestOnce sync.Once
	var releaseOnce sync.Once
	releaseProviderRequest := func() { releaseOnce.Do(func() { close(releaseRequest) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestOnce.Do(func() { close(requestStarted) })
		<-releaseRequest
		writeAssemblyGenerateTextResponse(w, "run completed", "stop")
	}))
	t.Cleanup(server.Close)
	t.Cleanup(releaseProviderRequest)

	const providerName = "r3-switch-admission-provider"
	rc := &config.Config{Provider: config.ProviderConfig{
		Name:    providerName,
		BaseURL: server.URL,
		APIKey:  newTestCredentialCanary(t),
	}}
	assembly, err := AssembleAgent(context.Background(), rc, providerName, "initial-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	selector := assembly.ModelConfig.ModelSelector
	initialModel := selector.PrimaryModel()
	var initialCleanupCalls atomic.Int32
	selector.WithPrimaryCleanup(func() { initialCleanupCalls.Add(1) })

	var builderCalls atomic.Int32
	var candidateCleanupCalls [2]atomic.Int32
	selector.WithModelSpecResultBuilder(func(_ context.Context, spec llm.ModelSpec) (llm.ModelSpecBuildResult, error) {
		index := int(builderCalls.Add(1)) - 1
		model := &r3SwitchTestModel{name: spec.ModelID}
		return llm.ModelSpecBuildResult{Model: model, Cleanup: func() { candidateCleanupCalls[index].Add(1) }}, nil
	})

	runCtx, cancelRun := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRun()
	runDone := make(chan error, 1)
	go func() {
		_, runErr := assembly.TurnRunner.RunTurn(runCtx, core.Submission{Content: "hold the provider request"})
		runDone <- runErr
	}()
	select {
	case <-requestStarted:
	case <-runCtx.Done():
		t.Fatalf("provider request did not start: %v", runCtx.Err())
	}

	deps := &slashContext{modelSelector: selector, modelName: "initial-model", out: io.Discard}
	_, switchErr := (&ModelHandler{}).Handle(context.Background(), []string{"--provider", providerName, "--", "next-model"}, deps)
	releaseProviderRequest()
	select {
	case runErr := <-runDone:
		require.NoError(t, runErr)
	case <-runCtx.Done():
		t.Fatalf("run did not finish after releasing its provider request: %v", runCtx.Err())
	}

	require.ErrorIs(t, switchErr, core.ErrRunSlotBusy, "a model switch must not replace or dispose the model held by the active run")
	assert.Same(t, initialModel, selector.PrimaryModel())
	assert.Equal(t, int32(0), initialCleanupCalls.Load(), "the active model must remain owned until the run releases the shared slot")
	assert.Equal(t, int32(1), candidateCleanupCalls[0].Load(), "the candidate rejected by run admission must be cleaned exactly once")

	_, retryErr := (&ModelHandler{}).Handle(context.Background(), []string{"--provider", providerName, "--", "next-model"}, deps)
	require.NoError(t, retryErr)
	assert.Equal(t, "next-model", selector.PrimaryModelName())
	assert.Equal(t, int32(1), initialCleanupCalls.Load(), "the previous generation may retire after the run completes")
	assert.Equal(t, int32(1), candidateCleanupCalls[0].Load())
	assert.Equal(t, int32(0), candidateCleanupCalls[1].Load(), "the successfully published generation remains owned by the selector")

	assembly.Cleanup()
	assert.Equal(t, int32(1), candidateCleanupCalls[1].Load(), "assembly shutdown releases the active generation exactly once")
}

func TestAssembledModelSwitchFailureClosesSharedRunAdmission(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	const providerName = "r3-selector-failure-provider"
	assembly, err := AssembleAgent(context.Background(), &config.Config{Provider: config.ProviderConfig{
		Name:    providerName,
		BaseURL: server.URL,
	}}, providerName, "initial-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	var candidateCleanupCalls atomic.Int32
	assembly.ModelSelector.WithModelSpecResultBuilder(func(_ context.Context, spec llm.ModelSpec) (llm.ModelSpecBuildResult, error) {
		return llm.ModelSpecBuildResult{
			Model:   &r3SwitchTestModel{name: spec.ModelID},
			Cleanup: func() { candidateCleanupCalls.Add(1) },
		}, nil
	})
	assembly.ModelSelector.WithModelSwitchCallback(func(llm.BaseChatModel) { panic("r3 callback failure") })

	deps := &slashContext{modelSelector: assembly.ModelSelector, modelName: "initial-model", out: io.Discard}
	_, switchErr := (&ModelHandler{}).Handle(context.Background(), []string{"--provider", providerName, "--", "candidate-model"}, deps)
	require.ErrorIs(t, switchErr, llm.ErrModelSelectorFailed)

	_, runErr := assembly.TurnRunner.RunTurn(context.Background(), core.Submission{Content: "admission must be closed after selector failure"})
	require.ErrorIs(t, runErr, core.ErrRunAdmissionClosed)

	assembly.Cleanup()
	assert.Equal(t, int32(1), candidateCleanupCalls.Load(), "the published candidate must be released on assembly shutdown")
}

func TestModelSwitchPublishesContextWindowAndCostUsesActiveSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	const providerName = "r3-window-snapshot-provider"
	assembly, err := AssembleAgent(context.Background(), &config.Config{Provider: config.ProviderConfig{
		Name:    providerName,
		BaseURL: server.URL,
	}}, providerName, "initial-model", io.Discard, WithMaxTokens(8192))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	assembly.ModelSelector.WithModelSpecResultBuilder(func(_ context.Context, spec llm.ModelSpec) (llm.ModelSpecBuildResult, error) {
		window := 0
		if spec.ModelID == "known-window-model" {
			window = 32768
		}
		return llm.ModelSpecBuildResult{Model: &r3SwitchTestModel{name: spec.ModelID}, ContextWindow: window}, nil
	})
	deps := &slashContext{
		modelSelector:          assembly.ModelSelector,
		modelName:              "initial-model",
		contextWindow:          assembly.ContextWindow,
		localCompactionCeiling: assembly.MaxTokens,
		out:                    io.Discard,
	}

	_, err = (&ModelHandler{}).Handle(context.Background(), []string{"--provider", providerName, "--", "known-window-model"}, deps)
	require.NoError(t, err)
	assert.Equal(t, llm.ActiveModelSnapshot{ProviderID: providerName, ModelID: "known-window-model", ContextWindow: 32768}, assembly.ModelSelector.ActiveModelSnapshot())
	assert.Equal(t, "known-window-model", deps.ModelName())
	assert.Equal(t, 32768, deps.ContextWindow())

	knownUsageStream := core.NewEventStream(4)
	emitTokenUsageEvent(knownUsageStream, assembly)
	knownUsageEvent := <-knownUsageStream.Events()
	require.NotNil(t, knownUsageEvent.TokenUsage)
	assert.Equal(t, 32768, knownUsageEvent.TokenUsage.MaxTokens)

	var knownCost bytes.Buffer
	deps.out = &knownCost
	_, err = (&CostHandler{}).Handle(context.Background(), nil, deps)
	require.NoError(t, err)
	assert.Contains(t, knownCost.String(), "Context:   unknown/32768 (current occupancy unavailable)")
	assert.Contains(t, knownCost.String(), "Local compaction ceiling: 8192 tokens")

	_, err = (&ModelHandler{}).Handle(context.Background(), []string{"--provider", providerName, "--", "unknown-window-model"}, deps)
	require.NoError(t, err)
	assert.Zero(t, deps.ContextWindow(), "a switched model with unknown metadata must not retain the previous model's window")

	unknownUsageStream := core.NewEventStream(4)
	emitTokenUsageEvent(unknownUsageStream, assembly)
	unknownUsageEvent := <-unknownUsageStream.Events()
	require.NotNil(t, unknownUsageEvent.TokenUsage)
	assert.Zero(t, unknownUsageEvent.TokenUsage.MaxTokens, "the local compaction ceiling is not a model context-window substitute")

	var unknownCost bytes.Buffer
	deps.out = &unknownCost
	_, err = (&CostHandler{}).Handle(context.Background(), nil, deps)
	require.NoError(t, err)
	assert.Contains(t, unknownCost.String(), "Context:   unknown (model context window unavailable)")
	assert.Contains(t, unknownCost.String(), "Local compaction ceiling: 8192 tokens")
}

func TestAssembledCyclerSamplingMatchesModelIdentityAndPreparedRequest(t *testing.T) {
	primaryServer := newR3SamplingServer(t)
	smallServer := newR3SamplingServer(t)

	const (
		primaryProvider = "r3-primary-sampling-provider"
		smallProvider   = "r3-small-sampling-provider"
		primaryModel    = "primary-configured-model"
		smallModel      = "small-configured-model"
	)

	tests := []struct {
		name          string
		provider      string
		model         string
		wantTemp      *float64
		wantMaxTokens int
	}{
		{
			name:          "exact primary model retains explicit zero and model overrides",
			provider:      primaryProvider,
			model:         primaryModel,
			wantTemp:      r3Float64Pointer(0),
			wantMaxTokens: 128,
		},
		{
			name:          "another model at primary provider uses provider settings only",
			provider:      primaryProvider,
			model:         "primary-other-model",
			wantTemp:      r3Float64Pointer(0.6),
			wantMaxTokens: 512,
		},
		{
			name:          "exact small model uses its own explicit zero settings",
			provider:      smallProvider,
			model:         smallModel,
			wantTemp:      r3Float64Pointer(0),
			wantMaxTokens: 192,
		},
		{
			name:          "another small-provider model gets only the generic default",
			provider:      smallProvider,
			model:         "small-other-model",
			wantTemp:      nil,
			wantMaxTokens: config.DefaultLLMMaxTokens,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			primaryKey := newTestCredentialCanary(t)
			smallKey := newTestCredentialCanary(t)
			primaryServer.reset()
			smallServer.reset()

			const envPrefix = "GO_CLI_R3_CYCLER_SAMPLING"
			for _, suffix := range []string{
				"PROVIDER_NAME", "PROVIDER_BASE_URL", "PROVIDER_API_KEY", "PROVIDER_TEMPERATURE", "PROVIDER_MAX_TOKENS",
				"MODEL_NAME", "MODEL_TEMPERATURE", "MODEL_MAX_TOKENS",
				"SMALL_MODEL_PROVIDER", "SMALL_MODEL_BASE_URL", "SMALL_MODEL_API_KEY", "SMALL_MODEL_MODEL", "SMALL_MODEL_TEMPERATURE", "SMALL_MODEL_MAX_TOKENS",
				"MODELCYCLER_ENABLED",
			} {
				t.Setenv(envPrefix+"_"+suffix, "")
			}
			configData, err := json.Marshal(map[string]any{
				"provider": map[string]any{
					"name": primaryProvider, "base_url": primaryServer.server.URL, "api_key": primaryKey,
					"temperature": 0.6, "max_tokens": 512,
				},
				"model": map[string]any{"temperature": 0, "max_tokens": 128},
				"small_model": map[string]any{
					"provider": smallProvider, "base_url": smallServer.server.URL, "api_key": smallKey, "model": smallModel,
					"temperature": 0, "max_tokens": 192,
				},
				"model_cycler": map[string]any{
					"enabled": true, "strategy": llm.StrategyRoundRobin,
					"models": []config.ModelEntry{{Provider: test.provider, Model: test.model}},
				},
			})
			require.NoError(t, err)
			rc, err := config.NewLoader().WithFileBytes("r3-cycler-sampling.json", configData).WithEnvPrefix(envPrefix).Load(context.Background())
			require.NoError(t, err)

			assembly, err := AssembleAgent(context.Background(), rc, primaryProvider, primaryModel, io.Discard, WithApproveMode(ApproveAuto))
			require.NoError(t, err)
			t.Cleanup(assembly.Cleanup)

			recorder := &r3PreparedPlanRecordingModel{BaseChatModel: assembly.Model}
			assembly.LoopAgent.SetModel(recorder)
			_, err = assembly.TurnRunner.RunTurn(context.Background(), core.Submission{Content: "exercise resolved cycler sampling"})
			require.NoError(t, err)

			server := primaryServer
			if test.provider == smallProvider {
				server = smallServer
			}
			request := server.singleRequest(t)
			wantAPIKey := primaryKey
			otherAPIKey := smallKey
			if test.provider == smallProvider {
				wantAPIKey, otherAPIKey = smallKey, primaryKey
			}
			assert.Equal(t, "Bearer "+wantAPIKey, request.authorization)
			assert.NotContains(t, request.authorization, otherAPIKey)

			var body struct {
				Model       string   `json:"model"`
				Temperature *float64 `json:"temperature"`
				MaxTokens   *int     `json:"max_tokens"`
			}
			require.NoError(t, json.Unmarshal(request.body, &body))
			assert.Equal(t, test.model, body.Model)
			assert.Equal(t, test.wantTemp, body.Temperature, "HTTP temperature must preserve explicit zero and identity-scoped inheritance")
			require.NotNil(t, body.MaxTokens)
			assert.Equal(t, test.wantMaxTokens, *body.MaxTokens)

			plans := recorder.planSnapshot()
			require.Len(t, plans, 1)
			planOptions := plans[0].Options()
			assert.Equal(t, test.wantTemp, planOptions.Temperature, "prepared-plan temperature must match the wire request")
			require.NotNil(t, planOptions.MaxTokens)
			assert.Equal(t, test.wantMaxTokens, *planOptions.MaxTokens, "prepared-plan token limit must match the wire request")
		})
	}
}

func TestAssemblyKeepsUnknownModelContextSeparateFromCompactionCeiling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	assembly, err := AssembleAgent(context.Background(), &config.Config{Provider: config.ProviderConfig{
		Name:    "r3-unknown-window-provider",
		BaseURL: server.URL,
	}}, "r3-unknown-window-provider", "unknown-window-model", io.Discard, WithMaxTokens(8192))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	assert.Zero(t, assembly.ContextWindow, "a local compaction threshold must not be reported as the model context window")
	assert.Equal(t, 8192, assembly.MaxTokens, "the independent local compaction ceiling must remain available")
}

type r3SamplingRequest struct {
	authorization string
	body          []byte
}

type r3SamplingServer struct {
	server   *httptest.Server
	requests chan r3SamplingRequest
}

func newR3SamplingServer(t *testing.T) *r3SamplingServer {
	t.Helper()
	result := &r3SamplingServer{requests: make(chan r3SamplingRequest, 4)}
	result.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result.requests <- r3SamplingRequest{authorization: request.Header.Get("Authorization"), body: body}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"accepted\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(result.server.Close)
	return result
}

func (s *r3SamplingServer) reset() {
	for {
		select {
		case <-s.requests:
		default:
			return
		}
	}
}

func (s *r3SamplingServer) singleRequest(t *testing.T) r3SamplingRequest {
	t.Helper()
	select {
	case request := <-s.requests:
		return request
	case <-time.After(5 * time.Second):
		t.Fatal("provider did not receive the selected cycler request")
		return r3SamplingRequest{}
	}
}

func r3Float64Pointer(value float64) *float64 { return &value }

type r3SwitchTestModel struct {
	name string
}

func (m *r3SwitchTestModel) Generate(context.Context, []llm.Message, ...llm.Option) (*llm.Message, error) {
	return &llm.Message{Role: llm.RoleAssistant, Content: m.name}, nil
}

func (m *r3SwitchTestModel) Stream(context.Context, []llm.Message, ...llm.Option) (<-chan llm.MessageChunk, error) {
	return nil, nil
}
