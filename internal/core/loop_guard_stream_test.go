package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/stretchr/testify/require"
)

type sequenceStreamModel struct {
	responses [][]llm.MessageChunk
	calls     int
}

type contextBlockedStreamModel struct {
	stopped chan struct{}
}

func (*contextBlockedStreamModel) Generate(context.Context, []llm.Message, ...llm.Option) (*llm.Message, error) {
	return nil, errors.New("unexpected Generate call")
}

func (m *contextBlockedStreamModel) Stream(ctx context.Context, _ []llm.Message, _ ...llm.Option) (<-chan llm.MessageChunk, error) {
	ch := make(chan llm.MessageChunk)
	go func() {
		<-ctx.Done()
		close(m.stopped)
	}()
	return ch, nil
}

func (m *sequenceStreamModel) Generate(context.Context, []llm.Message, ...llm.Option) (*llm.Message, error) {
	return nil, errors.New("stream loop must not call Generate")
}

func (m *sequenceStreamModel) Stream(context.Context, []llm.Message, ...llm.Option) (<-chan llm.MessageChunk, error) {
	if m.calls >= len(m.responses) {
		return nil, fmt.Errorf("unexpected stream call %d", m.calls+1)
	}
	response := m.responses[m.calls]
	m.calls++
	ch := make(chan llm.MessageChunk, len(response))
	for _, chunk := range response {
		ch <- chunk
	}
	close(ch)
	return ch, nil
}

type recordingCompleteGuardModel struct {
	*sequenceStreamModel
	result *CandidateOutputResult
	err    error
	checks []string
}

func (m *recordingCompleteGuardModel) MaxCandidateBytes() int {
	return llm.GuardedStreamMaxBytes
}

func (m *recordingCompleteGuardModel) CheckCandidate(_ context.Context, text string) (*CandidateOutputResult, error) {
	m.checks = append(m.checks, text)
	return m.result, m.err
}

func TestCompleteResponseGuardSeesWholeContinuationAndPreservesToolCalls(t *testing.T) {
	model := &recordingCompleteGuardModel{
		sequenceStreamModel: &sequenceStreamModel{responses: [][]llm.MessageChunk{
			{
				{Role: llm.RoleAssistant, Content: "john@"},
				{Role: llm.RoleAssistant, Content: "exam"},
				{Role: llm.RoleAssistant, Final: true, FinishReason: "length"},
			},
			{
				{Role: llm.RoleAssistant, Content: "ple.com"},
				{Role: llm.RoleAssistant, Final: true, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{
					ID: "call-safe", Name: "inspect", Args: map[string]any{"path": "a.go"},
				}}},
			},
		}},
		result: &CandidateOutputResult{Allowed: false, Sanitized: "[redacted]"},
	}
	stream := &collectingEventStream{}
	loop := NewLoopAgent()
	response, err := loop.generateWithContinuation(context.Background(), model, nil, nil, stream, slog.Default())
	require.NoError(t, err)
	require.Equal(t, "[redacted]", response.Content)
	require.Equal(t, []string{"john@example.com"}, model.checks, "guard must receive one complete candidate across chunks and continuations")
	require.Equal(t, 2, model.calls)
	require.Equal(t, []llm.ToolCall{{ID: "call-safe", Name: "inspect", Args: map[string]any{"path": "a.go"}}}, response.ToolCalls)
	events := stream.collected()
	require.Len(t, events, 1)
	require.True(t, events[0].Incremental)
	require.Equal(t, "[redacted]", events[0].Content, "only checked sanitized text may be published")
}

func TestCompleteResponseGuardContinuationFailurePublishesNoCandidate(t *testing.T) {
	continuationErr := errors.New("continuation transport failed")
	model := &recordingCompleteGuardModel{
		sequenceStreamModel: &sequenceStreamModel{responses: [][]llm.MessageChunk{
			{
				{Role: llm.RoleAssistant, Content: "unreviewed partial"},
				{Role: llm.RoleAssistant, Final: true, FinishReason: "length"},
			},
			{{Error: continuationErr}},
		}},
		result: &CandidateOutputResult{Allowed: true},
	}
	stream := &collectingEventStream{}
	loop := NewLoopAgent()
	_, err := loop.generateWithContinuation(context.Background(), model, nil, nil, stream, slog.Default())
	require.ErrorIs(t, err, continuationErr)
	require.Empty(t, model.checks, "the guard must not inspect an incomplete continuation chain")
	require.Empty(t, stream.collected(), "incomplete guarded output must not reach event consumers")
}

func TestCompleteResponseGuardDenialPublishesNoCandidate(t *testing.T) {
	model := &recordingCompleteGuardModel{
		sequenceStreamModel: &sequenceStreamModel{responses: [][]llm.MessageChunk{{
			{Role: llm.RoleAssistant, Content: "denied text"},
			{Role: llm.RoleAssistant, Final: true, FinishReason: "stop"},
		}}},
		result: &CandidateOutputResult{Allowed: false},
	}
	stream := &collectingEventStream{}
	loop := NewLoopAgent()
	_, err := loop.generateWithContinuation(context.Background(), model, nil, nil, stream, slog.Default())
	require.ErrorContains(t, err, "guard denied candidate")
	require.Equal(t, []string{"denied text"}, model.checks)
	require.Empty(t, stream.collected())
}

func TestGuardedStreamBudgetIncludesToolCallPayload(t *testing.T) {
	message := &llm.Message{
		Role: llm.RoleAssistant,
		ToolCalls: []llm.ToolCall{{
			ID: "call-1", Name: "inspect", Args: map[string]any{"payload": string(make([]byte, guardedStreamMaxBytes))},
		}},
	}
	bytes, err := candidatePayloadBytes(message)
	require.NoError(t, err)
	require.Greater(t, bytes, guardedStreamMaxBytes)
}

func TestGuardedStreamBudgetIncludesLengthSegmentToolPayload(t *testing.T) {
	model := &recordingCompleteGuardModel{
		sequenceStreamModel: &sequenceStreamModel{responses: [][]llm.MessageChunk{{
			{Role: llm.RoleAssistant, Content: strings.Repeat("x", 240_000)},
			{Role: llm.RoleAssistant, Final: true, FinishReason: "length", ToolCalls: []llm.ToolCall{{
				ID: "call-1", Name: "inspect", Args: map[string]any{"payload": strings.Repeat("y", 170_000)},
			}}},
		}}},
		result: &CandidateOutputResult{Allowed: true},
	}
	stream := &collectingEventStream{}
	loop := NewLoopAgent()
	_, err := loop.generateWithContinuation(context.Background(), model, nil, nil, stream, slog.Default())
	require.ErrorContains(t, err, "response segment exceeds")
	require.Equal(t, 1, model.calls, "an oversized partial segment must fail before continuation")
	require.Empty(t, model.checks, "an incomplete segment must not reach OutputGuard")
	require.Empty(t, stream.collected(), "guarded partial content must remain unpublished")
}

func TestCompleteResponseGuardRejectsInvalidToolGroupBeforePublishingText(t *testing.T) {
	calls := make([]llm.ToolCall, guardedStreamMaxToolCalls+1)
	for i := range calls {
		calls[i] = llm.ToolCall{ID: fmt.Sprintf("call-%d", i), Name: "inspect", Args: map[string]any{}}
	}
	model := &recordingCompleteGuardModel{
		sequenceStreamModel: &sequenceStreamModel{responses: [][]llm.MessageChunk{{
			{Role: llm.RoleAssistant, Content: "candidate text"},
			{Role: llm.RoleAssistant, Final: true, FinishReason: "stop", ToolCalls: calls},
		}}},
		result: &CandidateOutputResult{Allowed: true},
	}
	stream := &collectingEventStream{}
	loop := NewLoopAgent()
	_, err := loop.generateWithContinuation(context.Background(), model, nil, nil, stream, slog.Default())
	require.ErrorContains(t, err, "more than")
	require.Empty(t, model.checks, "structurally invalid tool groups must not reach output guard")
	require.Empty(t, stream.collected(), "candidate text must not publish before tool group validation")
}

func TestGuardedStreamBudgetIgnoresChunkFramingAndEmptyContent(t *testing.T) {
	text := strings.Repeat("x", 10_000)
	oneChunk := &guardedStreamBudget{}
	require.NoError(t, oneChunk.accountChunk(llm.MessageChunk{Content: text}))

	manyChunks := &guardedStreamBudget{}
	for i := range len(text) {
		require.NoError(t, manyChunks.accountChunk(llm.MessageChunk{Content: text[i : i+1]}))
	}
	require.Equal(t, len(text), oneChunk.bytes)
	require.Equal(t, oneChunk.bytes, manyChunks.bytes)

	emptyChunks := &guardedStreamBudget{}
	for range guardedStreamMaxChunks {
		require.NoError(t, emptyChunks.accountChunk(llm.MessageChunk{}))
	}
	err := emptyChunks.accountChunk(llm.MessageChunk{})
	require.ErrorContains(t, err, "chunks")
	require.Zero(t, emptyChunks.bytes, "empty fragments consume only the independent chunk-count budget")
}

func TestStreamGenerateCancellationDoesNotWaitForInnerChannelClose(t *testing.T) {
	model := &contextBlockedStreamModel{stopped: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	loop := NewLoopAgent()
	_, err := loop.streamGenerateWithBudget(ctx, model, nil, nil, nil, &guardedStreamBudget{}, false)
	require.ErrorIs(t, err, context.Canceled)
	select {
	case <-model.stopped:
	case <-time.After(time.Second):
		t.Fatal("inner stream did not observe cancellation")
	}
}
