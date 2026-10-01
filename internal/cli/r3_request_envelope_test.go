package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type r3EnvelopeFixtureTool struct{}

func (r3EnvelopeFixtureTool) Name() string { return "r3_envelope_fixture" }
func (r3EnvelopeFixtureTool) Description() string {
	return "An assembly request-envelope schema fixture."
}
func (r3EnvelopeFixtureTool) Parameters() any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (r3EnvelopeFixtureTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	return &tools.ToolResult{Output: "unused"}, nil
}

type r3RecordingTokenEstimator struct {
	inner  compaction.TokenEstimator
	mu     sync.Mutex
	inputs []string
}

func (estimator *r3RecordingTokenEstimator) Estimate(text string) (int, error) {
	estimator.mu.Lock()
	estimator.inputs = append(estimator.inputs, text)
	estimator.mu.Unlock()
	return estimator.inner.Estimate(text)
}

func (estimator *r3RecordingTokenEstimator) snapshot() []string {
	estimator.mu.Lock()
	defer estimator.mu.Unlock()
	return append([]string(nil), estimator.inputs...)
}

type r3PreparedPlanRecordingModel struct {
	llm.BaseChatModel
	mu    sync.Mutex
	plans []*llm.PreparedRequestPlan
}

func (model *r3PreparedPlanRecordingModel) ProviderCandidates() []llm.ResolvedCandidateHandle {
	return llm.ProviderCandidatesFromModel(model.BaseChatModel)
}

func (model *r3PreparedPlanRecordingModel) CandidateSelectionSource() llm.CandidateSelectionSource {
	source, _ := llm.CandidateSelectionSourceFromModel(model.BaseChatModel)
	return source
}

func (model *r3PreparedPlanRecordingModel) Stream(ctx context.Context, messages []llm.Message, options ...llm.Option) (<-chan llm.MessageChunk, error) {
	if plan, ok := llm.PreparedRequestPlanFromContext(ctx); ok {
		model.mu.Lock()
		model.plans = append(model.plans, plan)
		model.mu.Unlock()
	}
	return model.BaseChatModel.Stream(ctx, messages, options...)
}

func (model *r3PreparedPlanRecordingModel) planSnapshot() []*llm.PreparedRequestPlan {
	model.mu.Lock()
	defer model.mu.Unlock()
	return append([]*llm.PreparedRequestPlan(nil), model.plans...)
}

type r3EnvelopeServer struct {
	server   *httptest.Server
	requests atomic.Int32
	bodies   chan []byte
}

func newR3EnvelopeServer() *r3EnvelopeServer {
	result := &r3EnvelopeServer{bodies: make(chan []byte, 2)}
	result.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result.requests.Add(1)
		result.bodies <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"accepted\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	return result
}

func r3KnownCandidateModel(t *testing.T, ctx context.Context, endpoint string, inputLimit int) (llm.BaseChatModel, func()) {
	t.Helper()
	const providerID = "r3-assembly-envelope"
	const modelID = "assembly-envelope-model"
	registry := llm.NewProviderRegistry()
	require.NoError(t, registry.Register(llm.NewEinoProvider(
		llm.WithProviderName(providerID),
		llm.WithModels([]llm.ModelInfo{{Name: modelID, InputTokenLimit: inputLimit}}),
		llm.WithBaseURL(endpoint),
		llm.WithHTTPClient(http.DefaultClient),
	)))
	resolved, err := registry.SnapshotResolver().ResolveModel(ctx, llm.ModelRequest{
		ProviderID:   providerID,
		ModelID:      modelID,
		Endpoint:     endpoint,
		MaxTokens:    512,
		MaxTokensSet: true,
	})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(ctx)
	require.NoError(t, err)
	return model, cleanup
}

type r3AssemblyEnvelopeRun struct {
	assembly  *AgentAssembly
	estimator *r3RecordingTokenEstimator
	recorder  *r3PreparedPlanRecordingModel
	server    *r3EnvelopeServer
	err       error
}

func runR3AssemblyEnvelopeCase(t *testing.T, inputLimit, toolSearchThreshold int) r3AssemblyEnvelopeRun {
	t.Helper()
	ctx := context.Background()
	provider := newR3EnvelopeServer()
	t.Cleanup(provider.server.Close)

	cfg := newAssembleTestConfig(t)
	cfg.Provider.BaseURL = provider.server.URL
	cfg.Provider.Model = "assembly-39-envelope"
	cfg.Provider.MaxTokens = 512
	cfg.Agent.SystemPrompt = "s"
	assembly, err := AssembleAgent(ctx, cfg, "openai", cfg.Provider.Model, io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	require.Equal(t, 8000, assembly.MaxTokens, "the acceptance request must use default local C=8000 / safe=6400")

	definitions, err := assembly.ToolRegistry.List(ctx)
	require.NoError(t, err)
	require.Len(t, definitions, 38, "the stock Assembly registry baseline is stable for this acceptance fixture")
	require.NoError(t, assembly.ToolRegistry.Register(ctx, r3EnvelopeFixtureTool{}))
	definitions, err = assembly.ToolRegistry.List(ctx)
	require.NoError(t, err)
	require.Len(t, definitions, 39, "the request must use the complete assembled registry plus one explicit schema fixture")
	if toolSearchThreshold > 0 {
		assembly.LoopAgent.WithToolSearchThreshold(toolSearchThreshold)
	}

	var model llm.BaseChatModel
	var modelCleanup func()
	if inputLimit > 0 {
		model, modelCleanup = r3KnownCandidateModel(t, ctx, provider.server.URL, inputLimit)
	} else {
		model = assembly.Model
	}
	if modelCleanup != nil {
		t.Cleanup(modelCleanup)
	}
	recorder := &r3PreparedPlanRecordingModel{BaseChatModel: model}
	assembly.LoopAgent.SetModel(recorder)
	estimator := &r3RecordingTokenEstimator{inner: assembly.Estimator}
	assembly.LoopAgent.SetMidTurnCompaction(assembly.MidTurn, assembly.Compactor, estimator, assembly.MaxTokens)

	_, runErr := assembly.LoopAgent.RunCanonical(ctx, core.Submission{Content: "use the available tools only if needed"})
	return r3AssemblyEnvelopeRun{
		assembly: assembly, estimator: estimator, recorder: recorder, server: provider, err: runErr,
	}
}

func TestAssembledDefault30ToolRequestUsesLocalEnvelopeWhenProviderLimitIsUnknown(t *testing.T) {
	result := runR3AssemblyEnvelopeCase(t, 0, 0)
	require.NoError(t, result.err, "unknown provider capacity must still admit a request that fits the local message budget")
	require.EqualValues(t, 1, result.server.requests.Load())
	plan, body := r3OnlyPreparedPlanAndBody(t, result)
	options := plan.Options()
	require.Len(t, options.Tools, 30, "Assembly's default dynamic tool threshold must remain active")
	require.Equal(t, llm.EstimateUnknown, plan.Estimate().ProviderLimit.State)
	require.Equal(t, llm.EstimateUnknown, plan.Estimate().ProviderFitState)
	require.Equal(t, 6400, plan.Estimate().LocalMessageBudget)
	r3RequirePlanMatchesRequestBody(t, result.estimator, plan, body)
}

func TestAssembledFull39ToolRequestFitsKnownProviderLimitAfterEnvelopeTerms(t *testing.T) {
	result := runR3AssemblyEnvelopeCase(t, 12000, 1000)
	require.NoError(t, result.err, "39 tool schemas must fit a known P=12000 provider envelope without inflating local C")
	require.EqualValues(t, 1, result.server.requests.Load())
	plan, body := r3OnlyPreparedPlanAndBody(t, result)
	require.Len(t, plan.Options().Tools, 39, "the acceptance request must exercise all assembled tool schemas")
	require.Equal(t, 12000, plan.Estimate().RawInputLimit)
	require.Equal(t, 9600, plan.Estimate().ProviderEnvelopeBudget)
	require.LessOrEqual(t, plan.Estimate().AccountedMessages.Tokens, plan.Estimate().LocalMessageBudget)
	require.LessOrEqual(t, plan.Estimate().AccountedMessages.Tokens+plan.Estimate().ToolSchemas.Tokens+plan.Estimate().ProtocolFraming.Tokens, plan.Estimate().ProviderEnvelopeBudget)
	r3RequirePlanMatchesRequestBody(t, result.estimator, plan, body)
}

func TestAssembledFull39ToolRequestExceedsKnownProviderLimitBeforeDispatch(t *testing.T) {
	result := runR3AssemblyEnvelopeCase(t, 8000, 1000)
	require.ErrorIs(t, result.err, core.ErrRequestEnvelopeRejected)
	require.Zero(t, result.server.requests.Load(), "P=8000 cannot admit the full 39-tool envelope under the frozen output reserve")
	require.Empty(t, result.recorder.planSnapshot(), "a rejected candidate must not start provider streaming")
	select {
	case body := <-result.server.bodies:
		t.Fatalf("provider must receive no request body before rejection: %s", body)
	default:
	}
}

type r3CapturedRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Temperature *float64        `json:"temperature"`
	MaxTokens   *int            `json:"max_tokens"`
	Stop        []string        `json:"stop"`
	Tools       json.RawMessage `json:"tools"`
}

func r3OnlyPreparedPlanAndBody(t *testing.T, result r3AssemblyEnvelopeRun) (*llm.PreparedRequestPlan, r3CapturedRequest) {
	t.Helper()
	plans := result.recorder.planSnapshot()
	require.Len(t, plans, 1, "one immutable Loop plan must back the provider dispatch")
	var body r3CapturedRequest
	require.NoError(t, json.Unmarshal(<-result.server.bodies, &body))
	return plans[0], body
}

func r3RequirePlanMatchesRequestBody(t *testing.T, estimator *r3RecordingTokenEstimator, plan *llm.PreparedRequestPlan, body r3CapturedRequest) {
	t.Helper()
	messages := plan.Messages()
	require.Len(t, body.Messages, len(messages))
	for index, message := range messages {
		require.Equal(t, string(message.Role), body.Messages[index].Role)
		require.Equal(t, message.Content, body.Messages[index].Content)
	}
	options := plan.Options()
	require.NotNil(t, options.MaxTokens)
	require.Equal(t, options.MaxTokens, body.MaxTokens, "the frozen output option must be the one serialized to the provider")
	require.Equal(t, options.Temperature, body.Temperature, "the frozen temperature option must be the one serialized to the provider")
	require.Equal(t, options.StopStrings, body.Stop, "the frozen stop sequences must be the ones serialized to the provider")

	type functionSchema struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		Parameters  any    `json:"parameters"`
	}
	type protocolTool struct {
		Type     string         `json:"type"`
		Function functionSchema `json:"function"`
	}
	wireTools := make([]protocolTool, 0, len(options.Tools))
	for _, tool := range options.Tools {
		parameters := tool.Parameters
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		wireTools = append(wireTools, protocolTool{
			Type:     "function",
			Function: functionSchema{Name: tool.Name, Description: tool.Description, Parameters: parameters},
		})
	}
	expectedTools, err := json.Marshal(wireTools)
	require.NoError(t, err)
	var expectedDecoded, actualDecoded any
	require.NoError(t, json.Unmarshal(expectedTools, &expectedDecoded))
	require.NoError(t, json.Unmarshal(body.Tools, &actualDecoded))
	require.True(t, reflect.DeepEqual(expectedDecoded, actualDecoded), "the schemas estimated from the prepared plan must equal the exact provider body")
	schemaTokens, err := estimator.Estimate(string(body.Tools))
	require.NoError(t, err)
	require.Equal(t, schemaTokens, plan.Estimate().ToolSchemas.Tokens, "the final serialized schemas sent must be the schemas charged to the envelope")
	require.Contains(t, estimator.snapshot(), string(body.Tools), "the estimator must see the exact serialized schemas sent by the provider")
}
