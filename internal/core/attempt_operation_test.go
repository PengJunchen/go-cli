package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
)

type attemptGuardedModel struct{ llm.BaseChatModel }

func (m attemptGuardedModel) ProviderCandidates() []llm.ResolvedCandidateHandle {
	return llm.ProviderCandidatesFromModel(m.BaseChatModel)
}

func (attemptGuardedModel) MaxCandidateBytes() int { return llm.GuardedStreamMaxBytes }

func (attemptGuardedModel) CheckCandidate(context.Context, string) (*CandidateOutputResult, error) {
	return &CandidateOutputResult{Allowed: true}, nil
}

type attemptResultRecorder struct {
	mu      sync.Mutex
	results []llm.ProviderAttemptResult
}

type operationTestRetryPolicy struct{}

func (operationTestRetryPolicy) ShouldRetry(context.Context, error, int) bool   { return true }
func (operationTestRetryPolicy) NextBackoff(context.Context, int) time.Duration { return 0 }
func (operationTestRetryPolicy) Name() string                                   { return "operation-test-retry" }

func (r *attemptResultRecorder) record(result llm.ProviderAttemptResult) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, result)
	return nil
}

func (r *attemptResultRecorder) snapshot() []llm.ProviderAttemptResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]llm.ProviderAttemptResult(nil), r.results...)
}

type operationControlledRetryPolicy struct {
	mu             sync.Mutex
	allowedRetries int
	attempts       []int
}

func (p *operationControlledRetryPolicy) ShouldRetry(_ context.Context, _ error, attempt int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempts = append(p.attempts, attempt)
	return attempt < p.allowedRetries
}

func (*operationControlledRetryPolicy) NextBackoff(context.Context, int) time.Duration { return 0 }
func (*operationControlledRetryPolicy) Name() string                                   { return "operation-controlled" }

func (p *operationControlledRetryPolicy) calls() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.attempts...)
}

func TestLoopOwnsResolvedProviderDispatchAndFinish(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "done"}}},
			"usage":   map[string]any{"prompt_tokens": 7, "completion_tokens": 2, "total_tokens": 9},
		})
	}))
	defer server.Close()

	registry := llm.NewProviderRegistry()
	require.NoError(t, registry.Register(llm.NewEinoProvider(
		llm.WithProviderName("core-attempt-test"),
		llm.WithHTTPClient(server.Client()),
	)))
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID: "core-attempt-test",
		ModelID:    "core-attempt-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()

	recorder := &attemptResultRecorder{}
	loop := NewLoopAgent(
		WithLLM(model),
		WithMaxIterations(1),
		WithProviderAttemptRecorder(recorder.record),
	)
	_, err = loop.Run(context.Background(), Submission{Content: "hello"})
	require.NoError(t, err)
	require.Equal(t, 1, requests, "one Core operation must cross exactly one HTTP dispatch fence")

	results := recorder.snapshot()
	require.Len(t, results, 1, "the dispatch owner must receive one terminal attempt result")
	result := results[0]
	require.NotEmpty(t, result.OperationID)
	require.NotEmpty(t, result.AttemptID)
	require.Equal(t, "core-attempt-test", result.Identity.ProviderID)
	require.Equal(t, "core-attempt-model", result.Identity.ModelID)
	require.Equal(t, llm.UsageKnown, result.Usage.State)
	require.Equal(t, 7, result.Usage.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.Usage.OutputTokens)
}

func TestLoopCapsActualDispatchesAcrossRetryMiddleware(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "temporary provider failure", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	registry := llm.NewProviderRegistry()
	require.NoError(t, registry.Register(llm.NewEinoProvider(
		llm.WithProviderName("core-retry-cap-test"),
		llm.WithHTTPClient(server.Client()),
	)))
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID: "core-retry-cap-test",
		ModelID:    "core-retry-cap-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()

	recorder := &attemptResultRecorder{}
	retry := llm.NewRetryModelMiddleware(llm.WithRetryPolicy(operationTestRetryPolicy{}))
	loop := NewLoopAgent(
		WithLLM(model),
		WithMaxIterations(1),
		WithProviderAttemptRecorder(recorder.record),
		WithProviderRetryPolicy(operationTestRetryPolicy{}),
		WithModelWrapper(func(value any) any {
			base, ok := value.(llm.BaseChatModel)
			if !ok {
				return value
			}
			return retry.WrapModel(base)
		}),
	)
	_, err = loop.Run(context.Background(), Submission{Content: "hello"})
	require.Error(t, err)
	terminal, finished := llm.FinishedProviderAttemptFromError(err)
	require.True(t, finished, "the operation returns its final joined provider terminal")
	require.Equal(t, llm.FailureTransientProvider, terminal.Failure)
	require.Equal(t, defaultProviderOperationAttempts, requests, "the actual HTTP dispatch count must honor the Loop-owned cap")

	results := recorder.snapshot()
	require.Len(t, results, defaultProviderOperationAttempts)
	seen := make(map[llm.ProviderAttemptID]struct{}, len(results))
	for _, result := range results {
		require.Equal(t, llm.FailureTransientProvider, result.Failure)
		require.Equal(t, llm.UsageUnknown, result.Usage.State)
		require.Equal(t, resolved.CandidateHandle().CandidateID(), result.Identity.Candidate)
		if _, duplicate := seen[result.AttemptID]; duplicate {
			t.Fatalf("attempt ID %q was recorded twice", result.AttemptID)
		}
		seen[result.AttemptID] = struct{}{}
	}
}

func TestLoopOwnsRetryPolicyDecisionForActiveOperation(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "temporary provider failure", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	registry := llm.NewProviderRegistry()
	require.NoError(t, registry.Register(llm.NewEinoProvider(
		llm.WithProviderName("core-retry-owner-test"),
		llm.WithHTTPClient(server.Client()),
	)))
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID: "core-retry-owner-test",
		ModelID:    "core-retry-owner-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()

	loopPolicy := &operationControlledRetryPolicy{allowedRetries: 1}
	middlewarePolicy := &operationControlledRetryPolicy{allowedRetries: 5}
	retry := llm.NewRetryModelMiddleware(llm.WithRetryPolicy(middlewarePolicy))
	recorder := &attemptResultRecorder{}
	loop := NewLoopAgent(
		WithLLM(model),
		WithMaxIterations(1),
		WithProviderAttemptRecorder(recorder.record),
		WithProviderRetryPolicy(loopPolicy),
		WithModelWrapper(func(value any) any {
			base, ok := value.(llm.BaseChatModel)
			if !ok {
				return value
			}
			return retry.WrapModel(base)
		}),
	)

	_, err = loop.Run(context.Background(), Submission{Content: "retry policy owner"})
	require.Error(t, err)
	require.Equal(t, 2, requests, "the Loop policy allows one retry and the operation owner stops further dispatch")
	require.Len(t, recorder.snapshot(), 2)
	require.Equal(t, []int{0, 1}, loopPolicy.calls(), "the Loop must evaluate the policy against typed finished attempts")
	require.Empty(t, middlewarePolicy.calls(), "active retry middleware must not make policy decisions")
}

func TestLoopRetryStreamIncrementalEventsCarryRunAndTranscriptCoordinates(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
			http.Error(w, "temporary provider failure", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"retried response\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	registry := llm.NewProviderRegistry()
	require.NoError(t, registry.Register(llm.NewEinoProvider(
		llm.WithProviderName("core-retry-coordinate-test"),
		llm.WithHTTPClient(server.Client()),
	)))
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID: "core-retry-coordinate-test",
		ModelID:    "core-retry-coordinate-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()

	policy := &operationControlledRetryPolicy{allowedRetries: 1}
	loop := NewLoopAgent(
		WithLLM(model),
		WithMaxIterations(1),
		WithProviderRetryPolicy(policy),
	)
	stream := &collectingEventStream{}
	result, err := loop.RunCanonical(withRunID(context.Background(), "run-retry-coordinate"), Submission{Content: "retry this stream"}, stream)
	require.NoError(t, err)
	require.NotEmpty(t, result.Transcript)
	require.Equal(t, 2, requests, "Core should retry the first transient HTTP failure once")

	var incremental []AgentEvent
	var complete []AgentEvent
	for _, event := range stream.collected() {
		if event.Kind != "message" {
			continue
		}
		if event.Incremental {
			incremental = append(incremental, event)
		} else {
			complete = append(complete, event)
		}
	}
	require.Len(t, incremental, 1, "the failed request must not publish a partial event")
	require.Len(t, complete, 1)
	for _, event := range []AgentEvent{incremental[0], complete[0]} {
		require.Equal(t, "run-retry-coordinate", event.RunID)
		require.NotNil(t, event.CanonicalTranscriptIndex)
		require.Equal(t, uint64(1), *event.CanonicalTranscriptIndex, "the assistant row follows the input at canonical index zero")
	}
	require.Equal(t, "retried response", incremental[0].Content)
	require.Equal(t, "retried response", complete[0].Content)
}

func TestLoopModelCyclerUsesCoreAuthorityForSelectedAndPrimaryDispatches(t *testing.T) {
	var requestModels []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requestModels = append(requestModels, request.Model)
		if request.Model == "cycle-selected-model" {
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"primary response\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	registry := llm.NewProviderRegistry()
	for _, provider := range []string{"core-cycle-selected", "core-cycle-primary"} {
		require.NoError(t, registry.Register(llm.NewEinoProvider(
			llm.WithProviderName(provider),
			llm.WithHTTPClient(server.Client()),
		)))
	}
	resolver := registry.SnapshotResolver()
	selected, err := resolver.ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID: "core-cycle-selected",
		ModelID:    "cycle-selected-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	primary, err := resolver.ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID: "core-cycle-primary",
		ModelID:    "cycle-primary-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	primaryModel, cleanup, err := primary.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()
	cycler := llm.NewModelCycler(llm.ModelCyclerConfig{
		Models: []llm.ModelEntry{{Provider: "core-cycle-selected", Model: "cycle-selected-model"}},
	}).WithResolvedModels([]llm.ResolvedModel{selected})
	recorder := &attemptResultRecorder{}
	loop := NewLoopAgent(
		WithLLM(cycler.WrapModel(primaryModel)),
		WithMaxIterations(1),
		WithProviderAttemptRecorder(recorder.record),
	)

	_, err = loop.Run(context.Background(), Submission{Content: "cycle"})
	require.NoError(t, err)
	require.Equal(t, []string{"cycle-selected-model", "cycle-primary-model"}, requestModels)
	results := recorder.snapshot()
	require.Len(t, results, 2)
	require.Equal(t, llm.FailureTransientProvider, results[0].Failure)
	require.Equal(t, selected.CandidateHandle().CandidateID(), results[0].Identity.Candidate)
	require.Equal(t, llm.AttemptSuccess, results[1].Outcome)
	require.Equal(t, primary.CandidateHandle().CandidateID(), results[1].Identity.Candidate)
	require.Equal(t, results[0].OperationID, results[1].OperationID, "cycler fallback must share its selected request's operation")
}

func TestLoopFailoverUsesCoreAuthorityAndSharesOperation(t *testing.T) {
	var requestModels []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requestModels = append(requestModels, request.Model)
		if request.Model == "failover-selected-model" {
			http.Error(w, "temporary provider failure", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"fallback response\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	registry := llm.NewProviderRegistry()
	for _, provider := range []string{"core-failover-selected", "core-failover-fallback"} {
		require.NoError(t, registry.Register(llm.NewEinoProvider(
			llm.WithProviderName(provider),
			llm.WithHTTPClient(server.Client()),
		)))
	}
	resolver := registry.SnapshotResolver()
	selected, err := resolver.ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID: "core-failover-selected",
		ModelID:    "failover-selected-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	fallback, err := resolver.ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID: "core-failover-fallback",
		ModelID:    "failover-fallback-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	selectedModel, selectedCleanup, err := selected.Build(context.Background())
	require.NoError(t, err)
	defer selectedCleanup()
	fallbackModel, fallbackCleanup, err := fallback.Build(context.Background())
	require.NoError(t, err)
	defer fallbackCleanup()
	failover := llm.NewFailoverModelMiddleware(llm.WithFallbackModels(fallbackModel))
	recorder := &attemptResultRecorder{}
	loop := NewLoopAgent(
		WithLLM(failover.WrapModel(selectedModel)),
		WithMaxIterations(1),
		WithProviderAttemptRecorder(recorder.record),
	)

	result, err := loop.RunCanonical(context.Background(), Submission{Content: "use failover"})
	require.NoError(t, err)
	require.NotEmpty(t, result.Transcript)
	require.Equal(t, "fallback response", result.Transcript[len(result.Transcript)-1].Message.Content)
	require.Equal(t, []string{"failover-selected-model", "failover-fallback-model"}, requestModels)
	results := recorder.snapshot()
	require.Len(t, results, 2)
	require.Equal(t, llm.FailureTransientProvider, results[0].Failure)
	require.Equal(t, selected.CandidateHandle().CandidateID(), results[0].Identity.Candidate)
	require.Equal(t, llm.AttemptSuccess, results[1].Outcome)
	require.Equal(t, fallback.CandidateHandle().CandidateID(), results[1].Identity.Candidate)
	require.Equal(t, results[0].OperationID, results[1].OperationID, "failover must share the initial request's operation")
}

func TestLoopOwnsTypedOverflowRecoveryAfterCompactionAndBeforeCandidateDispatch(t *testing.T) {
	var requestBodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requestBodies = append(requestBodies, request.Messages[len(request.Messages)-1].Content)
		if len(requestBodies) == 1 {
			http.Error(w, "context_length_exceeded", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"recovered\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	registry := llm.NewProviderRegistry()
	require.NoError(t, registry.Register(llm.NewEinoProvider(
		llm.WithProviderName("core-overflow-test"),
		llm.WithHTTPClient(server.Client()),
	)))
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID: "core-overflow-test",
		ModelID:    "core-overflow-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()
	model = llm.NewOverflowRecoveryMiddleware(llm.WithOverflowMaxRetries(3)).WrapModel(model)
	guarded := attemptGuardedModel{BaseChatModel: model}

	recorder := &attemptResultRecorder{}
	loop := NewLoopAgent(
		WithLLM(guarded),
		WithSystemPrompt("sys"),
		WithMaxIterations(1),
		WithProviderAttemptRecorder(recorder.record),
	)
	duplicateRecoveryDenied := false
	compactor := overflowSummaryCompactor{onCompact: func(ctx context.Context, _ []compaction.TurnItem) {
		authority, ok := llm.OperationDispatchAuthorityFromContext(ctx)
		require.True(t, ok)
		owner, ok := authority.(llm.OverflowRecoveryAuthority)
		require.True(t, ok)
		results := recorder.snapshot()
		require.Len(t, results, 1, "the actual overflow attempt must finish before compaction starts")
		require.Equal(t, llm.FailureOverflow, results[0].Failure)
		duplicateRecoveryDenied = owner.BeginOverflowRecovery(ctx, results[0].AttemptID) != nil
	}}
	loop.SetMidTurnCompaction(nil, compactor, compaction.NewHeuristicTokenEstimator(), 3)

	input := strings.Repeat("long request context ", 300)
	events := &collectingEventStream{}
	result, err := loop.RunCanonical(context.Background(), Submission{Content: input}, events)
	require.NoError(t, err)
	require.NotEmpty(t, result.Transcript)
	require.Equal(t, "recovered", result.Transcript[len(result.Transcript)-1].Message.Content)
	require.True(t, duplicateRecoveryDenied, "the one-recovery bit must be consumed before compaction begins")
	require.Len(t, requestBodies, 2, "the wrapper must not retry; Core may dispatch only after compaction")
	require.Equal(t, input, requestBodies[0])
	require.NotEqual(t, input, requestBodies[1], "the recovery dispatch must use the compacted request")

	results := recorder.snapshot()
	require.Len(t, results, 2)
	require.Equal(t, llm.FailureOverflow, results[0].Failure)
	require.Equal(t, llm.AttemptSuccess, results[1].Outcome)
	require.Equal(t, results[0].OperationID, results[1].OperationID)
	require.Equal(t, results[0].Identity.Candidate, results[1].Identity.Candidate)

	var incremental []AgentEvent
	for _, event := range events.collected() {
		if event.Kind == "message" && event.Incremental {
			incremental = append(incremental, event)
		}
	}
	require.Len(t, incremental, 1, "the failed partial candidate must not have been published")
	require.Equal(t, "recovered", incremental[0].Content)
}

func TestLoopSharesDispatchBudgetAcrossRetryFailoverAndOverflow(t *testing.T) {
	var requestModels []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requestModels = append(requestModels, request.Model)
		if request.Model == "shared-budget-primary" {
			http.Error(w, "temporary provider failure", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "context_length_exceeded", http.StatusBadRequest)
	}))
	defer server.Close()

	registry := llm.NewProviderRegistry()
	for _, provider := range []string{"shared-budget-primary-provider", "shared-budget-fallback-provider"} {
		require.NoError(t, registry.Register(llm.NewEinoProvider(
			llm.WithProviderName(provider),
			llm.WithHTTPClient(server.Client()),
		)))
	}
	resolver := registry.SnapshotResolver()
	primary, err := resolver.ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID: "shared-budget-primary-provider",
		ModelID:    "shared-budget-primary",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	fallback, err := resolver.ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID: "shared-budget-fallback-provider",
		ModelID:    "shared-budget-fallback",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	primaryModel, primaryCleanup, err := primary.Build(context.Background())
	require.NoError(t, err)
	defer primaryCleanup()
	fallbackModel, fallbackCleanup, err := fallback.Build(context.Background())
	require.NoError(t, err)
	defer fallbackCleanup()

	policy := &operationControlledRetryPolicy{allowedRetries: 1}
	model := llm.NewFailoverModelMiddleware(llm.WithFallbackModels(fallbackModel)).WrapModel(
		llm.NewRetryModelMiddleware(llm.WithRetryPolicy(policy)).WrapModel(primaryModel),
	)
	model = llm.NewOverflowRecoveryMiddleware().WrapModel(model)
	recorder := &attemptResultRecorder{}
	var compactionCalls int
	compactor := overflowSummaryCompactor{onCompact: func(context.Context, []compaction.TurnItem) {
		compactionCalls++
	}}
	loop := NewLoopAgent(
		WithLLM(attemptGuardedModel{BaseChatModel: model}),
		WithSystemPrompt("system"),
		WithMaxIterations(1),
		WithProviderAttemptRecorder(recorder.record),
		WithProviderRetryPolicy(policy),
	)
	loop.SetMidTurnCompaction(nil, compactor, compaction.NewHeuristicTokenEstimator(), 3)

	_, err = loop.Run(context.Background(), Submission{Content: strings.Repeat("long request context ", 300)})
	require.Error(t, err)
	require.Equal(t, []string{"shared-budget-primary", "shared-budget-primary", "shared-budget-fallback"}, requestModels,
		"retry, candidate switch, and overflow must share one three-dispatch operation")
	require.Zero(t, compactionCalls,
		"an overflow received after the shared dispatch budget is exhausted must not compact before a denied fourth dispatch")

	results := recorder.snapshot()
	require.Len(t, results, defaultProviderOperationAttempts)
	operationID := results[0].OperationID
	seen := make(map[llm.ProviderAttemptID]struct{}, len(results))
	for _, result := range results {
		require.Equal(t, operationID, result.OperationID)
		require.NotEmpty(t, result.AttemptID)
		if _, duplicate := seen[result.AttemptID]; duplicate {
			t.Fatalf("attempt ID %q was recorded more than once", result.AttemptID)
		}
		seen[result.AttemptID] = struct{}{}
	}
	require.Equal(t, llm.FailureTransientProvider, results[0].Failure)
	require.Equal(t, llm.FailureTransientProvider, results[1].Failure)
	require.Equal(t, llm.FailureOverflow, results[2].Failure)
}
