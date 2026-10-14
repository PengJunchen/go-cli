package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

type sequenceStreamModel struct {
	responses [][]llm.MessageChunk
	calls     int
}

type contextBlockedStreamModel struct {
	stopped chan struct{}
	release chan struct{}
}

type heldCoreBudgetAbortModel struct {
	content    string
	setupErr   error
	nilNil     bool
	release    <-chan struct{}
	cancelled  chan error
	cleanupEnd chan struct{}
}

func (*contextBlockedStreamModel) Generate(context.Context, []llm.Message, ...llm.Option) (*llm.Message, error) {
	return nil, errors.New("unexpected Generate call")
}

func (m *contextBlockedStreamModel) Stream(ctx context.Context, _ []llm.Message, _ ...llm.Option) (<-chan llm.MessageChunk, error) {
	ch := make(chan llm.MessageChunk)
	go func() {
		<-ctx.Done()
		close(m.stopped)
		<-m.release
		close(ch)
	}()
	return ch, nil
}

func (*heldCoreBudgetAbortModel) Generate(context.Context, []llm.Message, ...llm.Option) (*llm.Message, error) {
	return nil, errors.New("unexpected Generate call")
}

func (m *heldCoreBudgetAbortModel) Stream(ctx context.Context, _ []llm.Message, _ ...llm.Option) (<-chan llm.MessageChunk, error) {
	if m.nilNil {
		return nil, nil
	}
	ch := make(chan llm.MessageChunk, 1)
	ch <- llm.MessageChunk{Role: llm.RoleAssistant, Content: m.content}
	go func() {
		<-ctx.Done()
		m.cancelled <- context.Cause(ctx)
		<-m.release
		close(m.cleanupEnd)
		close(ch)
	}()
	return ch, m.setupErr
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
	events := messageEvents(stream.collected())
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
	require.Empty(t, messageEvents(stream.collected()), "incomplete guarded output must not reach event consumers")
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
	require.Empty(t, messageEvents(stream.collected()))
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
	require.Empty(t, messageEvents(stream.collected()), "guarded partial content must remain unpublished")
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
	require.Empty(t, messageEvents(stream.collected()), "candidate text must not publish before tool group validation")
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

func TestStreamGenerateCancellationWaitsForInnerChannelClose(t *testing.T) {
	model := &contextBlockedStreamModel{stopped: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	loop := NewLoopAgent()
	finished := make(chan error, 1)
	go func() {
		_, err := loop.streamGenerateWithBudget(ctx, model, nil, nil, nil, &guardedStreamBudget{}, false)
		finished <- err
	}()
	select {
	case <-model.stopped:
	case <-time.After(time.Second):
		t.Fatal("inner stream did not observe cancellation")
	}
	select {
	case err := <-finished:
		t.Fatalf("stream returned before source close: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(model.release)
	select {
	case err := <-finished:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("stream did not return after source close")
	}
}

func TestStreamGenerateBudgetAbortWaitsForInnerChannelClose(t *testing.T) {
	release := make(chan struct{})
	model := &heldCoreBudgetAbortModel{
		content:    strings.Repeat("x", guardedStreamMaxBytes+1),
		release:    release,
		cancelled:  make(chan error, 1),
		cleanupEnd: make(chan struct{}),
	}
	finished := make(chan error, 1)
	go func() {
		_, err := NewLoopAgent().streamGenerateWithBudget(context.Background(), model, nil, nil, nil, &guardedStreamBudget{}, false)
		finished <- err
	}()
	select {
	case cause := <-model.cancelled:
		var localAbort *llm.LocalStreamAbortCause
		require.ErrorAs(t, cause, &localAbort)
		require.ErrorContains(t, cause, "local stream abort")
	case <-time.After(time.Second):
		t.Fatal("budget failure did not cancel the source with a typed local abort")
	}
	select {
	case err := <-finished:
		t.Fatalf("stream returned before source close: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-model.cleanupEnd:
	case <-time.After(time.Second):
		t.Fatal("source cleanup did not finish after release")
	}
	select {
	case err := <-finished:
		require.ErrorContains(t, err, "candidate limit")
	case <-time.After(time.Second):
		t.Fatal("stream did not return after source close")
	}
}

func TestStreamGenerateSetupErrorJoinsReturnedChannel(t *testing.T) {
	setupErr := errors.New("stream setup failed")
	release := make(chan struct{})
	model := &heldCoreBudgetAbortModel{
		content:    "partial",
		setupErr:   setupErr,
		release:    release,
		cancelled:  make(chan error, 1),
		cleanupEnd: make(chan struct{}),
	}
	finished := make(chan error, 1)
	go func() {
		_, err := NewLoopAgent().streamGenerateWithBudget(context.Background(), model, nil, nil, nil, &guardedStreamBudget{}, false)
		finished <- err
	}()
	select {
	case cause := <-model.cancelled:
		var localAbort *llm.LocalStreamAbortCause
		require.ErrorAs(t, cause, &localAbort)
		require.ErrorIs(t, cause, setupErr)
	case <-time.After(time.Second):
		t.Fatal("setup failure did not cancel the returned source")
	}
	select {
	case err := <-finished:
		t.Fatalf("stream returned before joining setup-error source: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-model.cleanupEnd:
	case <-time.After(time.Second):
		t.Fatal("source cleanup did not finish after release")
	}
	select {
	case err := <-finished:
		require.ErrorIs(t, err, setupErr)
	case <-time.After(time.Second):
		t.Fatal("stream did not return after joining setup-error source")
	}
}

func TestStreamGenerateRejectsNilChannelWithoutError(t *testing.T) {
	model := &heldCoreBudgetAbortModel{nilNil: true}
	_, err := NewLoopAgent().streamGenerateWithBudget(context.Background(), model, nil, nil, nil, &guardedStreamBudget{}, false)
	require.ErrorContains(t, err, "nil channel")
}

func TestStreamGenerateRejectsClosedStreamWithoutFinalChunk(t *testing.T) {
	model := &sequenceStreamModel{responses: [][]llm.MessageChunk{{}}}
	loop := NewLoopAgent()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := loop.streamGenerateWithBudget(ctx, model, nil, nil, nil, &guardedStreamBudget{}, false)
	require.NoError(t, ctx.Err(), "closed input stream must terminate without waiting for cancellation")
	require.ErrorContains(t, err, "stream ended without a final chunk")
}
