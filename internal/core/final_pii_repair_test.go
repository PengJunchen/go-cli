package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

const syntheticDeniedCandidate = "private fixture: example@example.invalid"

type piiRepairGuardModel struct {
	llm.BaseChatModel
}

func (m piiRepairGuardModel) ProviderCandidates() []llm.ResolvedCandidateHandle {
	return llm.ProviderCandidatesFromModel(m.BaseChatModel)
}
func (m piiRepairGuardModel) CandidateSelectionSource() llm.CandidateSelectionSource {
	source, _ := llm.CandidateSelectionSourceFromModel(m.BaseChatModel)
	return source
}
func (piiRepairGuardModel) MaxCandidateBytes() int { return llm.GuardedStreamMaxBytes }
func (piiRepairGuardModel) CheckCandidate(_ context.Context, candidate string) (*CandidateOutputResult, error) {
	if strings.Contains(candidate, syntheticDeniedCandidate) {
		return &CandidateOutputResult{RejectionCode: CandidateOutputRejectionPII}, nil
	}
	return &CandidateOutputResult{Allowed: true}, nil
}

func TestFinalPIIRepairAdmitsOneSafeResponse(t *testing.T) {
	var mu sync.Mutex
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		mu.Lock()
		requests = append(requests, request)
		index := len(requests)
		mu.Unlock()
		answer := "safe aggregate status"
		if index == 1 {
			answer = syntheticDeniedCandidate
		}
		payload, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": answer}}}})
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSERecoveryFrame(t, w, string(payload))
		writeSSERecoveryFrame(t, w, `{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`)
		writeSSERecoveryFrame(t, w, "[DONE]")
	}))
	defer server.Close()
	model, cleanup := sseRecoveryModel(t, server)
	defer cleanup()
	recorder := &attemptResultRecorder{}
	exporter := &captureExporter{}
	loop := NewLoopAgent(WithLLM(piiRepairGuardModel{model}), WithMaxIterations(1), WithTracer(tracing.NewTracer("synthetic-repair", exporter)), WithProviderAttemptRecorder(recorder.record), WithFinalPIIGuardRepairGate(func(context.Context) bool { return true }))
	loop.SetMidTurnCompaction(nil, nil, compaction.NewHeuristicTokenEstimator(), 50000)
	events := &collectingEventStream{}
	result, err := loop.RunCanonical(context.Background(), Submission{Content: "produce a status"}, events)
	require.NoError(t, err)
	mu.Lock()
	captured := append([]map[string]any(nil), requests...)
	mu.Unlock()
	require.Len(t, captured, 2)
	first, second := captured[0]["messages"].([]any), captured[1]["messages"].([]any)
	require.Len(t, second, len(first)+1)
	require.Equal(t, first, second[:len(first)])
	require.Equal(t, "user", second[len(second)-1].(map[string]any)["role"])
	require.NotContains(t, fmt.Sprint(second), syntheticDeniedCandidate)
	require.NotContains(t, fmt.Sprint(result.Transcript), syntheticDeniedCandidate)
	require.NotContains(t, fmt.Sprint(events.collected()), syntheticDeniedCandidate)
	require.Equal(t, "safe aggregate status", result.Transcript[len(result.Transcript)-1].Message.Content)
	attempts := recorder.snapshot()
	require.Len(t, attempts, 2)
	require.NotEqual(t, attempts[0].AttemptID, attempts[1].AttemptID)
	for _, attempt := range attempts {
		require.Equal(t, llm.UsageKnown, attempt.Usage.State)
		require.Equal(t, 7, attempt.Usage.Usage.InputTokens)
	}
	span := waitForSpan(t, exporter, "core.output_guard.repair")
	code, ok := attrValue(span.Attributes, "code")
	require.True(t, ok)
	require.Equal(t, "pii", code)
	resultTag, ok := attrValue(span.Attributes, "result")
	require.True(t, ok)
	require.Equal(t, "accepted", resultTag)
	require.NotContains(t, fmt.Sprint(span), syntheticDeniedCandidate)
}

type piiRepairFixtureResponse struct {
	text         string
	finish       string
	tool         bool
	status       int
	unknownUsage bool
}

type piiRepairTestFixture struct {
	mu       sync.Mutex
	requests []map[string]any
	server   *httptest.Server
}

func newPIIRepairTestFixture(t *testing.T, responses []piiRepairFixtureResponse) (*piiRepairTestFixture, llm.BaseChatModel) {
	t.Helper()
	fixture := &piiRepairTestFixture{}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode synthetic request: %v", err)
			return
		}
		fixture.mu.Lock()
		fixture.requests = append(fixture.requests, request)
		index := len(fixture.requests) - 1
		fixture.mu.Unlock()
		if index >= len(responses) {
			http.Error(w, "unexpected request", http.StatusInternalServerError)
			return
		}
		response := responses[index]
		if response.status != 0 {
			http.Error(w, "synthetic provider error", response.status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if response.text != "" {
			payload, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": response.text}}}})
			writeSSERecoveryFrame(t, w, string(payload))
		}
		if response.tool {
			writeSSERecoveryFrame(t, w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"synthetic-call","type":"function","function":{"name":"run_once","arguments":"{}"}}]}}]}`)
		}
		finish := response.finish
		if finish == "" {
			finish = "stop"
		}
		frame := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": finish}}}
		if !response.unknownUsage {
			frame["usage"] = map[string]any{"prompt_tokens": 7, "completion_tokens": 2, "total_tokens": 9}
		}
		payload, _ := json.Marshal(frame)
		writeSSERecoveryFrame(t, w, string(payload))
		writeSSERecoveryFrame(t, w, "[DONE]")
	}))
	model, cleanup := sseRecoveryModel(t, fixture.server)
	t.Cleanup(func() { cleanup(); fixture.server.Close() })
	return fixture, model
}

func (fixture *piiRepairTestFixture) count() int {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return len(fixture.requests)
}

func TestFinalPIIRepairFailsClosedAtEachBoundary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		responses []piiRepairFixtureResponse
		gate      bool
		unknown   bool
		wantCalls int
	}{
		{name: "disabled", responses: []piiRepairFixtureResponse{{text: syntheticDeniedCandidate}}, wantCalls: 1},
		{name: "unknown denial", responses: []piiRepairFixtureResponse{{text: syntheticDeniedCandidate}}, gate: true, unknown: true, wantCalls: 1},
		{name: "first candidate contains tool", responses: []piiRepairFixtureResponse{{text: syntheticDeniedCandidate, tool: true, finish: "tool_calls"}}, gate: true, wantCalls: 1},
		{name: "second denial", responses: []piiRepairFixtureResponse{{text: syntheticDeniedCandidate}, {text: syntheticDeniedCandidate}}, gate: true, wantCalls: 2},
		{name: "second candidate contains tool", responses: []piiRepairFixtureResponse{{text: syntheticDeniedCandidate}, {text: "safe but has a tool", tool: true, finish: "tool_calls"}}, gate: true, wantCalls: 2},
		{name: "second candidate truncated", responses: []piiRepairFixtureResponse{{text: syntheticDeniedCandidate}, {text: "incomplete", finish: "length"}}, gate: true, wantCalls: 2},
		{name: "second provider fails", responses: []piiRepairFixtureResponse{{text: syntheticDeniedCandidate}, {status: http.StatusServiceUnavailable}}, gate: true, wantCalls: 2},
		{name: "continuation is not repaired", responses: []piiRepairFixtureResponse{{text: "partial ", finish: "length"}, {text: syntheticDeniedCandidate}}, gate: true, wantCalls: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture, model := newPIIRepairTestFixture(t, tc.responses)
			guard := &configurablePIIRepairGuard{BaseChatModel: model, unknown: tc.unknown}
			tool := &sseRecoveryActionTool{}
			registry := tools.NewDefaultToolRegistry()
			require.NoError(t, registry.Register(context.Background(), tool))
			policy := &operationControlledRetryPolicy{allowedRetries: 10}
			attempts := &attemptResultRecorder{}
			loop := NewLoopAgent(WithLLM(guard), WithTools(registry), WithMaxIterations(2), WithProviderAttemptRecorder(attempts.record), WithProviderRetryPolicy(policy), WithFinalPIIGuardRepairGate(func(context.Context) bool { return tc.gate }))
			loop.SetMidTurnCompaction(nil, nil, compaction.NewHeuristicTokenEstimator(), 50000)
			events := &collectingEventStream{}
			result, err := loop.RunCanonical(context.Background(), Submission{Content: "produce status"}, events)
			require.Error(t, err)
			require.Equal(t, tc.wantCalls, fixture.count())
			require.Zero(t, tool.calls.Load())
			require.Empty(t, policy.calls(), "a denied candidate and repair failure never schedule a provider retry")
			require.NotContains(t, err.Error(), syntheticDeniedCandidate)
			require.NotContains(t, fmt.Sprint(events.collected()), syntheticDeniedCandidate)
			require.NotContains(t, fmt.Sprint(result.Transcript), syntheticDeniedCandidate)
		})
	}
}

type configurablePIIRepairGuard struct {
	llm.BaseChatModel
	unknown   bool
	guardErr  error
	nilResult bool
	sanitized string
}

func (m *configurablePIIRepairGuard) ProviderCandidates() []llm.ResolvedCandidateHandle {
	return llm.ProviderCandidatesFromModel(m.BaseChatModel)
}
func (m *configurablePIIRepairGuard) CandidateSelectionSource() llm.CandidateSelectionSource {
	source, _ := llm.CandidateSelectionSourceFromModel(m.BaseChatModel)
	return source
}
func (*configurablePIIRepairGuard) MaxCandidateBytes() int { return llm.GuardedStreamMaxBytes }
func (m *configurablePIIRepairGuard) CheckCandidate(_ context.Context, text string) (*CandidateOutputResult, error) {
	if strings.Contains(text, syntheticDeniedCandidate) {
		if m.guardErr != nil {
			return nil, m.guardErr
		}
		if m.nilResult {
			return nil, nil
		}
		if m.sanitized != "" {
			return &CandidateOutputResult{Sanitized: m.sanitized, RejectionCode: CandidateOutputRejectionPII}, nil
		}
		if m.unknown {
			return &CandidateOutputResult{}, nil
		}
		return &CandidateOutputResult{RejectionCode: CandidateOutputRejectionPII}, nil
	}
	return &CandidateOutputResult{Allowed: true}, nil
}

func TestFinalPIIRepairPreservesCompletedToolAndAttemptAccounting(t *testing.T) {
	fixture, model := newPIIRepairTestFixture(t, []piiRepairFixtureResponse{{tool: true, finish: "tool_calls"}, {text: syntheticDeniedCandidate}, {text: "safe status"}})
	tool := &sseRecoveryActionTool{}
	registry := tools.NewDefaultToolRegistry()
	require.NoError(t, registry.Register(context.Background(), tool))
	attempts := &attemptResultRecorder{}
	loop := NewLoopAgent(WithLLM(&configurablePIIRepairGuard{BaseChatModel: model}), WithTools(registry), WithMaxIterations(2), WithProviderAttemptRecorder(attempts.record), WithFinalPIIGuardRepairGate(func(context.Context) bool { return true }))
	loop.SetMidTurnCompaction(nil, nil, compaction.NewHeuristicTokenEstimator(), 50000)
	result, err := loop.RunCanonical(context.Background(), Submission{Content: "run once then report"})
	require.NoError(t, err)
	require.Equal(t, 3, fixture.count())
	require.EqualValues(t, 1, tool.calls.Load())
	require.Len(t, attempts.snapshot(), 3)
	require.Equal(t, "safe status", result.Transcript[len(result.Transcript)-1].Message.Content)
	require.NotContains(t, fmt.Sprint(result.Transcript), syntheticDeniedCandidate)
	fixture.mu.Lock()
	second, third := fixture.requests[1]["messages"].([]any), fixture.requests[2]["messages"].([]any)
	fixture.mu.Unlock()
	require.Equal(t, second, third[:len(second)], "the completed tool group must not be replayed or rewritten")
}

func TestFinalPIIRepairStopsBeforeDispatchWhenFeedbackCannotFit(t *testing.T) {
	fixture, model := newPIIRepairTestFixture(t, []piiRepairFixtureResponse{{text: syntheticDeniedCandidate}})
	compactor := &runCommitCountingCompactor{}
	attempts := &attemptResultRecorder{}
	loop := NewLoopAgent(WithLLM(&configurablePIIRepairGuard{BaseChatModel: model}), WithSystemPrompt("s"), WithMaxIterations(1), WithProviderAttemptRecorder(attempts.record), WithFinalPIIGuardRepairGate(func(context.Context) bool { return true }))
	loop.SetMidTurnCompaction(nil, compactor, durableRuneCountEstimator{}, 100)
	result, err := loop.RunCanonical(context.Background(), Submission{Content: "status"})
	require.ErrorIs(t, err, ErrRequestEnvelopeRejected)
	require.Equal(t, 1, fixture.count())
	require.Zero(t, compactor.calls, "repair admission must not compact or summarize")
	require.Len(t, attempts.snapshot(), 1)
	require.NotContains(t, fmt.Sprint(result.Transcript), syntheticDeniedCandidate)
}

func TestFinalPIIRepairHonorsCancellationAndConflictingControl(t *testing.T) {
	for _, tc := range []struct {
		name               string
		cancel             bool
		conflictingControl bool
	}{
		{name: "cancellation", cancel: true},
		{name: "existing edit control", conflictingControl: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture, model := newPIIRepairTestFixture(t, []piiRepairFixtureResponse{{text: syntheticDeniedCandidate}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.conflictingControl {
				ctx = context.WithValue(ctx, editLoopRecoveryControlKey{}, llm.Message{Role: llm.RoleSystem, Content: "existing recovery"})
			}
			gateCalls := 0
			loop := NewLoopAgent(WithLLM(&configurablePIIRepairGuard{BaseChatModel: model}), WithMaxIterations(1), WithFinalPIIGuardRepairGate(func(context.Context) bool {
				gateCalls++
				if tc.cancel {
					cancel()
				}
				return true
			}))
			loop.SetMidTurnCompaction(nil, nil, compaction.NewHeuristicTokenEstimator(), 50000)
			_, err := loop.RunCanonical(ctx, Submission{Content: "status"})
			require.Error(t, err)
			if tc.cancel {
				require.ErrorIs(t, err, context.Canceled)
			}
			require.Equal(t, 1, gateCalls)
			require.Equal(t, 1, fixture.count())
		})
	}
}

func TestFinalPIIRepairDoesNotEnableLegacyModel(t *testing.T) {
	model := &recordingCompleteGuardModel{
		sequenceStreamModel: &sequenceStreamModel{responses: [][]llm.MessageChunk{{{Content: syntheticDeniedCandidate}, {Final: true, FinishReason: "stop"}}}},
		result:              &CandidateOutputResult{RejectionCode: CandidateOutputRejectionPII},
	}
	gateCalls := 0
	loop := NewLoopAgent(WithLLM(model), WithMaxIterations(1), WithFinalPIIGuardRepairGate(func(context.Context) bool { gateCalls++; return true }))
	_, err := loop.RunCanonical(context.Background(), Submission{Content: "status"})
	require.Error(t, err)
	require.Equal(t, 1, model.calls)
	require.Zero(t, gateCalls)
}

func TestFinalPIIRepairAttemptRecorderFailureNeverPublishes(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			fixture, model := newPIIRepairTestFixture(t, []piiRepairFixtureResponse{{text: syntheticDeniedCandidate}, {text: "safe status"}})
			calls := 0
			loop := NewLoopAgent(WithLLM(&configurablePIIRepairGuard{BaseChatModel: model}), WithMaxIterations(1), WithProviderAttemptRecorder(func(llm.ProviderAttemptResult) error {
				calls++
				if calls == failAt {
					return errors.New("synthetic recorder failure")
				}
				return nil
			}), WithFinalPIIGuardRepairGate(func(context.Context) bool { return true }))
			loop.SetMidTurnCompaction(nil, nil, compaction.NewHeuristicTokenEstimator(), 50000)
			events := &collectingEventStream{}
			result, err := loop.RunCanonical(context.Background(), Submission{Content: "status"}, events)
			require.ErrorContains(t, err, "synthetic recorder failure")
			require.Equal(t, failAt, fixture.count())
			require.Equal(t, failAt, calls)
			require.NotContains(t, fmt.Sprint(events.collected()), syntheticDeniedCandidate)
			require.NotContains(t, fmt.Sprint(events.collected()), "safe status")
			require.NotContains(t, fmt.Sprint(result.Transcript), syntheticDeniedCandidate)
			require.NotContains(t, fmt.Sprint(result.Transcript), "safe status")
		})
	}
}

func TestFinalPIIRepairPreservesUnknownAttemptUsage(t *testing.T) {
	fixture, model := newPIIRepairTestFixture(t, []piiRepairFixtureResponse{{text: syntheticDeniedCandidate, unknownUsage: true}, {text: "safe status"}})
	attempts := &attemptResultRecorder{}
	loop := NewLoopAgent(WithLLM(&configurablePIIRepairGuard{BaseChatModel: model}), WithMaxIterations(1), WithProviderAttemptRecorder(attempts.record), WithFinalPIIGuardRepairGate(func(context.Context) bool { return true }))
	loop.SetMidTurnCompaction(nil, nil, compaction.NewHeuristicTokenEstimator(), 50000)
	_, err := loop.RunCanonical(context.Background(), Submission{Content: "status"})
	require.NoError(t, err)
	require.Equal(t, 2, fixture.count())
	results := attempts.snapshot()
	require.Len(t, results, 2)
	require.Equal(t, llm.UsageUnknown, results[0].Usage.State)
	require.Nil(t, results[0].Usage.Usage)
	require.Equal(t, llm.UsageKnown, results[1].Usage.State)
	require.NotEqual(t, results[0].OperationID, results[1].OperationID)
	require.NotEqual(t, results[0].AttemptID, results[1].AttemptID)
}

func TestFinalPIIRepairDoesNotSwitchCyclerCandidate(t *testing.T) {
	fixture, _ := newPIIRepairTestFixture(t, []piiRepairFixtureResponse{{text: syntheticDeniedCandidate}})
	registry := llm.NewProviderRegistry()
	require.NoError(t, registry.Register(llm.NewEinoProvider(llm.WithProviderName("pii-cycler-fixture"), llm.WithHTTPClient(fixture.server.Client()))))
	resolver := registry.SnapshotResolver()
	first, err := resolver.ResolveModel(context.Background(), llm.ModelRequest{ProviderID: "pii-cycler-fixture", ModelID: "first", Endpoint: fixture.server.URL})
	require.NoError(t, err)
	second, err := resolver.ResolveModel(context.Background(), llm.ModelRequest{ProviderID: "pii-cycler-fixture", ModelID: "second", Endpoint: fixture.server.URL})
	require.NoError(t, err)
	primary, cleanup, err := first.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()
	cycler := llm.NewModelCycler(llm.ModelCyclerConfig{Models: []llm.ModelEntry{{Provider: "pii-cycler-fixture", Model: "first"}, {Provider: "pii-cycler-fixture", Model: "second"}}, Strategy: llm.StrategyRoundRobin}).WithResolvedModels([]llm.ResolvedModel{first, second})
	guard := &configurablePIIRepairGuard{BaseChatModel: cycler.WrapModel(primary)}
	loop := NewLoopAgent(WithLLM(guard), WithMaxIterations(1), WithFinalPIIGuardRepairGate(func(context.Context) bool { return true }))
	loop.SetMidTurnCompaction(nil, nil, compaction.NewHeuristicTokenEstimator(), 50000)
	_, err = loop.RunCanonical(context.Background(), Submission{Content: "status"})
	require.ErrorContains(t, err, "final PII repair candidate changed")
	require.Equal(t, 1, fixture.count())
}

func TestFinalPIIRepairPreservesOtherGuardOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		guardErr  error
		nilResult bool
		sanitized string
		success   bool
	}{
		{name: "guard error", guardErr: errors.New("synthetic guard failure")},
		{name: "nil guard result", nilResult: true},
		{name: "existing sanitized result", sanitized: "filtered status", success: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture, model := newPIIRepairTestFixture(t, []piiRepairFixtureResponse{{text: syntheticDeniedCandidate}})
			guard := &configurablePIIRepairGuard{BaseChatModel: model, guardErr: tc.guardErr, nilResult: tc.nilResult, sanitized: tc.sanitized}
			gateCalls := 0
			loop := NewLoopAgent(WithLLM(guard), WithMaxIterations(1), WithFinalPIIGuardRepairGate(func(context.Context) bool { gateCalls++; return true }))
			loop.SetMidTurnCompaction(nil, nil, compaction.NewHeuristicTokenEstimator(), 50000)
			result, err := loop.RunCanonical(context.Background(), Submission{Content: "status"})
			if tc.success {
				require.NoError(t, err)
				require.Equal(t, tc.sanitized, result.Transcript[len(result.Transcript)-1].Message.Content)
			} else {
				require.Error(t, err)
			}
			require.Equal(t, 1, fixture.count())
			require.Zero(t, gateCalls)
			require.NotContains(t, fmt.Sprint(result.Transcript), syntheticDeniedCandidate)
		})
	}
}

func TestFinalPIIRepairRechecksRevokedGeneration(t *testing.T) {
	fixture, model := newPIIRepairTestFixture(t, []piiRepairFixtureResponse{{text: syntheticDeniedCandidate}})
	revokedErr := errors.New("synthetic generation revoked")
	revoked := false
	ctx := WithRunCapabilityGuard(context.Background(), providerGenerationAdmissionGuard{check: func() error {
		if revoked {
			return revokedErr
		}
		return nil
	}})
	loop := NewLoopAgent(WithLLM(&configurablePIIRepairGuard{BaseChatModel: model}), WithMaxIterations(1), WithFinalPIIGuardRepairGate(func(context.Context) bool { revoked = true; return true }))
	loop.SetMidTurnCompaction(nil, nil, compaction.NewHeuristicTokenEstimator(), 50000)
	_, err := loop.RunCanonical(ctx, Submission{Content: "status"})
	require.ErrorIs(t, err, revokedErr)
	require.Equal(t, 1, fixture.count())
}

func TestFinalPIIRepairBudgetFailureNeverCommitsProjection(t *testing.T) {
	fixture, baseModel := newPIIRepairTestFixture(t, []piiRepairFixtureResponse{{text: syntheticDeniedCandidate}})
	model := &configurablePIIRepairGuard{BaseChatModel: baseModel}
	const runID = "synthetic-repair-run"
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: runID}, nil)
	sink := &compactionCommitCountingSink{}
	input := &AgentMessage{Role: "user", Content: "status", InputKind: InputKindUser}
	consumer := &runCommitConsumer{ctx: owner.ctx, owner: owner, sink: sink, source: &CompactionSourceSnapshot{BranchID: "main"}, runID: runID, initialInput: input}
	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "s", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceEphemeral}}},
		{Role: llm.RoleUser, Content: input.Content, Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunTranscript, RunID: runID}}},
	}
	ctx := llm.WithRunOperationFactory(owner.ctx, &providerOperationFactory{})
	compactor := &runCommitCountingCompactor{}
	loop := NewLoopAgent(WithFinalPIIGuardRepairGate(func(context.Context) bool { return true }))
	_, after, _, err := loop.generateWithPreparedContinuation(ctx, model, messages, 1, nil, nil, slog.Default(), consumer, nil, compactor, durableRuneCountEstimator{}, 100, 0)
	require.ErrorIs(t, err, ErrRequestEnvelopeRejected)
	require.Equal(t, messages, after)
	require.Equal(t, 1, fixture.count())
	require.Zero(t, compactor.calls)
	require.Zero(t, sink.commits)
	require.Empty(t, consumer.projections)
}
