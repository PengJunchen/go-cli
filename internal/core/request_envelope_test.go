package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
)

type requestEnvelopePlanRecordingModel struct {
	llm.BaseChatModel
	plan *llm.PreparedRequestPlan
}

func (model *requestEnvelopePlanRecordingModel) ProviderCandidates() []llm.ResolvedCandidateHandle {
	return llm.ProviderCandidatesFromModel(model.BaseChatModel)
}

func (model *requestEnvelopePlanRecordingModel) CandidateSelectionSource() llm.CandidateSelectionSource {
	source, _ := llm.CandidateSelectionSourceFromModel(model.BaseChatModel)
	return source
}

func (model *requestEnvelopePlanRecordingModel) Stream(ctx context.Context, messages []llm.Message, options ...llm.Option) (<-chan llm.MessageChunk, error) {
	model.plan, _ = llm.PreparedRequestPlanFromContext(ctx)
	return model.BaseChatModel.Stream(ctx, messages, options...)
}

func TestEstimateProviderMessagesAccountsTextOnlyContentBlocks(t *testing.T) {
	text, multimodal, err := estimateProviderMessages([]llm.Message{{
		Role:          llm.RoleUser,
		ContentBlocks: []llm.ContentBlock{{Type: "text", Text: "caption"}},
	}}, compaction.NewHeuristicTokenEstimator(), 0)
	require.NoError(t, err)
	require.Greater(t, text, 0)
	require.Equal(t, llm.EstimateKnown, multimodal.State)
	require.Zero(t, multimodal.Tokens)
	accounted, known, err := accountedMessageEstimate(text, multimodal)
	require.NoError(t, err)
	require.True(t, known)
	require.Equal(t, text, accounted)
}

func TestEstimateProviderMessagesKeepsUnknownMultimodalVisibleAndAppliesFiniteReserve(t *testing.T) {
	message := llm.Message{
		Role: llm.RoleUser,
		ContentBlocks: []llm.ContentBlock{
			{Type: "text", Text: "inspect"},
			{Type: "image_url", ImageURL: &llm.ImageURL{URL: "https://example.invalid/a.png"}},
			{Type: "audio", Text: "unsupported payload"},
		},
	}
	estimator := compaction.NewHeuristicTokenEstimator()
	text, unknown, err := estimateProviderMessages([]llm.Message{message}, estimator, 0)
	require.NoError(t, err)
	require.Equal(t, llm.EstimateUnknown, unknown.State)
	require.Zero(t, unknown.Tokens)
	require.Contains(t, unknown.Reason, "no finite conservative reserve")
	_, accountedKnown, err := accountedMessageEstimate(text, unknown)
	require.NoError(t, err)
	require.False(t, accountedKnown, "unknown payload must not silently count as zero")

	textWithReserve, reserved, err := estimateProviderMessages([]llm.Message{message}, estimator, 256)
	require.NoError(t, err)
	require.Equal(t, llm.EstimateUnknown, reserved.State, "a reserve is not a provider-aware estimate")
	require.Equal(t, 512, reserved.Tokens, "the configured reserve must be applied once per unknown multimodal block")
	require.Contains(t, reserved.Reason, "finite configured per-item reserve is included")
	accounted, accountedKnown, err := accountedMessageEstimate(textWithReserve, reserved)
	require.NoError(t, err)
	require.True(t, accountedKnown, "a finite reserve supplies a bounded local amount while provider fit remains unknown")
	require.Equal(t, textWithReserve+512, accounted)
}

func TestEstimateProviderMessagesRejectsInvalidMultimodalReserves(t *testing.T) {
	message := llm.Message{Role: llm.RoleUser, ContentBlocks: []llm.ContentBlock{
		{Type: "image_url", ImageURL: &llm.ImageURL{URL: "https://example.invalid/a.png"}},
		{Type: "image_url", ImageURL: &llm.ImageURL{URL: "https://example.invalid/b.png"}},
	}}
	_, _, err := estimateProviderMessages([]llm.Message{message}, compaction.NewHeuristicTokenEstimator(), -1)
	require.ErrorContains(t, err, "must not be negative")
	_, _, err = estimateProviderMessages([]llm.Message{message}, compaction.NewHeuristicTokenEstimator(), math.MaxInt)
	require.ErrorContains(t, err, "multimodal reserve overflow")
}

func TestRequestEnvelopeUsesCandidateInputCapacityAndFrozenOutputReserve(t *testing.T) {
	tests := []struct {
		name           string
		info           llm.ModelInfo
		requestMax     *int
		wantRawInput   int
		wantSafeInput  int
		wantEffectiveO *int
		wantLimitKnown bool
	}{
		{
			name:       "input token limit takes precedence over context window and output reserve",
			info:       llm.ModelInfo{Name: "input-priority", InputTokenLimit: 1000, ContextWindow: 5000, MaxOutputTokens: 900},
			requestMax: r3TokenPointer(100), wantRawInput: 1000, wantSafeInput: 800,
			wantEffectiveO: r3TokenPointer(100), wantLimitKnown: true,
		},
		{
			name:       "context window subtracts clamped effective output reserve",
			info:       llm.ModelInfo{Name: "context-fallback", ContextWindow: 1000, MaxOutputTokens: 200},
			requestMax: r3TokenPointer(500), wantRawInput: 800, wantSafeInput: 640,
			wantEffectiveO: r3TokenPointer(200), wantLimitKnown: true,
		},
		{
			name:       "explicit zero remains the frozen output option",
			info:       llm.ModelInfo{Name: "explicit-zero", ContextWindow: 1000, MaxOutputTokens: 200},
			requestMax: r3TokenPointer(0), wantRawInput: 1000, wantSafeInput: 800,
			wantEffectiveO: r3TokenPointer(0), wantLimitKnown: true,
		},
		{
			name:           "context window without any output reserve remains unknown",
			info:           llm.ModelInfo{Name: "unknown-output", ContextWindow: 1000},
			wantLimitKnown: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := requestEnvelopeCandidate(t, test.info)
			requested, err := llm.MaterializeGenerationOptions()
			require.NoError(t, err)
			if test.requestMax != nil {
				requested, err = llm.MaterializeGenerationOptions(llm.WithMaxTokens(*test.requestMax))
				require.NoError(t, err)
			}
			messages := []llm.Message{{Role: llm.RoleUser, Content: "input"}}
			plan, err := prepareRequestPlan(context.Background(), llm.CandidateProposal{
				OperationID: llm.ProviderOperationID(test.name),
				Candidate:   candidate,
				Reason:      llm.CandidateInitial,
				Antecedent:  llm.CandidateAntecedent{Kind: llm.AntecedentInitial},
			}, messages, preparedRequestBase{requested: requested},
				compaction.NewHeuristicTokenEstimator(), 8000)
			require.NoError(t, err)
			options := plan.Options()
			if test.wantEffectiveO == nil {
				require.Nil(t, options.MaxTokens)
			} else {
				require.NotNil(t, options.MaxTokens)
				require.Equal(t, *test.wantEffectiveO, *options.MaxTokens)
			}
			estimate := plan.Estimate()
			if !test.wantLimitKnown {
				require.Equal(t, llm.EstimateUnknown, estimate.ProviderLimit.State)
				require.Equal(t, llm.EstimateUnknown, estimate.ProviderFitState)
				return
			}
			require.Equal(t, test.wantRawInput, estimate.RawInputLimit)
			require.Equal(t, test.wantSafeInput, estimate.SafeInputLimit)
			require.Equal(t, llm.EstimateKnown, estimate.ProviderLimit.State)
			require.Equal(t, llm.EstimateUnknown, estimate.ProviderFitState,
				"a local estimate never proves that the provider will accept the request")
		})
	}
}

func r3TokenPointer(value int) *int { return &value }

func requestEnvelopeCandidate(t *testing.T, info llm.ModelInfo) llm.ResolvedCandidateHandle {
	t.Helper()
	const providerID = "core-request-envelope-metadata-test"
	registry := llm.NewProviderRegistry()
	require.NoError(t, registry.Register(llm.NewEinoProvider(
		llm.WithProviderName(providerID),
		llm.WithModels([]llm.ModelInfo{info}),
	)))
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID: providerID,
		ModelID:    info.Name,
		Endpoint:   "https://example.invalid",
	})
	require.NoError(t, err)
	return resolved.CandidateHandle()
}

func TestUnknownMultimodalInputIsRejectedBeforeProviderDispatchWithoutReserve(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "unexpected"}}},
		})
	}))
	defer server.Close()

	registry := llm.NewProviderRegistry()
	require.NoError(t, registry.Register(llm.NewEinoProvider(llm.WithProviderName("core-multimodal-envelope-test"), llm.WithHTTPClient(server.Client()))))
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID: "core-multimodal-envelope-test",
		ModelID:    "core-multimodal-envelope-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()

	loop := NewLoopAgent(WithLLM(model), WithMaxIterations(1))
	loop.SetMidTurnCompaction(nil, nil, compaction.NewHeuristicTokenEstimator(), 50000)
	_, err = loop.Run(context.Background(), Submission{
		Content: "current request",
		History: []AgentMessage{{Role: string(llm.RoleUser), ContentBlocks: []llm.ContentBlock{
			{Type: "image_url", ImageURL: &llm.ImageURL{URL: "https://example.invalid/image.png"}},
		}}},
	})
	require.ErrorIs(t, err, ErrRequestEnvelopeRejected)
	require.Zero(t, requests.Load(), "unknown multimodal size must stop before the first provider dispatch")
}

func TestFiniteMultimodalReserveIsBoundedInPreparedLoopPlan(t *testing.T) {
	requestBodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestBody, _ := io.ReadAll(r.Body)
		requestBodies <- requestBody
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "accepted"}}},
		})
	}))
	defer server.Close()

	registry := llm.NewProviderRegistry()
	require.NoError(t, registry.Register(llm.NewEinoProvider(
		llm.WithProviderName("core-multimodal-reserve-test"),
		llm.WithModels([]llm.ModelInfo{{Name: "core-multimodal-reserve-model", InputTokenLimit: 50000}}),
		llm.WithHTTPClient(server.Client()),
	)))
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID: "core-multimodal-reserve-test",
		ModelID:    "core-multimodal-reserve-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()

	recorder := &requestEnvelopePlanRecordingModel{BaseChatModel: model}
	loop := NewLoopAgent(WithLLM(recorder), WithMaxIterations(1), WithMultimodalTokenReserve(256))
	loop.SetMidTurnCompaction(nil, nil, compaction.NewHeuristicTokenEstimator(), 50000)
	_, err = loop.Run(context.Background(), Submission{
		Content: "current request",
		History: []AgentMessage{{Role: string(llm.RoleUser), ContentBlocks: []llm.ContentBlock{
			{Type: "image_url", ImageURL: &llm.ImageURL{URL: "https://example.invalid/image-a.png"}},
			{Type: "image_url", ImageURL: &llm.ImageURL{URL: "https://example.invalid/image-b.png"}},
		}}},
	})
	require.NoError(t, err)
	require.NotNil(t, recorder.plan)
	require.Equal(t, llm.EstimateUnknown, recorder.plan.Estimate().Multimodal.State,
		"the reserve must not be reported as provider-aware multimodal usage")
	require.Equal(t, 512, recorder.plan.Estimate().Multimodal.Tokens)
	require.Equal(t, llm.EstimateKnown, recorder.plan.Estimate().AccountedMessages.State)
	require.Equal(t, llm.EstimateUnknown, recorder.plan.Estimate().ProviderFitState)
	requestBody := <-requestBodies
	var body struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(requestBody, &body))
	require.NotEmpty(t, body.Messages)
	var imageBlocks []llm.ContentBlock
	require.NoError(t, json.Unmarshal(body.Messages[1].Content, &imageBlocks))
	require.Len(t, imageBlocks, 2, "the prepared plan and actual request should include both image parts")
	require.Greater(t, len(requestBody), 0)
}

func TestCompactPreparedMessagesReservesProviderMessageMetadata(t *testing.T) {
	messages := []llm.Message{{Role: llm.RoleSystem, Content: "rules"}}
	for index := 0; index < 100; index++ {
		messages = append(messages, llm.Message{Role: llm.RoleUser, Content: "short user input"})
	}
	messages = append(messages, llm.Message{Role: llm.RoleUser, Content: "current request"})
	estimator := compaction.NewHeuristicTokenEstimator()
	const budget = 400
	compacted, current, err := compactPreparedMessages(context.Background(), messages, nil, nil,
		compaction.NewTruncatingCompactor(), estimator, budget, 0, len(messages)-1)
	require.NoError(t, err)
	require.Equal(t, messages[len(messages)-1], compacted[current], "current user input must survive unchanged")
	tokens, _, err := estimateProviderMessages(compacted, estimator, 0)
	require.NoError(t, err)
	require.LessOrEqual(t, tokens, budget, "compaction must leave room for per-message provider metadata")
}

func TestPreparedCompactionDropsOversizedOldMetadataAtExactBoundary(t *testing.T) {
	system := llm.Message{Role: llm.RoleSystem, Content: "rules"}
	droppedCall := llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
		ID: "drop-call", Name: "write", Args: map[string]any{"path": "old"},
	}}}
	droppedReceipt := llm.Message{Role: llm.RoleTool, Name: "write", ToolCallID: "drop-call", Content: "old result",
		Runtime: llm.RuntimeMetadata{ReceiptOutcome: "success"}}
	keptCall := llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
		ID: "keep-call", Name: "write", Args: map[string]any{"path": "current"},
	}}}
	keptReceipt := llm.Message{Role: llm.RoleTool, Name: "write", ToolCallID: "keep-call", Content: "kept result",
		Runtime: llm.RuntimeMetadata{ReceiptOutcome: "success"}}
	current := llm.Message{Role: llm.RoleUser, Content: "current input"}
	messages := []llm.Message{system, droppedCall, droppedReceipt, keptCall, keptReceipt, current}
	before := make([]llm.Message, len(messages))
	for i := range messages {
		before[i] = cloneLLMMessage(messages[i])
	}
	estimator := oneTokenPerNonEmptyText{}
	const budget = 12

	for _, test := range []struct {
		name    string
		compact func() ([]llm.Message, int, error)
	}{
		{
			name: "prepared request",
			compact: func() ([]llm.Message, int, error) {
				return compactPreparedMessages(context.Background(), messages, nil, nil,
					compaction.NewTruncatingCompactor(), estimator, budget, 0, len(messages)-1)
			},
		},
		{
			name: "provider overflow recovery",
			compact: func() ([]llm.Message, int, error) {
				return compactPreparedOverflowRequest(context.Background(), messages, len(messages)-1, nil,
					compaction.NewTruncatingCompactor(), estimator, budget)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			compacted, currentIndex, err := test.compact()
			require.NoError(t, err)
			require.Equal(t, len(compacted)-1, currentIndex)
			require.Equal(t, []llm.Message{system, keptCall, keptReceipt, current}, compacted,
				"keep the exact-fit latest tool-call group and current input, while dropping old metadata")
			tokens, _, err := estimateProviderMessages(compacted, estimator, 0)
			require.NoError(t, err)
			require.Equal(t, budget, tokens, "the retained provider-visible messages should fit exactly")
			require.Equal(t, before, messages, "compaction must not mutate the original request")
		})
	}
}

func TestPreparedCompactionMessageBudgetValidatesMetadataWithoutReservingIt(t *testing.T) {
	messages := []llm.Message{{
		Role: llm.RoleAssistant, Name: "named sender", ToolCallID: "receipt-id",
		Content:       "body must not count as metadata",
		ContentBlocks: []llm.ContentBlock{{Type: "image_url", ImageURL: &llm.ImageURL{URL: "https://example.invalid/image"}}},
		ToolCalls:     []llm.ToolCall{{ID: "call-id", Name: "tool-name", Args: map[string]any{"argument": "value"}}},
	}}
	before := cloneLLMMessage(messages[0])
	estimator := oneTokenPerNonEmptyText{}
	// The compactor counts provider-visible role/name/ID/argument metadata. Only
	// the separate multimodal reserve is removed from its budget, exactly once.
	budget, err := preparedCompactionMessageBudget(messages, estimator, 30, 7)
	require.NoError(t, err)
	require.Equal(t, 23, budget)
	require.Equal(t, before, messages[0])
	lessBudget, err := preparedCompactionMessageBudget(messages, estimator, 12, 7)
	require.NoError(t, err, "history metadata must not be pre-reserved before the compactor can remove it")
	require.Equal(t, 5, lessBudget, "the multimodal reserve must be subtracted once")
}

type requestMetadataEstimator struct {
	tokens int
	err    error
}

func (estimator requestMetadataEstimator) Estimate(string) (int, error) {
	return estimator.tokens, estimator.err
}

func TestPreparedCompactionMessageBudgetFailsClosedOnInvalidEstimates(t *testing.T) {
	failure := errors.New("estimator failed")
	for _, test := range []struct {
		name      string
		estimator compaction.TokenEstimator
	}{
		{"missing", nil},
		{"negative", requestMetadataEstimator{tokens: -1}},
		{"overflow", requestMetadataEstimator{tokens: math.MaxInt}},
		{"failed", requestMetadataEstimator{err: failure}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := preparedCompactionMessageBudget([]llm.Message{{Role: llm.RoleUser, Name: "sender"}}, test.estimator, 100, 0)
			require.ErrorIs(t, err, ErrRequestEnvelopeRejected)
			if test.name == "failed" {
				require.ErrorIs(t, err, failure)
			}
		})
	}
}
