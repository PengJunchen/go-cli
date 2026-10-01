package core

import (
	"context"
	"encoding/json"
	"errors"
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
	toolspkg "github.com/pengjunchen/go-cli/internal/tools"
)

type r3EnvelopeDispatch struct {
	plan     *llm.PreparedRequestPlan
	messages []llm.Message
	options  llm.GenerationOptions
}

type r3EnvelopeDispatchRecorder struct {
	llm.BaseChatModel
	mu         sync.Mutex
	dispatches []r3EnvelopeDispatch
}

func (model *r3EnvelopeDispatchRecorder) ProviderCandidates() []llm.ResolvedCandidateHandle {
	return llm.ProviderCandidatesFromModel(model.BaseChatModel)
}

func (model *r3EnvelopeDispatchRecorder) CandidateSelectionSource() llm.CandidateSelectionSource {
	source, _ := llm.CandidateSelectionSourceFromModel(model.BaseChatModel)
	return source
}

func (model *r3EnvelopeDispatchRecorder) Stream(ctx context.Context, messages []llm.Message, options ...llm.Option) (<-chan llm.MessageChunk, error) {
	plan, ok := llm.PreparedRequestPlanFromContext(ctx)
	if !ok {
		return nil, errors.New("r3 envelope dispatch recorder: Loop plan is missing")
	}
	materialized, err := llm.MaterializeGenerationOptions(options...)
	if err != nil {
		return nil, err
	}
	model.mu.Lock()
	model.dispatches = append(model.dispatches, r3EnvelopeDispatch{
		plan:     plan,
		messages: append([]llm.Message(nil), messages...),
		options:  materialized,
	})
	model.mu.Unlock()
	return model.BaseChatModel.Stream(ctx, messages, options...)
}

func (model *r3EnvelopeDispatchRecorder) snapshot() []r3EnvelopeDispatch {
	model.mu.Lock()
	defer model.mu.Unlock()
	return append([]r3EnvelopeDispatch(nil), model.dispatches...)
}

type r3EnvelopeTool struct{}

func (r3EnvelopeTool) Name() string { return "envelope_fixture" }
func (r3EnvelopeTool) Description() string {
	return "A schema-bearing tool for request-envelope tests."
}
func (r3EnvelopeTool) Execute(context.Context, toolspkg.ToolCall) (*toolspkg.ToolResult, error) {
	return &toolspkg.ToolResult{Output: "unused"}, nil
}
func (r3EnvelopeTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "Fixture path"},
		},
		"required": []string{"path"},
	}
}

func r3EnvelopeTools(t *testing.T) toolspkg.ToolRegistry {
	t.Helper()
	registry := toolspkg.NewDefaultToolRegistry()
	require.NoError(t, registry.Register(context.Background(), r3EnvelopeTool{}))
	return registry
}

type r3WireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type r3WireFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters"`
}

type r3WireTool struct {
	Type     string         `json:"type"`
	Function r3WireFunction `json:"function"`
}

type r3ProviderRequest struct {
	Model     string          `json:"model"`
	Messages  []r3WireMessage `json:"messages"`
	Tools     []r3WireTool    `json:"tools"`
	MaxTokens *int            `json:"max_tokens"`
}

type r3HTTPRecorder struct {
	mu       sync.Mutex
	requests []r3ProviderRequest
	respond  func(index int, model string) (int, string)
}

func (recorder *r3HTTPRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "could not read request", http.StatusBadRequest)
		return
	}
	var request r3ProviderRequest
	if err := json.Unmarshal(body, &request); err != nil {
		http.Error(w, "invalid request JSON", http.StatusBadRequest)
		return
	}
	recorder.mu.Lock()
	index := len(recorder.requests)
	recorder.requests = append(recorder.requests, request)
	recorder.mu.Unlock()
	if recorder.respond != nil {
		if status, message := recorder.respond(index, request.Model); status != 0 {
			http.Error(w, message, status)
			return
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"accepted\"}}]}\n\n")
	_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func (recorder *r3HTTPRecorder) snapshot() []r3ProviderRequest {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]r3ProviderRequest(nil), recorder.requests...)
}

func r3RegisterCandidate(
	t *testing.T,
	registry *llm.ProviderRegistry,
	providerID string,
	info llm.ModelInfo,
	serverURL string,
	maxTokens int,
) (llm.ResolvedModel, llm.BaseChatModel, func()) {
	t.Helper()
	require.NoError(t, registry.Register(llm.NewEinoProvider(
		llm.WithProviderName(providerID),
		llm.WithModels([]llm.ModelInfo{info}),
	)))
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID:   providerID,
		ModelID:      info.Name,
		Endpoint:     serverURL,
		MaxTokens:    maxTokens,
		MaxTokensSet: true,
	})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	return resolved, model, cleanup
}

func r3NewLoop(
	t *testing.T,
	model llm.BaseChatModel,
	recorder *attemptResultRecorder,
	estimator compaction.TokenEstimator,
	compactor compaction.Compactor,
	extra ...LoopOption,
) *LoopAgent {
	t.Helper()
	options := []LoopOption{
		WithLLM(model),
		WithTools(r3EnvelopeTools(t)),
		WithMaxIterations(1),
		WithProviderAttemptRecorder(recorder.record),
	}
	options = append(options, extra...)
	loop := NewLoopAgent(options...)
	loop.SetMidTurnCompaction(nil, compactor, estimator, 50000)
	return loop
}

func r3ExpectedWireMessages(messages []llm.Message) []r3WireMessage {
	result := make([]r3WireMessage, 0, len(messages))
	for _, message := range messages {
		result = append(result, r3WireMessage{Role: string(message.Role), Content: message.Content})
	}
	return result
}

func r3ExpectedWireTools(definitions []llm.ToolDefinition) []r3WireTool {
	result := make([]r3WireTool, 0, len(definitions))
	for _, definition := range definitions {
		parameters := definition.Parameters
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		result = append(result, r3WireTool{
			Type: "function",
			Function: r3WireFunction{
				Name: definition.Name, Description: definition.Description, Parameters: parameters,
			},
		})
	}
	return result
}

func r3AssertPlanTerms(t *testing.T, dispatch r3EnvelopeDispatch, ceiling int) {
	t.Helper()
	plan := dispatch.plan
	require.NotNil(t, plan)
	proposal := plan.Proposal()
	require.NotNil(t, proposal.Candidate)
	require.NotEmpty(t, proposal.OperationID)
	require.Equal(t, proposal.Candidate.CandidateID(), proposal.Candidate.Identity().Candidate)
	require.Equal(t, proposal.Candidate.Identity().ModelID, plan.Model().Info.Name)

	messages := plan.Messages()
	options := plan.Options()
	require.Equal(t, messages, dispatch.messages, "the plan's frozen messages must be the actual Stream input")
	require.Equal(t, options, dispatch.options, "the plan's effective options must be the actual Stream options")

	estimate := plan.Estimate()
	require.Equal(t, llm.EstimateKnown, estimate.ProviderLimit.State)
	require.Equal(t, plan.Model().Info.InputTokenLimit, estimate.RawInputLimit,
		"direct InputTokenLimit is P and must not be reduced by O")
	require.NotNil(t, options.MaxTokens)
	require.Equal(t, llm.EstimateUnknown, estimate.ProviderFitState,
		"local request accounting does not prove provider acceptance")
	require.Greater(t, estimate.ToolSchemas.Tokens, 0)
	require.Greater(t, estimate.ProtocolFraming.Tokens, 0)

	schemaJSON, err := json.Marshal(r3ExpectedWireTools(options.Tools))
	require.NoError(t, err)
	schemaTokens, err := compaction.NewHeuristicTokenEstimator().Estimate(string(schemaJSON))
	require.NoError(t, err)
	framingTokens, err := proposal.Candidate.EstimateProtocolFraming(messages, options)
	require.NoError(t, err)
	expectedEnvelope := schemaTokens + framingTokens
	require.Equal(t, schemaTokens, estimate.ToolSchemas.Tokens)
	require.Equal(t, framingTokens, estimate.ProtocolFraming.Tokens)
	require.Equal(t, expectedEnvelope, estimate.ToolSchemas.Tokens+estimate.ProtocolFraming.Tokens,
		"E must equal final tool-schema plus candidate-specific protocol framing estimates")
	require.Equal(t, fourFifthsFloor(plan.Model().Info.InputTokenLimit), estimate.ProviderEnvelopeBudget)
	require.Equal(t, min(fourFifthsFloor(ceiling), estimate.ProviderEnvelopeBudget-expectedEnvelope), estimate.MessageBudget)
	require.LessOrEqual(t, estimate.AccountedMessages.Tokens+expectedEnvelope, estimate.ProviderEnvelopeBudget)
}

func r3AssertBodyMatchesDispatch(t *testing.T, request r3ProviderRequest, dispatch r3EnvelopeDispatch, ceiling int) {
	t.Helper()
	r3AssertPlanTerms(t, dispatch, ceiling)
	plan := dispatch.plan
	options := plan.Options()
	require.Equal(t, plan.Model().Info.Name, request.Model)
	require.Equal(t, r3ExpectedWireMessages(plan.Messages()), request.Messages)
	require.Equal(t, r3ExpectedWireTools(options.Tools), request.Tools)
	require.Equal(t, options.MaxTokens, request.MaxTokens,
		"the effective candidate output cap must match the actual HTTP max_tokens")
}

func TestLoopRetrySuccessDispatchesFreshPreparedEnvelope(t *testing.T) {
	httpRecorder := &r3HTTPRecorder{respond: func(index int, _ string) (int, string) {
		if index == 0 {
			return http.StatusServiceUnavailable, "temporary provider failure"
		}
		return 0, ""
	}}
	server := httptest.NewServer(httpRecorder)
	defer server.Close()

	registry := llm.NewProviderRegistry()
	resolved, model, cleanup := r3RegisterCandidate(t, registry, "core-r3-retry", llm.ModelInfo{
		Name: "r3-retry-model", InputTokenLimit: 24000, MaxOutputTokens: 128,
	}, server.URL, 240)
	defer cleanup()

	dispatches := &r3EnvelopeDispatchRecorder{BaseChatModel: model}
	attempts := &attemptResultRecorder{}
	retryPolicy := &operationControlledRetryPolicy{allowedRetries: 1}
	loop := r3NewLoop(t, dispatches, attempts, compaction.NewHeuristicTokenEstimator(), nil,
		WithProviderRetryPolicy(retryPolicy))

	_, err := loop.Run(context.Background(), Submission{Content: "retry exact prepared request"})
	require.NoError(t, err)
	requests := httpRecorder.snapshot()
	plans := dispatches.snapshot()
	require.Len(t, requests, 2)
	require.Len(t, plans, 2)
	require.NotSame(t, plans[0].plan, plans[1].plan, "a retry must receive a fresh prepared plan")
	for index := range plans {
		r3AssertBodyMatchesDispatch(t, requests[index], plans[index], 50000)
		require.Equal(t, resolved.CandidateHandle().CandidateID(), plans[index].plan.Proposal().Candidate.CandidateID())
		require.Equal(t, 128, *plans[index].plan.Options().MaxTokens, "candidate output cap must clamp the resolver default")
	}
	require.Equal(t, plans[0].plan.Proposal().OperationID, plans[1].plan.Proposal().OperationID)
	results := attempts.snapshot()
	require.Len(t, results, 2)
	require.Equal(t, llm.FailureTransientProvider, results[0].Failure)
	require.Equal(t, llm.AttemptSuccess, results[1].Outcome)
	require.Equal(t, results[0].OperationID, results[1].OperationID)
	require.Equal(t, []int{0}, retryPolicy.calls())
}

func TestLoopFallbackSuccessDispatchesCandidateSpecificPreparedEnvelope(t *testing.T) {
	httpRecorder := &r3HTTPRecorder{respond: func(index int, _ string) (int, string) {
		if index == 0 {
			return http.StatusServiceUnavailable, "primary unavailable"
		}
		return 0, ""
	}}
	server := httptest.NewServer(httpRecorder)
	defer server.Close()

	registry := llm.NewProviderRegistry()
	selected, selectedModel, selectedCleanup := r3RegisterCandidate(t, registry, "core-r3-fallback-primary", llm.ModelInfo{
		Name: "r3-primary-model", InputTokenLimit: 24000, MaxOutputTokens: 120,
	}, server.URL, 200)
	defer selectedCleanup()
	fallback, fallbackModel, fallbackCleanup := r3RegisterCandidate(t, registry, "core-r3-fallback-secondary", llm.ModelInfo{
		Name: "r3-fallback-model", InputTokenLimit: 16000, MaxOutputTokens: 64,
	}, server.URL, 100)
	defer fallbackCleanup()

	failover := llm.NewFailoverModelMiddleware(llm.WithFallbackModels(fallbackModel))
	dispatches := &r3EnvelopeDispatchRecorder{BaseChatModel: failover.WrapModel(selectedModel)}
	attempts := &attemptResultRecorder{}
	loop := r3NewLoop(t, dispatches, attempts, compaction.NewHeuristicTokenEstimator(), nil)

	_, err := loop.Run(context.Background(), Submission{Content: "fallback exact prepared request"})
	require.NoError(t, err)
	requests := httpRecorder.snapshot()
	plans := dispatches.snapshot()
	require.Len(t, requests, 2)
	require.Len(t, plans, 2)
	require.NotSame(t, plans[0].plan, plans[1].plan, "fallback must receive a fresh candidate-bound plan")
	for index := range plans {
		r3AssertBodyMatchesDispatch(t, requests[index], plans[index], 50000)
	}
	require.Equal(t, selected.CandidateHandle().CandidateID(), plans[0].plan.Proposal().Candidate.CandidateID())
	require.Equal(t, fallback.CandidateHandle().CandidateID(), plans[1].plan.Proposal().Candidate.CandidateID())
	require.Equal(t, 120, *plans[0].plan.Options().MaxTokens)
	require.Equal(t, 64, *plans[1].plan.Options().MaxTokens,
		"fallback plan must use its own clamped output cap")
	require.Equal(t, plans[0].plan.Proposal().OperationID, plans[1].plan.Proposal().OperationID)
	require.Equal(t, "r3-primary-model", requests[0].Model)
	require.Equal(t, "r3-fallback-model", requests[1].Model)

	results := attempts.snapshot()
	require.Len(t, results, 2)
	require.Equal(t, llm.FailureTransientProvider, results[0].Failure)
	require.Equal(t, llm.AttemptSuccess, results[1].Outcome)
	require.Equal(t, selected.CandidateHandle().CandidateID(), results[0].Identity.Candidate)
	require.Equal(t, fallback.CandidateHandle().CandidateID(), results[1].Identity.Candidate)
	require.Equal(t, results[0].OperationID, results[1].OperationID)
}

func TestLoopOverflowSuccessDispatchesFreshCompactedPreparedEnvelope(t *testing.T) {
	httpRecorder := &r3HTTPRecorder{respond: func(index int, _ string) (int, string) {
		if index == 0 {
			return http.StatusBadRequest, "context_length_exceeded"
		}
		return 0, ""
	}}
	server := httptest.NewServer(httpRecorder)
	defer server.Close()

	registry := llm.NewProviderRegistry()
	resolved, model, cleanup := r3RegisterCandidate(t, registry, "core-r3-overflow", llm.ModelInfo{
		Name: "r3-overflow-model", InputTokenLimit: 50000, MaxOutputTokens: 96,
	}, server.URL, 180)
	defer cleanup()
	overflow := llm.NewOverflowRecoveryMiddleware(llm.WithOverflowMaxRetries(3)).WrapModel(model)
	dispatches := &r3EnvelopeDispatchRecorder{BaseChatModel: overflow}
	guarded := attemptGuardedModel{BaseChatModel: dispatches}
	attempts := &attemptResultRecorder{}
	loop := r3NewLoop(t, guarded, attempts, compaction.NewHeuristicTokenEstimator(), overflowSummaryCompactor{summary: "typed overflow summary"})
	input := strings.Repeat("current overflow request ", 120)

	_, err := loop.Run(context.Background(), Submission{Content: input})
	require.NoError(t, err)
	requests := httpRecorder.snapshot()
	plans := dispatches.snapshot()
	require.Len(t, requests, 2)
	require.Len(t, plans, 2)
	require.NotSame(t, plans[0].plan, plans[1].plan, "overflow recovery must re-estimate and dispatch a new plan")
	for index := range plans {
		r3AssertBodyMatchesDispatch(t, requests[index], plans[index], 50000)
		require.Equal(t, resolved.CandidateHandle().CandidateID(), plans[index].plan.Proposal().Candidate.CandidateID())
		require.Equal(t, 96, *plans[index].plan.Options().MaxTokens)
	}
	firstMessages := plans[0].plan.Messages()
	secondMessages := plans[1].plan.Messages()
	require.NotEqual(t, firstMessages, secondMessages, "overflow recovery must re-plan the compacted request")
	require.Contains(t, r3ExpectedWireMessages(secondMessages)[len(secondMessages)-1].Content, input,
		"the exact current submission must survive the compaction")
	require.Contains(t, requests[1].Messages[len(requests[1].Messages)-1].Content, input)
	require.Equal(t, plans[0].plan.Proposal().OperationID, plans[1].plan.Proposal().OperationID)
	results := attempts.snapshot()
	require.Len(t, results, 2)
	require.Equal(t, llm.FailureOverflow, results[0].Failure)
	require.Equal(t, llm.AttemptSuccess, results[1].Outcome)
	require.Equal(t, results[0].OperationID, results[1].OperationID)
	require.Equal(t, results[0].Identity.Candidate, results[1].Identity.Candidate)
}

type r3SelectionBarrierEstimator struct {
	inner   compaction.TokenEstimator
	arrived chan string
	release chan struct{}
	seen    sync.Map
}

func (estimator *r3SelectionBarrierEstimator) Estimate(text string) (int, error) {
	for _, marker := range []string{"stale-run-alpha", "stale-run-beta"} {
		if strings.Contains(text, marker) {
			if _, loaded := estimator.seen.LoadOrStore(marker, struct{}{}); !loaded {
				estimator.arrived <- marker
				<-estimator.release
			}
			break
		}
	}
	return estimator.inner.Estimate(text)
}

func TestLoopReplansWhenConcurrentCyclerSelectionLeaseGoesStale(t *testing.T) {
	httpRecorder := &r3HTTPRecorder{}
	server := httptest.NewServer(httpRecorder)
	defer server.Close()

	registry := llm.NewProviderRegistry()
	alpha, alphaModel, alphaCleanup := r3RegisterCandidate(t, registry, "core-r3-stale-alpha", llm.ModelInfo{
		Name: "r3-stale-alpha", InputTokenLimit: 24000, MaxOutputTokens: 128,
	}, server.URL, 200)
	defer alphaCleanup()
	beta, _, betaCleanup := r3RegisterCandidate(t, registry, "core-r3-stale-beta", llm.ModelInfo{
		Name: "r3-stale-beta", InputTokenLimit: 16000, MaxOutputTokens: 64,
	}, server.URL, 100)
	defer betaCleanup()

	cycler := llm.NewModelCycler(llm.ModelCyclerConfig{
		Models: []llm.ModelEntry{
			{Provider: "core-r3-stale-alpha", Model: "r3-stale-alpha"},
			{Provider: "core-r3-stale-beta", Model: "r3-stale-beta"},
		},
		Strategy: llm.StrategyRoundRobin,
	}).WithResolvedModels([]llm.ResolvedModel{alpha, beta})
	dispatches := &r3EnvelopeDispatchRecorder{BaseChatModel: cycler.WrapModel(alphaModel)}
	attempts := &attemptResultRecorder{}
	estimator := &r3SelectionBarrierEstimator{
		inner:   compaction.NewHeuristicTokenEstimator(),
		arrived: make(chan string, 2),
		release: make(chan struct{}),
	}
	firstLoop := r3NewLoop(t, dispatches, attempts, estimator, nil)
	secondLoop := r3NewLoop(t, dispatches, attempts, estimator, nil)

	type runResult struct {
		marker string
		err    error
	}
	done := make(chan runResult, 2)
	for _, marker := range []string{"stale-run-alpha", "stale-run-beta"} {
		marker := marker
		loop := firstLoop
		if marker == "stale-run-beta" {
			loop = secondLoop
		}
		go func() {
			_, err := loop.Run(context.Background(), Submission{Content: marker})
			done <- runResult{marker: marker, err: err}
		}()
	}

	for range 2 {
		select {
		case <-estimator.arrived:
		case <-time.After(10 * time.Second):
			close(estimator.release)
			t.Fatal("both Loop runs must reach estimation after their initial metadata-only candidate peek")
		}
	}
	close(estimator.release)
	for range 2 {
		select {
		case result := <-done:
			require.NoError(t, result.err, "run %s", result.marker)
		case <-time.After(20 * time.Second):
			t.Fatal("concurrent Loop run did not finish")
		}
	}

	requests := httpRecorder.snapshot()
	plans := dispatches.snapshot()
	results := attempts.snapshot()
	require.Len(t, requests, 2, "the stale lease must not cross the provider dispatch fence")
	require.Len(t, results, 2, "only physically dispatched calls receive provider attempt results")
	require.Len(t, plans, 3, "two initial leases and one stale-lease re-peek must be planned")
	require.Equal(t, 1, countR3RequestModel(requests, "r3-stale-alpha"))
	require.Equal(t, 1, countR3RequestModel(requests, "r3-stale-beta"))

	byOperation := make(map[llm.ProviderOperationID][]r3EnvelopeDispatch)
	for _, dispatch := range plans {
		r3AssertPlanTerms(t, dispatch, 50000)
		byOperation[dispatch.plan.Proposal().OperationID] = append(byOperation[dispatch.plan.Proposal().OperationID], dispatch)
	}
	require.Len(t, byOperation, 2)
	var staleOperation llm.ProviderOperationID
	var singlePlanOperation llm.ProviderOperationID
	for operationID, operationPlans := range byOperation {
		switch len(operationPlans) {
		case 1:
			singlePlanOperation = operationID
			require.Equal(t, "r3-stale-alpha", operationPlans[0].plan.Model().Info.Name)
		case 2:
			staleOperation = operationID
			stalePlan := operationPlans[0].plan
			staleProposal := stalePlan.Proposal()
			require.Equal(t, "r3-stale-alpha", stalePlan.Model().Info.Name)
			require.Equal(t, llm.CandidateCyclerSelected, staleProposal.Reason)
			require.Equal(t, llm.AntecedentInitial, staleProposal.Antecedent.Kind)
			require.Equal(t, "r3-stale-beta", operationPlans[1].plan.Model().Info.Name,
				"the stale operation must re-peek and re-estimate the next cycler candidate")
			replacementProposal := operationPlans[1].plan.Proposal()
			require.Equal(t, llm.CandidateCyclerSelected, replacementProposal.Reason)
			require.Equal(t, llm.AntecedentInitial, replacementProposal.Antecedent.Kind)
			require.NotSame(t, operationPlans[0].plan, operationPlans[1].plan)

			frozenOption, err := llm.OptionFromGenerationOptions(stalePlan.Options())
			require.NoError(t, err)
			require.ErrorIs(t, stalePlan.Consume(stalePlan.Proposal().Candidate, stalePlan.Messages(), frozenOption),
				llm.ErrPreparedRequestConsumed,
				"a lease rejected before dispatch must not leave its prepared plan reusable")
		default:
			t.Fatalf("operation %q produced %d prepared plans", operationID, len(operationPlans))
		}
	}
	require.NotEmpty(t, staleOperation)
	require.NotEmpty(t, singlePlanOperation)
	require.NotEqual(t, staleOperation, singlePlanOperation)

	resultByCandidate := make(map[llm.CandidateID]llm.ProviderAttemptResult)
	for _, result := range results {
		require.Equal(t, llm.AttemptSuccess, result.Outcome)
		resultByCandidate[result.Identity.Candidate] = result
	}
	require.Len(t, resultByCandidate, 2)
	require.Equal(t, singlePlanOperation, resultByCandidate[alpha.CandidateHandle().CandidateID()].OperationID)
	require.Equal(t, staleOperation, resultByCandidate[beta.CandidateHandle().CandidateID()].OperationID)

	for _, request := range requests {
		marker := ""
		for _, message := range request.Messages {
			if strings.Contains(message.Content, "stale-run-") {
				marker = message.Content
				break
			}
		}
		require.NotEmpty(t, marker)
		var matched *r3EnvelopeDispatch
		for index := range plans {
			dispatch := &plans[index]
			if dispatch.plan.Model().Info.Name != request.Model {
				continue
			}
			for _, message := range dispatch.plan.Messages() {
				if message.Content == marker {
					if matched != nil {
						t.Fatalf("multiple plans match body model %s and marker %s", request.Model, marker)
					}
					matched = dispatch
				}
			}
		}
		require.NotNil(t, matched)
		r3AssertBodyMatchesDispatch(t, request, *matched, 50000)
	}
}

func countR3RequestModel(requests []r3ProviderRequest, model string) int {
	count := 0
	for _, request := range requests {
		if request.Model == model {
			count++
		}
	}
	return count
}
