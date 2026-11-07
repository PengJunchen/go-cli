package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
)

type anchoredCurrentMemoryTransport struct {
	sink             *consumerTestSink
	calls            atomic.Int32
	beforeProjection atomic.Bool
}

type anchoredCurrentCountingModel struct {
	llm.BaseChatModel
	calls atomic.Int32
}

func (model *anchoredCurrentCountingModel) ProviderCandidates() []llm.ResolvedCandidateHandle {
	return llm.ProviderCandidatesFromModel(model.BaseChatModel)
}

func (model *anchoredCurrentCountingModel) CandidateSelectionSource() llm.CandidateSelectionSource {
	source, _ := llm.CandidateSelectionSourceFromModel(model.BaseChatModel)
	return source
}

func (model *anchoredCurrentCountingModel) Stream(ctx context.Context, messages []llm.Message, options ...llm.Option) (<-chan llm.MessageChunk, error) {
	model.calls.Add(1)
	return model.BaseChatModel.Stream(ctx, messages, options...)
}

type anchoredCurrentSink struct {
	*requestEnvelopeIndexSink
	failProjection bool
}

func (sink *anchoredCurrentSink) CommitCompaction(ctx context.Context, projection CompactionProjection) error {
	if sink.failProjection {
		return errors.New("injected projection sync failure")
	}
	return sink.requestEnvelopeIndexSink.CommitCompaction(ctx, projection)
}

type anchoredCancelAfterCompaction struct {
	compaction.Compactor
	cancel context.CancelFunc
}

type anchoredCurrentPreparedState struct {
	ctx           context.Context
	loop          *LoopAgent
	model         llm.BaseChatModel
	consumer      *runCommitConsumer
	after         []llm.Message
	inputIndex    int
	sink          *anchoredCurrentSink
	countingModel *anchoredCurrentCountingModel
	transport     *anchoredCurrentMemoryTransport
}

func (c anchoredCancelAfterCompaction) Compact(ctx context.Context, items []compaction.TurnItem, budget int, estimator compaction.TokenEstimator) (compaction.CompactionResult, error) {
	result, err := c.Compactor.Compact(ctx, items, budget, estimator)
	if err == nil {
		c.cancel()
	}
	return result, err
}

func (transport *anchoredCurrentMemoryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.calls.Add(1)
	steps, _, _, _ := transport.sink.snapshot()
	if len(steps) == 0 || steps[len(steps)-1] != "compaction" {
		transport.beforeProjection.Store(true)
	}
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
}

func TestAnchoredCurrentRunPreparedRequestCommitBeforeDispatch(t *testing.T) {
	runAnchoredCurrentPreparedFixture(t, false, false)
}

func TestAnchoredCurrentRunProjectionFailureStopsProvider(t *testing.T) {
	runAnchoredCurrentPreparedFixture(t, true, false)
}

func TestAnchoredCurrentRunCancellationStopsProvider(t *testing.T) {
	runAnchoredCurrentPreparedFixture(t, false, true)
}

func TestAnchoredCurrentRunRejectsChangedInputBeforeDispatch(t *testing.T) {
	runAnchoredCurrentPreparedFixture(t, false, false, func(state anchoredCurrentPreparedState) {
		mutations := []struct {
			name   string
			mutate func(*llm.Message)
		}{
			{name: "content", mutate: func(message *llm.Message) { message.Content += " changed" }},
			{name: "blocks", mutate: func(message *llm.Message) {
				message.ContentBlocks = []llm.ContentBlock{{Type: "text", Text: "changed"}}
			}},
			{name: "tool_calls", mutate: func(message *llm.Message) {
				message.ToolCalls = []llm.ToolCall{{ID: "forged-call", Name: "fixture_read", Args: map[string]any{"path": "changed"}}}
			}},
			{name: "tool_call_id", mutate: func(message *llm.Message) { message.ToolCallID = "forged-call" }},
			{name: "name", mutate: func(message *llm.Message) { message.Name = "forged-tool" }},
			{name: "finish_reason", mutate: func(message *llm.Message) { message.FinishReason = "stop" }},
			{name: "marker_hint", mutate: func(message *llm.Message) { message.Runtime.CompactionMarker = true }},
		}
		for _, mutation := range mutations {
			t.Run(mutation.name, func(t *testing.T) {
				changed := append([]llm.Message(nil), state.after...)
				changed[1] = cloneLLMMessage(changed[1])
				mutation.mutate(&changed[1])
				require.LessOrEqual(t, compaction.EstimateTokens(messagesToTurnItems(changed), compaction.NewHeuristicTokenEstimator()), 1024,
					"the changed request must fit without another compaction")

				response, _, _, err := state.loop.generateWithPreparedContinuation(
					state.ctx, state.model, changed, state.inputIndex, nil, nil, slog.Default(), state.consumer, nil,
					compaction.NewUnifiedCompactor(), compaction.NewHeuristicTokenEstimator(), 1024, 0,
				)
				require.ErrorIs(t, err, ErrDurableCompactionUnsupported)
				require.Nil(t, response)
				require.EqualValues(t, 1, state.countingModel.calls.Load(), "only the first, valid request may reach the provider")
				require.EqualValues(t, 1, state.transport.calls.Load())
				steps, _, _, _ := state.sink.snapshot()
				require.Equal(t, []string{"input", "compaction"}, steps, "a rejected retry must not append another projection")
			})
		}
	})
}

func runAnchoredCurrentPreparedFixture(t *testing.T, failProjection, cancelAfterCompaction bool, checks ...func(anchoredCurrentPreparedState)) {
	t.Helper()
	const runID = "synthetic-anchor-run"
	sink := &anchoredCurrentSink{
		requestEnvelopeIndexSink: &requestEnvelopeIndexSink{
			consumerTestSink: &consumerTestSink{},
			source:           CompactionSourceSnapshot{Revision: "fixture-revision", BranchID: "main"},
		},
		failProjection: failProjection,
	}
	transport := &anchoredCurrentMemoryTransport{sink: sink.consumerTestSink}
	registry := llm.NewProviderRegistry()
	require.NoError(t, registry.Register(llm.NewEinoProvider(
		llm.WithProviderName("anchor-fixture-provider"),
		llm.WithModels([]llm.ModelInfo{{Name: "anchor-fixture-model", InputTokenLimit: 1_000_000}}),
		llm.WithHTTPClient(&http.Client{Transport: transport}),
	)))
	resolved, err := registry.SnapshotResolver().ResolveModel(t.Context(), llm.ModelRequest{
		ProviderID: "anchor-fixture-provider", ModelID: "anchor-fixture-model", Endpoint: "https://fixture.invalid",
	})
	require.NoError(t, err)
	base, cleanup, err := resolved.Build(t.Context())
	require.NoError(t, err)
	defer cleanup()
	countingModel := &anchoredCurrentCountingModel{BaseChatModel: base}
	model := attemptGuardedModel{BaseChatModel: countingModel}
	loop := NewLoopAgent(WithLLM(model))
	owner := newRunOwner(t.Context(), nil, RunClaim{ID: runID}, nil)
	defer owner.cancel()
	ctx := withRunID(withRunWorkerTracker(owner.ctx, owner), runID)
	ctx = WithRunCommit(ctx, sink, RunCommitBinding{SessionID: "fixture-session", WorkspaceID: "fixture-workspace", OwnerGeneration: owner.ownerGeneration})
	ctx = llm.WithRunOperationFactory(ctx, &providerOperationFactory{})
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	consumer, err := newRunCommitConsumer(ctx)
	require.NoError(t, err)
	input := AgentMessage{Role: "user", Content: "original input", InputKind: InputKindUser}
	require.NoError(t, consumer.commitInput(0, input))

	oldCall := llm.ToolCall{ID: "old-call", Name: "fixture_read", Args: map[string]any{}}
	newCall := llm.ToolCall{ID: "new-call", Name: "fixture_read", Args: map[string]any{}}
	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "system", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceEphemeral}}},
		{Role: llm.RoleUser, Content: input.Content, Runtime: consumer.transcriptMetadata(0, "")},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{oldCall}, Runtime: consumer.transcriptMetadata(1, "")},
		{Role: llm.RoleTool, Name: oldCall.Name, ToolCallID: oldCall.ID, Content: strings.Repeat("synthetic data ", 2000), Runtime: consumer.transcriptMetadata(2, ReceiptOutcomeSuccess)},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{newCall}, Runtime: consumer.transcriptMetadata(3, "")},
		{Role: llm.RoleTool, Name: newCall.Name, ToolCallID: newCall.ID, Content: "recent", Runtime: consumer.transcriptMetadata(4, ReceiptOutcomeSuccess)},
	}
	items := messagesToTurnItems(messages)
	require.NoError(t, compaction.ValidateRunRequestIntegrity(items))
	require.NoError(t, compaction.ValidateCompactionEligible(items))
	require.NoError(t, validateDurableRequestSource(items, consumer.source, consumer.runID))
	var compactor compaction.Compactor = compaction.NewUnifiedCompactor()
	if cancelAfterCompaction {
		compactor = anchoredCancelAfterCompaction{Compactor: compactor, cancel: cancel}
	}
	response, after, inputIndex, generateErr := loop.generateWithPreparedContinuation(
		ctx, model, messages, 1, nil, nil, slog.Default(), consumer, nil,
		compactor, compaction.NewHeuristicTokenEstimator(), 1024, 0,
	)
	if failProjection || cancelAfterCompaction {
		require.Error(t, generateErr)
		require.Nil(t, response)
		require.Zero(t, countingModel.calls.Load())
		require.Zero(t, transport.calls.Load())
		steps, _, _, _ := sink.snapshot()
		require.Equal(t, []string{"input"}, steps)
		return
	}
	require.NoError(t, generateErr)
	require.NotNil(t, response)
	require.EqualValues(t, 1, transport.calls.Load())
	require.EqualValues(t, 1, countingModel.calls.Load())
	require.False(t, transport.beforeProjection.Load(), "a provider request requires the synced projection first")
	require.Equal(t, 1, inputIndex)
	require.Len(t, after, 5)
	require.Equal(t, messages[1], after[1])
	require.Equal(t, messages[4:], after[3:])
	steps, _, _, _ := sink.snapshot()
	require.Equal(t, []string{"input", "compaction"}, steps)
	for _, check := range checks {
		check(anchoredCurrentPreparedState{
			ctx: ctx, loop: loop, model: model, consumer: consumer, after: after, inputIndex: inputIndex,
			sink: sink, countingModel: countingModel, transport: transport,
		})
	}
}

func TestAnchoredCurrentRunLastUserQueryIgnoresMarker(t *testing.T) {
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: "real input", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunTranscript, RunID: "run", TranscriptIndex: 0}}},
		{Role: llm.RoleUser, Content: compaction.UntrustedSummaryContext(compaction.DurableTruncationMarker), Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunSummary, RunID: "run", Through: llm.SourceThrough{TranscriptCount: 3}}}},
	}
	require.Equal(t, "real input", lastUserQuery(messages))

	// A real user can type marker-like text; content alone must not classify it.
	realUserText := compaction.UntrustedSummaryContext("actual user text")
	messages = append(messages, llm.Message{Role: llm.RoleUser, Content: realUserText})
	require.Equal(t, realUserText, lastUserQuery(messages))
}

func TestAnchoredCurrentRunHistoricalMarkerCompatibility(t *testing.T) {
	wrapped := compaction.UntrustedSummaryContext("old summary body")
	priorSource := llm.RuntimeMetadata{Source: llm.SourceRef{
		Kind: llm.SourcePriorPublished, HistoryRevision: "revision", BranchID: "main",
		SourceEntryID: "historical-summary", PriorOrdinal: 0,
	}}

	t.Run("wrapper is presentation only", func(t *testing.T) {
		input := llm.Message{Role: llm.RoleUser, Content: wrapped, Runtime: priorSource}
		items := messagesToTurnItems([]llm.Message{input})
		require.Len(t, items, 1)
		require.True(t, items[0].IsCompaction, "the legacy summary wrapper remains a presentation classification")
		require.Equal(t, "old summary body", items[0].Content, "the wrapper is unwrapped once for the compactor")

		restored := turnItemsToMessages(items)
		require.Equal(t, input.Content, restored[0].Content, "the wrapper is restored exactly once")
		require.Equal(t, priorSource, restored[0].Runtime, "presentation classification does not mint trusted runtime authority")
		require.Equal(t, input.Content, lastUserQuery(restored), "an unmarked historical wrapper remains a real user query")
	})

	t.Run("trusted prior marker still skips query", func(t *testing.T) {
		trustedSource := priorSource
		trustedSource.CompactionMarker = true
		agent := NewAgentImpl("historical-marker-test", durableCompactionAgentLoop{})
		require.NoError(t, agent.SetHistoryFromSourceSnapshot(CompactionSourceSnapshot{
			Revision: "revision", BranchID: "main",
			Messages: []AgentMessage{
				{Role: "user", Content: "real prior question"},
				{Role: "user", Content: wrapped, Runtime: trustedSource},
			},
			SourceEntryIDs: []string{"real-user", "historical-summary"},
		}))
		items := agentMessagesToTurnItems(agent.Messages())
		require.True(t, items[1].IsCompaction)
		restored := turnItemsToMessages(items)
		require.True(t, restored[1].Runtime.CompactionMarker, "the trusted snapshot must preserve its marker")
		require.Equal(t, "real prior question", lastUserQuery(restored))
	})

	t.Run("SetHistory drops a supplied marker", func(t *testing.T) {
		forgedSource := priorSource
		forgedSource.CompactionMarker = true
		agent := NewAgentImpl("historical-marker-test", durableCompactionAgentLoop{})
		agent.SetHistory([]AgentMessage{{Role: "user", Content: wrapped, Runtime: forgedSource}})
		history := agent.Messages()
		require.Len(t, history, 1)
		require.Equal(t, wrapped, history[0].Content)
		require.Equal(t, llm.RuntimeMetadata{}, history[0].Runtime, "untrusted history cannot retain a source or marker bit")
	})
}

func TestAnchoredCurrentRunLossyInputOnlyBlocksAnchoredProjection(t *testing.T) {
	input := AgentMessage{Role: "user", Content: "input", InputKind: InputKindUser}
	digest, err := RetainedInputDigest("fixture-run", input)
	require.NoError(t, err)
	changed := input
	changed.Content += "!"
	other, err := RetainedInputDigest("fixture-run", changed)
	require.NoError(t, err)
	require.NotEqual(t, digest, other)
	changed = input
	changed.Content = string([]byte{0xff})
	_, err = RetainedInputDigest("fixture-run", changed)
	require.Error(t, err)

	// A non-nil empty slice is accepted by the ordinary sink, but omitempty
	// makes it impossible to reproduce byte-for-byte after journal reload.
	input.ContentBlocks = make([]llm.ContentBlock, 0)
	sink := &requestEnvelopeIndexSink{consumerTestSink: &consumerTestSink{}, source: CompactionSourceSnapshot{BranchID: "main"}}
	owner := newRunOwner(t.Context(), nil, RunClaim{ID: "fixture-run"}, nil)
	defer owner.cancel()
	ctx := withRunID(withRunWorkerTracker(owner.ctx, owner), "fixture-run")
	ctx = WithRunCommit(ctx, sink, RunCommitBinding{SessionID: "fixture-session", WorkspaceID: "fixture-workspace", OwnerGeneration: owner.ownerGeneration})
	consumer, err := newRunCommitConsumer(ctx)
	require.NoError(t, err)
	require.NoError(t, consumer.commitInput(0, input), "ordinary input admission must not change")
	err = consumer.commitCompaction(CompactionProjection{RetainCurrentInput: true}, compaction.TurnItem{Role: "user", Content: input.Content})
	require.ErrorIs(t, err, ErrDurableCompactionUnsupported)
	steps, _, _, _ := sink.snapshot()
	require.Equal(t, []string{"input"}, steps)
}

var _ http.RoundTripper = (*anchoredCurrentMemoryTransport)(nil)
