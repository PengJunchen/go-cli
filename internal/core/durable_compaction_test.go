package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestDurableMessageConversionsPreserveRuntimeAndHistoricalSummaryMarker(t *testing.T) {
	priorSource := llm.RuntimeMetadata{Source: llm.SourceRef{
		Kind: llm.SourcePriorPublished, HistoryRevision: "rev-8", BranchID: "branch-main",
		SourceEntryID: "real-compaction-marker", PriorOrdinal: 0,
	}}
	historical := llm.Message{
		Role: llm.RoleUser, Content: compaction.UntrustedSummaryContext("old summary body"), Runtime: priorSource,
	}
	items := messagesToTurnItems([]llm.Message{historical})
	require.Len(t, items, 1)
	require.True(t, items[0].IsCompaction)
	require.Equal(t, "old summary body", items[0].Content)
	require.Equal(t, priorSource, items[0].Runtime)

	restored := turnItemsToMessages(items)
	require.Len(t, restored, 1)
	require.Equal(t, historical.Content, restored[0].Content, "the untrusted wrapper is applied exactly once")
	require.Equal(t, priorSource, restored[0].Runtime, "a restored prior marker does not retain an old run Through")

	unknown := llm.Message{
		Role: llm.RoleTool, Content: "receipt", ToolCallID: "call-1", Name: "read",
		Runtime: llm.RuntimeMetadata{
			Source:         llm.SourceRef{Kind: llm.SourceRunTranscript, RunID: "run-new", PriorSourceCount: 1, TranscriptIndex: 2},
			ReceiptOutcome: ReceiptOutcomeUnknown,
		},
	}
	toolItems := messagesToTurnItems([]llm.Message{unknown})
	require.Equal(t, ReceiptOutcomeUnknown, toolItems[0].ReceiptOutcome)
	require.Equal(t, unknown.Runtime, toolItems[0].Runtime)
	require.Equal(t, unknown, turnItemsToMessages(toolItems)[0])
}

func TestDurableProjectionUsesRealSourceIDsForRepeatedText(t *testing.T) {
	snapshot := &CompactionSourceSnapshot{
		Revision: "rev-repeat", BranchID: "branch-main",
		Messages:       []AgentMessage{{Role: "user", Content: "repeat"}, {Role: "assistant", Content: "repeat"}},
		SourceEntryIDs: []string{"entry-user-real", "entry-assistant-real"},
	}
	input := []compaction.TurnItem{
		{ID: "msg-0", Role: "system", Content: "prompt", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceEphemeral}}},
		{ID: "msg-1", Role: "user", Content: "repeat", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{
			Kind: llm.SourcePriorPublished, HistoryRevision: snapshot.Revision, BranchID: snapshot.BranchID,
			SourceEntryID: snapshot.SourceEntryIDs[0], PriorOrdinal: 0,
		}}},
		{ID: "msg-2", Role: "assistant", Content: "repeat", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{
			Kind: llm.SourcePriorPublished, HistoryRevision: snapshot.Revision, BranchID: snapshot.BranchID,
			SourceEntryID: snapshot.SourceEntryIDs[1], PriorOrdinal: 1,
		}}},
		{ID: "msg-3", Role: "user", Content: "current", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{
			Kind: llm.SourceRunTranscript, RunID: "run-current", PriorSourceCount: 2, TranscriptIndex: 0,
		}}},
	}
	through := llm.SourceThrough{PriorHistoryCount: 2}
	result := compaction.CompactionResult{
		Items: []compaction.TurnItem{
			input[0],
			{ID: "summary", Role: "user", Content: "summary body", IsCompaction: true, Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunSummary, RunID: "run-current", Through: through}}},
			input[3],
		},
		Strategy:      compaction.StrategySummary,
		NewProjection: &compaction.NewProjection{Summary: "summary body", Through: through},
	}

	projection, err := validateDurableCompactionResult(input, result, snapshot, "run-current")
	require.NoError(t, err)
	require.Equal(t, snapshot.Revision, projection.ExpectedHistoryRevision)
	require.Equal(t, CompactionCursor{PriorHistoryCount: 2}, projection.Through)
	require.Equal(t, "summary body", projection.Summary)
}

func TestDurableProjectionRejectsSourceBackedSystemPrefixBeforeCommit(t *testing.T) {
	snapshot := &CompactionSourceSnapshot{
		Revision: "rev-system", BranchID: "branch-main",
		Messages:       []AgentMessage{{Role: "system", Content: "durable system"}, {Role: "user", Content: "old"}},
		SourceEntryIDs: []string{"entry-system", "entry-user"},
	}
	prior := func(index int, role, content string) llm.Message {
		return llm.Message{Role: llm.Role(role), Content: content, Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{
			Kind: llm.SourcePriorPublished, HistoryRevision: snapshot.Revision, BranchID: snapshot.BranchID,
			SourceEntryID: snapshot.SourceEntryIDs[index], PriorOrdinal: uint64(index),
		}}}
	}
	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "ephemeral prompt", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceEphemeral}}},
		prior(0, "system", "durable system"), prior(1, "user", "old"),
		{Role: llm.RoleUser, Content: "current", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunTranscript, RunID: "run-system", PriorSourceCount: 2, TranscriptIndex: 0}}},
	}
	input := messagesToTurnItems(messages)
	through := llm.SourceThrough{PriorHistoryCount: 2}
	result := compaction.CompactionResult{
		Items: []compaction.TurnItem{
			input[0], input[1],
			{ID: "summary", Role: "user", Content: "summary", IsCompaction: true, Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunSummary, RunID: "run-system", Through: through}}},
			input[3],
		},
		Strategy:      compaction.StrategySummary,
		NewProjection: &compaction.NewProjection{Summary: "summary", Through: through},
	}
	consumer := &runCommitConsumer{source: snapshot, runID: "run-system", sink: &compactionCommitCountingSink{}}
	compactor := &fixedCompactionResult{result: result}
	_, err := preflightDurableRequest(context.Background(), messages, consumer, compactor, oneTokenPerNonEmptyText{}, 1)
	require.ErrorIs(t, err, ErrDurableCompactionUnsupported)
	require.Equal(t, 1, compactor.calls)
	require.Zero(t, consumer.sink.(*compactionCommitCountingSink).commits, "unsupported retained source prefix must not sync a projection")
}

func TestDurableProjectionSecondCutKeepsOriginalTranscriptCoordinates(t *testing.T) {
	snapshot := &CompactionSourceSnapshot{
		Revision: "rev-second", BranchID: "branch-main",
		Messages:       []AgentMessage{{Role: "user", Content: "old"}, {Role: "assistant", Content: "old answer"}},
		SourceEntryIDs: []string{"old-user-id", "old-assistant-id"},
	}
	input := []compaction.TurnItem{
		{ID: "prompt", Role: "system", Content: "prompt", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceEphemeral}}},
		{ID: "prior-summary", Role: "user", Content: "first summary", IsCompaction: true, Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{
			Kind: llm.SourceRunSummary, RunID: "run-second", Through: llm.SourceThrough{PriorHistoryCount: 2, TranscriptCount: 1},
		}}},
		{ID: "transcript-1", Role: "assistant", Content: "first answer", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{
			Kind: llm.SourceRunTranscript, RunID: "run-second", PriorSourceCount: 2, TranscriptIndex: 1,
		}}},
		{ID: "transcript-2", Role: "user", Content: "follow-up", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{
			Kind: llm.SourceRunTranscript, RunID: "run-second", PriorSourceCount: 2, TranscriptIndex: 2,
		}}},
	}
	through := llm.SourceThrough{PriorHistoryCount: 2, TranscriptCount: 2}
	result := compaction.CompactionResult{
		Items: []compaction.TurnItem{
			input[0],
			{ID: "summary-2", Role: "user", Content: "second summary", IsCompaction: true, Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunSummary, RunID: "run-second", Through: through}}},
			input[3],
		},
		Strategy:      compaction.StrategySummary,
		NewProjection: &compaction.NewProjection{Summary: "second summary", Through: through},
	}
	projection, err := validateDurableCompactionResult(input, result, snapshot, "run-second")
	require.NoError(t, err)
	require.Equal(t, CompactionCursor{PriorHistoryCount: 2, TranscriptCount: 2}, projection.Through)
}

func TestPendingInputUsesCapturedPriorCountAndWriterAssignedIndex(t *testing.T) {
	for _, priorCount := range []uint64{0, 3} {
		t.Run(fmt.Sprintf("prior-%d", priorCount), func(t *testing.T) {
			consumer := &runCommitConsumer{runID: "run-pending", source: &CompactionSourceSnapshot{
				Revision: "rev", BranchID: "main", SourceEntryIDs: make([]string, priorCount),
			}}
			input := pendingRunInput([]AgentMessage{{Role: "user", Content: "same text", InputKind: InputKindUser}}, "same text", consumer)
			pending := input.message.Runtime.Source
			require.Equal(t, llm.SourcePendingRunInput, pending.Kind)
			require.Equal(t, priorCount, pending.PriorSourceCount)
			committed := consumer.transcriptMetadata(0, "")
			require.Equal(t, llm.SourceRunTranscript, committed.Source.Kind)
			require.Equal(t, priorCount, committed.Source.PriorSourceCount)
			require.Zero(t, committed.Source.TranscriptIndex, "the writer-assigned index is independent of the compacted Agent slice")
		})
	}
}

func TestDurableCurrentInputPreservationUsesTypedSourceIdentity(t *testing.T) {
	snapshot := &CompactionSourceSnapshot{
		Revision: "rev-current-identity", BranchID: "branch-main",
		Messages:       []AgentMessage{{Role: "user", Content: "same text"}},
		SourceEntryIDs: []string{"prior-same-text"},
	}
	current := compaction.TurnItem{
		ID: "current-input", Role: compaction.RoleUser, Content: "same text",
		ContentBlocks: []llm.ContentBlock{{Type: "text", Text: "original block"}},
		Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{
			Kind: llm.SourceRunTranscript, RunID: "run-current-identity", PriorSourceCount: 1, TranscriptIndex: 0,
		}},
	}
	prior := compaction.TurnItem{
		ID: "prior-input", Role: compaction.RoleUser, Content: "same text",
		Runtime: llm.RuntimeMetadata{Source: priorRef(snapshot, 0)},
	}

	captured, err := captureDurableCurrentInput([]compaction.TurnItem{prior, current}, snapshot, "run-current-identity")
	require.NoError(t, err)
	require.Equal(t, current.Runtime.Source, captured.Runtime.Source,
		"same text in an older durable row must not substitute for the current transcript identity")
	require.Equal(t, current, captured)

	mutatedInPlace := current
	mutatedInPlace.ContentBlocks[0].Text = "rewritten block"
	require.ErrorIs(t, validateDurableCurrentInputPreserved(captured, []compaction.TurnItem{prior, mutatedInPlace}), ErrDurableCompactionUnsupported,
		"the frozen current-input content graph must not share its backing storage with a mutating compactor")
	current.ContentBlocks[0].Text = "original block"

	changed := current
	changed.Content = "edited current input"
	wrongIdentity := current
	wrongIdentity.Runtime.Source.TranscriptIndex = 1

	tests := []struct {
		name   string
		result []compaction.TurnItem
	}{
		{name: "missing current input", result: []compaction.TurnItem{prior}},
		{name: "changed current input", result: []compaction.TurnItem{changed}},
		{name: "duplicated current input", result: []compaction.TurnItem{current, current}},
		{name: "same text with different typed identity", result: []compaction.TurnItem{prior, wrongIdentity}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.ErrorIs(t, validateDurableCurrentInputPreserved(captured, test.result), ErrDurableCompactionUnsupported)
		})
	}
	require.NoError(t, validateDurableCurrentInputPreserved(captured, []compaction.TurnItem{prior, current}))
}

func TestDurableEnvelopePreflightCompactsLargeWriteArgumentsWithTypedProjection(t *testing.T) {
	snapshot, messages := durableLargeWriteFixture()
	const runID = "run-large-write-envelope"
	estimator := durableRuneCountEstimator{}
	providerMessageTokens, multimodal, err := estimateProviderMessages(messages, estimator, 0)
	require.NoError(t, err)
	require.Equal(t, llm.EstimateKnown, multimodal.State)
	require.Equal(t, providerMessageTokens, compaction.EstimateTokens(messagesToTurnItems(messages), estimator),
		"the compactor and request-envelope preflight must use the same provider-visible message terms")
	const requestCeiling = 8000
	messageBudget := fourFifthsFloor(requestCeiling)
	require.Equal(t, 6400, messageBudget)
	require.Greater(t, providerMessageTokens, messageBudget,
		"write arguments plus the rest of the message transcript exceed the local budget")

	summarizer := &durableCompactionSummarizer{summary: "summary of earlier durable context"}
	compactor := compaction.NewUnifiedCompactor(compaction.WithSummary(compaction.NewSummaryCompactor(summarizer)))
	sink := &compactionCommitCountingSink{}
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: runID}, nil)
	consumer := &runCommitConsumer{ctx: owner.ctx, owner: owner, source: snapshot, runID: runID, sink: sink}
	compacted, currentIndex, err := preflightDurableRequestAt(
		context.Background(), messages, consumer, compactor,
		estimator, messageBudget, 3,
	)
	require.NoError(t, err)
	require.Equal(t, 1, sink.commits, "only a validated typed summary may update durable compaction state")
	require.Equal(t, CompactionCursor{PriorHistoryCount: 3}, sink.projection.Through)
	require.Equal(t, 1, currentIndex)
	require.Equal(t, messages[3], compacted[currentIndex], "the exact current source-backed input stays unchanged")
	require.Len(t, compacted, 2, "the old source prefix becomes one typed summary plus the current input")
	require.Equal(t, llm.SourceRunSummary, compacted[0].Runtime.Source.Kind)
	require.Equal(t, runID, compacted[0].Runtime.Source.RunID)
	require.Equal(t, llm.SourceThrough{PriorHistoryCount: 3}, compacted[0].Runtime.Source.Through)
	require.NotEmpty(t, summarizer.calls, "a real summary strategy must establish the typed projection")
}

func TestDurableEnvelopePreflightFailsClosedWithoutTypedSummary(t *testing.T) {
	failingSummarizer := &durableCompactionFailingSummarizer{err: errors.New("summary service unavailable")}
	for _, test := range []struct {
		name              string
		compactor         compaction.Compactor
		failingSummarizer *durableCompactionFailingSummarizer
	}{
		{name: "projection-free fallback", compactor: compaction.NewUnifiedCompactor(compaction.WithTruncating(&fixedCompactionResult{}))},
		{
			name:              "summarizer failed",
			compactor:         compaction.NewUnifiedCompactor(compaction.WithSummary(compaction.NewSummaryCompactor(failingSummarizer))),
			failingSummarizer: failingSummarizer,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot, messages := durableLargeWriteFixture()
			sink := &compactionCommitCountingSink{}
			consumer := &runCommitConsumer{source: snapshot, runID: "run-large-write-envelope", sink: sink}
			_, _, err := preflightDurableRequestAt(
				context.Background(), messages, consumer, test.compactor,
				durableRuneCountEstimator{}, 6400, 3,
			)
			require.Error(t, err, "a no-op micro or truncating result must not fake a typed projection")
			require.Zero(t, sink.commits)
			if test.failingSummarizer != nil {
				require.Equal(t, 1, test.failingSummarizer.calls, "the configured summary path must be attempted")
			}
		})
	}
}

func TestDurableEnvelopePreflightCannotSummarizeAwayOversizedCurrentInput(t *testing.T) {
	snapshot, messages := durableLargeWriteFixture()
	messages[3].Content = strings.Repeat("current-input-", 451)
	originalCurrentInput := messages[3].Content
	sink := &compactionCommitCountingSink{}
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "run-large-write-envelope"}, nil)
	consumer := &runCommitConsumer{ctx: owner.ctx, owner: owner, source: snapshot, runID: "run-large-write-envelope", sink: sink}
	summarizer := &durableCompactionSummarizer{summary: "summary that cannot replace the current input"}
	compactor := compaction.NewUnifiedCompactor(compaction.WithSummary(compaction.NewSummaryCompactor(summarizer)))

	_, _, err := preflightDurableRequestAt(
		context.Background(), messages, consumer, compactor,
		durableRuneCountEstimator{}, 6400, 3,
	)
	require.ErrorIs(t, err, ErrDurableCompactionUnsupported,
		"a source-safe summary may not drop the current input just to satisfy a small budget")
	require.Zero(t, sink.commits, "a summary covering the current input must not be committed")
	require.Equal(t, originalCurrentInput, messages[3].Content, "the caller-owned current input stays unchanged")
}

func TestCloneCompactionTurnItemCopiesNestedToolArguments(t *testing.T) {
	args := map[string]any{"nested": []any{map[string]any{"path": "original"}}}
	item := compaction.TurnItem{
		Role:      compaction.RoleAssistant,
		ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read", Args: args}},
	}
	cloned := cloneCompactionTurnItem(item)
	args["nested"].([]any)[0].(map[string]any)["path"] = "mutated"
	require.Equal(t, "original", cloned.ToolCalls[0].Args.(map[string]any)["nested"].([]any)[0].(map[string]any)["path"],
		"JSON tool arguments must not remain aliased to the mutable compactor input graph")
}

func TestClosedHistoricalUnknownPassesRequestGateButNotOverBudgetCompactionGate(t *testing.T) {
	snapshot := &CompactionSourceSnapshot{
		Revision: "rev-unknown", BranchID: "branch-main",
		Messages: []AgentMessage{
			{Role: "user", Content: "old"},
			{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call-unknown", Name: "read", Args: map[string]any{}}}},
			{Role: "tool", Content: "result uncertain", ToolCallID: "call-unknown", ToolName: "read", ReceiptOutcome: ReceiptOutcomeUnknown},
		},
		SourceEntryIDs: []string{"old-user", "old-assistant", "old-receipt"},
	}
	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "ephemeral", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceEphemeral}}},
		{Role: llm.RoleUser, Content: "old", Runtime: llm.RuntimeMetadata{Source: priorRef(snapshot, 0)}},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-unknown", Name: "read", Args: map[string]any{}}}, Runtime: llm.RuntimeMetadata{Source: priorRef(snapshot, 1)}},
		{Role: llm.RoleTool, Content: "result uncertain", ToolCallID: "call-unknown", Name: "read", Runtime: llm.RuntimeMetadata{Source: priorRef(snapshot, 2), ReceiptOutcome: ReceiptOutcomeUnknown}},
		{Role: llm.RoleUser, Content: "fresh operation", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunTranscript, RunID: "run-fresh", PriorSourceCount: 3, TranscriptIndex: 0}}},
	}
	consumer := &runCommitConsumer{source: snapshot, runID: "run-fresh", sink: &compactionCommitCountingSink{}}
	compactor := &fixedCompactionResult{}
	unchanged, err := preflightDurableRequest(context.Background(), messages, consumer, compactor, oneTokenPerNonEmptyText{}, 20)
	require.NoError(t, err)
	require.Equal(t, messages, unchanged)
	require.Zero(t, compactor.calls, "a closed unknown group remains unchanged below budget")

	_, err = preflightDurableRequest(context.Background(), messages, consumer, compactor, oneTokenPerNonEmptyText{}, 1)
	require.ErrorIs(t, err, ErrDurableCompactionUnsupported)
	require.Zero(t, compactor.calls, "unknown over-budget history is rejected before any strategy")
	require.Zero(t, consumer.sink.(*compactionCommitCountingSink).commits)
}

func priorRef(snapshot *CompactionSourceSnapshot, ordinal int) llm.SourceRef {
	return llm.SourceRef{
		Kind: llm.SourcePriorPublished, HistoryRevision: snapshot.Revision, BranchID: snapshot.BranchID,
		SourceEntryID: snapshot.SourceEntryIDs[ordinal], PriorOrdinal: uint64(ordinal),
	}
}

func TestDurableProjectionRejectsSameRunSummaryInRetainedSuffix(t *testing.T) {
	snapshot := &CompactionSourceSnapshot{Revision: "rev", BranchID: "main", Messages: []AgentMessage{{Role: "user", Content: "prior"}}, SourceEntryIDs: []string{"prior-id"}}
	prior := llm.SourceRef{Kind: llm.SourcePriorPublished, HistoryRevision: snapshot.Revision, BranchID: snapshot.BranchID, SourceEntryID: "prior-id"}
	oldThrough := llm.SourceThrough{PriorHistoryCount: 1, TranscriptCount: 1}
	input := []compaction.TurnItem{
		{ID: "prior", Role: "user", Content: "prior", Runtime: llm.RuntimeMetadata{Source: prior}},
		{ID: "old-summary", Role: "user", Content: "old", IsCompaction: true, Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunSummary, RunID: "run", Through: oldThrough}}},
		{ID: "tail", Role: "assistant", Content: "tail", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunTranscript, RunID: "run", PriorSourceCount: 1, TranscriptIndex: 1}}},
	}
	through := llm.SourceThrough{PriorHistoryCount: 1, TranscriptCount: 2}
	result := compaction.CompactionResult{
		Items: []compaction.TurnItem{
			{ID: "new-summary", Role: "user", Content: "new", IsCompaction: true, Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunSummary, RunID: "run", Through: through}}},
			input[1], input[2],
		},
		Strategy:      compaction.StrategySummary,
		NewProjection: &compaction.NewProjection{Summary: "new", Through: through},
	}
	_, err := validateDurableCompactionResult(input, result, snapshot, "run")
	require.ErrorIs(t, err, ErrDurableCompactionUnsupported)
}

type compactionCommitCountingSink struct {
	inertRunCommitSink
	commits    int
	projection CompactionProjection
}

func (sink *compactionCommitCountingSink) CommitCompaction(_ context.Context, projection CompactionProjection) error {
	sink.commits++
	sink.projection = projection
	return nil
}

type durableCompactionSummarizer struct {
	summary string
	calls   []string
}

func (summarizer *durableCompactionSummarizer) Summarize(_ context.Context, conversation string) (string, error) {
	summarizer.calls = append(summarizer.calls, conversation)
	return summarizer.summary, nil
}

type durableCompactionFailingSummarizer struct {
	err   error
	calls int
}

func (summarizer *durableCompactionFailingSummarizer) Summarize(context.Context, string) (string, error) {
	summarizer.calls++
	return "", summarizer.err
}

type durableRuneCountEstimator struct{}

func (durableRuneCountEstimator) Estimate(text string) (int, error) {
	return utf8.RuneCountInString(text), nil
}

func durableLargeWriteFixture() (*CompactionSourceSnapshot, []llm.Message) {
	const revision = "rev-large-write-envelope"
	const branch = "branch-main"
	const runID = "run-large-write-envelope"
	callID := "call-large-write"
	args := map[string]any{"path": "old.go", "content": strings.Repeat("x", 7200)}
	toolResult := strings.Repeat("r", 8000)
	snapshot := &CompactionSourceSnapshot{
		Revision: revision, BranchID: branch,
		Messages: []AgentMessage{
			{Role: "user", Content: "older user context"},
			{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: callID, Name: "write", Args: args}}},
			{Role: "tool", Content: toolResult, ToolCallID: callID, ToolName: "write", ReceiptOutcome: ReceiptOutcomeSuccess},
		},
		SourceEntryIDs: []string{"entry-user", "entry-write-call", "entry-write-receipt"},
	}
	prior := func(ordinal int) llm.RuntimeMetadata {
		return llm.RuntimeMetadata{Source: llm.SourceRef{
			Kind: llm.SourcePriorPublished, HistoryRevision: revision, BranchID: branch,
			SourceEntryID: snapshot.SourceEntryIDs[ordinal], PriorOrdinal: uint64(ordinal),
		}}
	}
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: "older user context", Runtime: prior(0)},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: callID, Name: "write", Args: args}}, Runtime: prior(1)},
		{Role: llm.RoleTool, Content: toolResult, ToolCallID: callID, Name: "write", Runtime: llm.RuntimeMetadata{
			Source:         llm.SourceRef{Kind: llm.SourcePriorPublished, HistoryRevision: revision, BranchID: branch, SourceEntryID: snapshot.SourceEntryIDs[2], PriorOrdinal: 2},
			ReceiptOutcome: ReceiptOutcomeSuccess,
		}},
		{Role: llm.RoleUser, Content: "current request", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{
			Kind: llm.SourceRunTranscript, RunID: runID, PriorSourceCount: 3, TranscriptIndex: 0,
		}}},
	}
	return snapshot, messages
}

type fixedCompactionResult struct {
	result compaction.CompactionResult
	calls  int
}

func (compactor *fixedCompactionResult) Compact(_ context.Context, _ []compaction.TurnItem, _ int, _ compaction.TokenEstimator) (compaction.CompactionResult, error) {
	compactor.calls++
	return compactor.result, nil
}

type oneTokenPerNonEmptyText struct{}

func (oneTokenPerNonEmptyText) Estimate(text string) (int, error) {
	if text == "" {
		return 0, nil
	}
	return 1, nil
}

func TestDurableTruncatingProjectionRequiresFixedMarkerAndExactSuffix(t *testing.T) {
	snapshot, messages := durableLargeWriteFixture()
	input := messagesToTurnItems(messages)
	result, err := compaction.NewTruncatingCompactor().Compact(t.Context(), input, 6400, durableRuneCountEstimator{})
	require.NoError(t, err)
	require.NotNil(t, result.NewProjection)
	_, err = validateDurableCompactionResult(input, result, snapshot, "run-large-write-envelope")
	require.NoError(t, err)
	for _, change := range []func(*compaction.CompactionResult){
		func(r *compaction.CompactionResult) {
			r.NewProjection.Summary = "invented factual summary"
			for i := range r.Items {
				if r.Items[i].IsCompaction {
					r.Items[i].Content = r.NewProjection.Summary
				}
			}
		},
		func(r *compaction.CompactionResult) { r.Items[len(r.Items)-1].Content = "changed current input" },
		func(r *compaction.CompactionResult) { r.Items = r.Items[:len(r.Items)-1] },
		func(r *compaction.CompactionResult) { r.NewProjection.Through.PriorHistoryCount-- },
	} {
		candidate := result
		candidate.Items = cloneCompactionTurnItems(result.Items)
		projection := *result.NewProjection
		candidate.NewProjection = &projection
		change(&candidate)
		_, err = validateDurableCompactionResult(input, candidate, snapshot, "run-large-write-envelope")
		require.ErrorIs(t, err, errDurableCompactionProjectionInvalid)
	}
}
