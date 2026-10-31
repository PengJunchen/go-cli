package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type requestEnvelopeSummarySink struct {
	*consumerTestSink
	committed atomic.Bool
}

func (sink *requestEnvelopeSummarySink) CompactionSourceSnapshot() (CompactionSourceSnapshot, error) {
	return CompactionSourceSnapshot{
		Revision:       "request-envelope-revision",
		BranchID:       "main",
		Messages:       []AgentMessage{{Role: string(llm.RoleUser), Content: strings.Repeat("a", 152)}},
		SourceEntryIDs: []string{"request-envelope-prior-entry"},
	}, nil
}

func (sink *requestEnvelopeSummarySink) CommitCompaction(ctx context.Context, projection CompactionProjection) error {
	if err := sink.consumerTestSink.CommitCompaction(ctx, projection); err != nil {
		return err
	}
	sink.committed.Store(true)
	return nil
}

func TestDurableRequestEnvelopeCompactsWithSummaryBeforeFirstDispatch(t *testing.T) {
	sink := &requestEnvelopeSummarySink{consumerTestSink: &consumerTestSink{}}
	var requests atomic.Int32
	requestMessages := make(chan []string, 2)
	var projectionWasCommittedBeforeDispatch atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requests.Add(1)
		projectionWasCommittedBeforeDispatch.Store(sink.committed.Load())
		contents := make([]string, 0, len(request.Messages))
		for _, message := range request.Messages {
			contents = append(contents, message.Content)
		}
		requestMessages <- contents
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"summary accepted\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	registry := llm.NewProviderRegistry()
	require.NoError(t, registry.Register(llm.NewEinoProvider(
		llm.WithProviderName("core-request-envelope-summary-test"),
		llm.WithModels([]llm.ModelInfo{{Name: "core-request-envelope-summary-model", InputTokenLimit: 60}}),
		llm.WithHTTPClient(server.Client()),
	)))
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID: "core-request-envelope-summary-test",
		ModelID:    "core-request-envelope-summary-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	model = attemptGuardedModel{BaseChatModel: model}
	defer cleanup()
	var compactions atomic.Int32
	loop := NewLoopAgent(WithLLM(model), WithSystemPrompt("s"), WithMaxIterations(1))
	loop.SetMidTurnCompaction(nil, overflowSummaryCompactor{
		summary: "p",
		onCompact: func(context.Context, []compaction.TurnItem) {
			compactions.Add(1)
		},
	}, compaction.NewHeuristicTokenEstimator(), 70)
	agent := NewAgentImpl("request-envelope-durable-summary", loop)
	source, err := sink.CompactionSourceSnapshot()
	require.NoError(t, err)
	require.NoError(t, agent.SetHistoryFromSourceSnapshot(source))
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "request-envelope-summary-run"}, nil)
	owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{
		SessionID: "request-envelope-session", WorkspaceID: "request-envelope-workspace",
	})

	result, runErr := owner.run(time.Second, func(ctx context.Context) (Result, error) {
		return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "now"})
	})
	require.NoError(t, runErr, "valid durable summary compaction should let the same admitted request proceed")
	require.Equal(t, "summary accepted", result.Message)
	require.EqualValues(t, 1, compactions.Load(), "the request's full provider-visible message term must trigger compaction")
	require.EqualValues(t, 1, requests.Load())
	require.True(t, projectionWasCommittedBeforeDispatch.Load(), "the typed source projection must be committed before provider dispatch")
	dispatchedMessages := <-requestMessages
	require.Equal(t, "s", dispatchedMessages[0])
	require.Contains(t, dispatchedMessages[1], "p", "the prepared request must include the compacted summary")
	require.Equal(t, "now", dispatchedMessages[2], "the exact current submission must be retained")
	require.NotContains(t, strings.Join(dispatchedMessages, "\n"), strings.Repeat("a", 152), "the long published context must be summarized")
	steps, _, _, _ := sink.snapshot()
	require.Equal(t, []string{"input", "compaction", "terminal"}, steps)
}

type requestEnvelopeIndexSink struct {
	*consumerTestSink
	source CompactionSourceSnapshot
}

func (sink *requestEnvelopeIndexSink) CompactionSourceSnapshot() (CompactionSourceSnapshot, error) {
	return sink.source, nil
}

type requestEnvelopeIndexCompactor struct {
	inputs  [][]compaction.TurnItem
	results []compaction.CompactionResult
	budgets []int
}

func (recorder *requestEnvelopeIndexCompactor) Compact(ctx context.Context, items []compaction.TurnItem, budget int, estimator compaction.TokenEstimator) (compaction.CompactionResult, error) {
	result, err := compaction.NewTruncatingCompactor().Compact(ctx, items, budget, estimator)
	recorder.inputs = append(recorder.inputs, cloneCompactionTurnItems(items))
	recorder.results = append(recorder.results, result)
	recorder.budgets = append(recorder.budgets, budget)
	return result, err
}

func TestDurableRequestEnvelopeRebindsCurrentInputAfterInitialTruncation(t *testing.T) {
	const runID = "request-envelope-two-stage-run"
	const current = "current request with its exact durable identity"
	const callID = "closed-historical-call"
	ctx := t.Context()
	estimator := durableRuneCountEstimator{}
	sink := &requestEnvelopeIndexSink{
		consumerTestSink: &consumerTestSink{},
		source: CompactionSourceSnapshot{
			Revision: "two-stage-revision", BranchID: "main",
			Messages: []AgentMessage{
				{Role: "user"}, {Role: "assistant"}, {Role: "user"},
				{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: callID, Name: "historical_probe", Args: map[string]any{"path": "old.txt"}}}},
				{Role: "tool", Content: "closed historical receipt", ToolCallID: callID, ToolName: "historical_probe", ReceiptOutcome: ReceiptOutcomeSuccess},
			},
			SourceEntryIDs: []string{"old-one", "old-two", "padding", "historical-call", "historical-receipt"},
		},
	}
	fixtureMessages := func() []llm.Message {
		messages := []llm.Message{{Role: llm.RoleSystem, Content: "s", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceEphemeral}}}}
		for index, message := range sink.source.Messages {
			messages = append(messages, llm.Message{
				Role: llm.Role(message.Role), Content: message.Content, ToolCalls: message.ToolCalls, ToolCallID: message.ToolCallID, Name: message.ToolName,
				Runtime: llm.RuntimeMetadata{ReceiptOutcome: message.ReceiptOutcome, Source: llm.SourceRef{
					Kind: llm.SourcePriorPublished, HistoryRevision: sink.source.Revision, BranchID: sink.source.BranchID,
					SourceEntryID: sink.source.SourceEntryIDs[index], PriorOrdinal: uint64(index),
				}},
			})
		}
		return append(messages, llm.Message{Role: llm.RoleUser, Content: current, Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{
			Kind: llm.SourceRunTranscript, RunID: runID, PriorSourceCount: uint64(len(sink.source.SourceEntryIDs)), TranscriptIndex: 0,
		}}})
	}
	// Derive the budgets from actual provider-visible terms, including the
	// omission marker's untrusted-summary wrapper, rather than tuning a constant.
	items := messagesToTurnItems(fixtureMessages())
	marker := compaction.TurnItem{ID: "fixture-marker", Role: "user", Content: compaction.DurableTruncationMarker, IsCompaction: true,
		Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunSummary, RunID: runID, Through: llm.SourceThrough{PriorHistoryCount: 2}}}}
	minimal := append([]compaction.TurnItem{items[0], marker}, items[4:]...)
	minimalTokens := compaction.EstimateTokens(minimal, estimator)
	sink.source.Messages[2].Content = strings.Repeat("p", minimalTokens)
	items = messagesToTurnItems(fixtureMessages())
	firstTail := append([]compaction.TurnItem{items[0], marker}, items[3:]...)
	ceiling := compaction.EstimateTokens(firstTail, estimator)
	messageBudget := fourFifthsFloor(ceiling)
	sink.source.Messages[0].Content = strings.Repeat("x", ceiling+1)
	sink.source.Messages[1].Content = strings.Repeat("y", ceiling+1)
	items = messagesToTurnItems(fixtureMessages())
	require.NoError(t, validateDurableRequestSource(items, &sink.source, runID), "fixture source identity must be valid")
	require.Greater(t, compaction.EstimateTokens(items, estimator), ceiling)
	require.LessOrEqual(t, minimalTokens, messageBudget)
	first, err := compaction.NewTruncatingCompactor().Compact(ctx, cloneCompactionTurnItems(items), ceiling, estimator)
	require.NoError(t, err, "fixture's first real truncation must be feasible")
	require.NotNil(t, first.NewProjection)
	require.EqualValues(t, 2, first.NewProjection.Through.PriorHistoryCount, "two old source entries must become one marker")
	firstTokens := compaction.EstimateTokens(first.Items, estimator)
	require.LessOrEqual(t, firstTokens, ceiling)
	require.Greater(t, firstTokens, messageBudget)
	second, err := compaction.NewTruncatingCompactor().Compact(ctx, cloneCompactionTurnItems(first.Items), messageBudget, estimator)
	require.NoError(t, err, "fixture's second real truncation must retain the whole tool pair and current input")
	require.EqualValues(t, 3, second.NewProjection.Through.PriorHistoryCount)
	require.Equal(t, items[4:], second.Items[2:])
	require.LessOrEqual(t, compaction.EstimateTokens(second.Items, estimator), messageBudget)
	originalIndex := len(items) - 1
	require.GreaterOrEqual(t, originalIndex, len(first.Items), "the first real cut must actually invalidate the original index")
	providerTokens, multimodal, err := estimateProviderMessages(turnItemsToMessages(first.Items), estimator, 0)
	require.NoError(t, err)
	require.Equal(t, llm.EstimateKnown, multimodal.State)
	require.Equal(t, firstTokens, providerTokens, "envelope and compactor must price the same wrapped marker and tool metadata")
	t.Logf("fixture full=%d C=%d first=%d local_budget=%d minimal=%d old_index=%d first_current_index=%d", compaction.EstimateTokens(items, estimator), ceiling, firstTokens, messageBudget, minimalTokens, originalIndex, len(first.Items)-1)

	var requests, toolExecutions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"accepted\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	registry := llm.NewProviderRegistry()
	require.NoError(t, registry.Register(llm.NewEinoProvider(
		llm.WithProviderName("durable-two-stage-test"),
		llm.WithModels([]llm.ModelInfo{{Name: "durable-two-stage-model", InputTokenLimit: 1000000}}),
		llm.WithHTTPClient(server.Client()),
	)))
	resolved, err := registry.SnapshotResolver().ResolveModel(ctx, llm.ModelRequest{ProviderID: "durable-two-stage-test", ModelID: "durable-two-stage-model", Endpoint: server.URL})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(ctx)
	require.NoError(t, err)
	defer cleanup()
	planRecorder := &requestEnvelopePlanRecordingModel{BaseChatModel: model}
	toolRegistry := mock.NewMockToolServer()
	_, err = toolRegistry.RegisterMockTool("historical_probe", func(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
		toolExecutions.Add(1)
		return &tools.ToolResult{Output: "unexpected replay"}, nil
	})
	require.NoError(t, err)
	recorder := &requestEnvelopeIndexCompactor{}
	loop := NewLoopAgent(WithLLM(attemptGuardedModel{BaseChatModel: planRecorder}), WithTools(toolRegistry), WithSystemPrompt("s"), WithMaxIterations(1))
	loop.SetMidTurnCompaction(nil, recorder, estimator, ceiling)
	definitions, err := loop.buildToolDefinitions(ctx)
	require.NoError(t, err)
	candidates := llm.ProviderCandidatesFromModel(model)
	require.Len(t, candidates, 1)
	candidate := candidates[0]
	options, err := effectiveCandidateOptions(candidate, candidate.ModelSpec(), llm.GenerationOptions{Tools: definitions})
	require.NoError(t, err)
	firstEnvelope, err := estimateRequestEnvelope(ctx, candidate, candidate.ModelSpec(), turnItemsToMessages(first.Items), options, estimator, ceiling, 0)
	require.NoError(t, err)
	require.Equal(t, messageBudget, firstEnvelope.MessageBudget, "the active candidate must use the intended local 80-percent budget")
	require.False(t, envelopeEstimateFits(firstEnvelope))
	secondEnvelope, err := estimateRequestEnvelope(ctx, candidate, candidate.ModelSpec(), turnItemsToMessages(second.Items), options, estimator, ceiling, 0)
	require.NoError(t, err)
	require.True(t, envelopeEstimateFits(secondEnvelope), "the real candidate's full envelope must admit the second real truncation")
	agent := NewAgentImpl("durable-two-stage", loop)
	require.NoError(t, agent.SetHistoryFromSourceSnapshot(sink.source))
	owner := newRunOwner(ctx, nil, RunClaim{ID: runID}, nil)
	owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "two-stage-session", WorkspaceID: "two-stage-workspace"})
	result, runErr := owner.run(10*time.Second, func(runCtx context.Context) (Result, error) {
		return agent.Run(runCtx, Submission{Type: SubmissionUserMessage, Content: current})
	})
	steps, intents, receipts, terminal := sink.snapshot()
	require.NotEmpty(t, recorder.results, "the complete Loop must reach the real initial compactor")
	require.Equal(t, items, recorder.inputs[0])
	require.Equal(t, first.Items, recorder.results[0].Items)
	require.Equal(t, ceiling, recorder.budgets[0])
	sourceRefs := func(entries []compaction.TurnItem) []llm.SourceRef {
		refs := make([]llm.SourceRef, len(entries))
		for index, entry := range entries {
			refs[index] = entry.Runtime.Source
		}
		return refs
	}
	t.Logf("actual first input_sources=%+v output_sources=%+v", sourceRefs(recorder.inputs[0]), sourceRefs(recorder.results[0].Items))
	t.Logf("actual error_type=%T error=%v provider_requests=%d compactor_calls=%d steps=%v", runErr, runErr, requests.Load(), len(recorder.results), steps)
	require.Zero(t, toolExecutions.Load(), "a closed historical tool call must never replay")
	require.Empty(t, intents)
	require.Empty(t, receipts)
	require.Empty(t, sink.executionSnapshot())
	require.Empty(t, sink.resolutionSnapshot())
	require.NotNil(t, result.StatusHandle)
	require.Zero(t, result.StatusHandle.Snapshot().PendingWorkers)
	require.NotNil(t, terminal)
	if runErr != nil {
		var envelopeError *RequestEnvelopeError
		require.ErrorIs(t, runErr, ErrRequestEnvelopeRejected)
		require.ErrorAs(t, runErr, &envelopeError)
		require.Equal(t, "current submission cannot be preserved during compaction", envelopeError.Reason)
		require.Zero(t, requests.Load())
		require.Len(t, recorder.results, 1, "the stale-index guard precedes the second compactor call")
		require.Equal(t, []string{"input", "compaction", "terminal"}, steps)
		require.Equal(t, RunOutcomeError, terminal.Outcome)
	}
	require.NoError(t, runErr, "a feasible second truncation must rebind the exact durable current input before envelope admission")
	require.EqualValues(t, 1, requests.Load())
	require.Equal(t, "accepted", result.Message)
	require.Equal(t, RunOutcomeSuccess, terminal.Outcome)
	require.Len(t, recorder.results, 2)
	require.Equal(t, []string{"input", "compaction", "compaction", "terminal"}, steps)
	require.NotNil(t, planRecorder.plan)
	require.Equal(t, messageBudget, planRecorder.plan.Estimate().MessageBudget)
	require.Equal(t, turnItemsToMessages(second.Items), planRecorder.plan.Messages(), "the admitted plan preserves the entire tool pair and the exact current user's typed source identity once")
}
