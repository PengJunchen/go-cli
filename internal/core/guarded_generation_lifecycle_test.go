package core

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

const guardedGenerationCandidateMarker = "R8_UNCHECKED_CANDIDATE_MUST_NOT_BE_PUBLISHED"

type lifecycleCandidateGuardModel struct {
	*recordingCompleteGuardModel
	candidates []llm.ResolvedCandidateHandle
}

func (m *lifecycleCandidateGuardModel) ProviderCandidates() []llm.ResolvedCandidateHandle {
	return append([]llm.ResolvedCandidateHandle(nil), m.candidates...)
}

type blockingLifecycleGuardModel struct {
	started chan struct{}
}

func (*blockingLifecycleGuardModel) Generate(context.Context, []llm.Message, ...llm.Option) (*llm.Message, error) {
	return nil, errors.New("unexpected Generate call")
}

func (m *blockingLifecycleGuardModel) Stream(ctx context.Context, _ []llm.Message, _ ...llm.Option) (<-chan llm.MessageChunk, error) {
	chunks := make(chan llm.MessageChunk)
	go func() {
		close(m.started)
		<-ctx.Done()
		close(chunks)
	}()
	return chunks, nil
}

func (*blockingLifecycleGuardModel) MaxCandidateBytes() int { return llm.GuardedStreamMaxBytes }

func (*blockingLifecycleGuardModel) CheckCandidate(context.Context, string) (*CandidateOutputResult, error) {
	return &CandidateOutputResult{Allowed: true}, nil
}

func TestGuardedGenerationLifecycleSurroundsHeldCompleteResponseCheck(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	model := &heldLifecycleGuardModel{
		candidate: guardedGenerationCandidateMarker,
		release:   release,
		started:   started,
		result:    &CandidateOutputResult{Allowed: true, Sanitized: "approved answer"},
	}
	stream := &collectingEventStream{}
	index := uint64(17)
	ctx := withAgentEventCoordinates(context.Background(), agentEventCoordinates{
		runID:                    "run-r8-held",
		canonicalTranscriptIndex: &index,
	})
	loop := NewLoopAgent()
	done := make(chan error, 1)
	go func() {
		response, err := loop.generateWithContinuationAndCompactionState(
			ctx, model, nil, 0, nil, stream, slog.Default(), nil, nil, nil, nil, 0, 0, nil,
		)
		if err == nil {
			if response == nil || response.Content != "approved answer" {
				err = errors.New("guarded response was not replaced with the approved text")
			}
		}
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("guarded provider stream did not emit its untrusted candidate chunk")
	}
	beforeRelease := stream.collected()
	require.Len(t, beforeRelease, 1)
	require.Equal(t, EventKindAssistantGenerationStarted, beforeRelease[0].Kind)
	require.Empty(t, beforeRelease[0].Content)
	require.Equal(t, "run-r8-held", beforeRelease[0].RunID)
	require.NotNil(t, beforeRelease[0].CanonicalTranscriptIndex)
	require.Equal(t, index, *beforeRelease[0].CanonicalTranscriptIndex)
	require.NotContains(t, eventContents(beforeRelease), guardedGenerationCandidateMarker)

	close(release)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("guarded response did not finish after the held provider was released")
	}

	events := stream.collected()
	require.Len(t, events, 3)
	require.Equal(t, "message", events[1].Kind)
	require.True(t, events[1].Incremental)
	require.Equal(t, "approved answer", events[1].Content)
	require.Equal(t, EventKindAssistantGenerationFinished, events[2].Kind)
	require.Empty(t, events[2].Content)
	require.Equal(t, "run-r8-held", events[2].RunID)
	require.NotNil(t, events[2].CanonicalTranscriptIndex)
	require.Equal(t, index, *events[2].CanonicalTranscriptIndex)
	require.NotContains(t, eventContents(events), guardedGenerationCandidateMarker)
	require.Equal(t, []string{guardedGenerationCandidateMarker}, model.checks)
}

func TestGuardedGenerationLifecycleClosesOnPreflightFailure(t *testing.T) {
	base := &recordingCompleteGuardModel{
		sequenceStreamModel: &sequenceStreamModel{},
		result:              &CandidateOutputResult{Allowed: true},
	}
	model := &lifecycleCandidateGuardModel{
		recordingCompleteGuardModel: base,
		candidates: []llm.ResolvedCandidateHandle{
			requestEnvelopeCandidate(t, llm.ModelInfo{Name: "r8-preflight-lifecycle", ContextWindow: 4096}),
		},
	}
	stream := &collectingEventStream{}
	index := uint64(3)
	ctx := withAgentEventCoordinates(context.Background(), agentEventCoordinates{
		runID:                    "run-r8-preflight",
		canonicalTranscriptIndex: &index,
	})

	_, err := NewLoopAgent().generateWithContinuationAndCompactionState(
		ctx, model, nil, 0, nil, stream, slog.Default(), nil, nil, nil, nil, 0, 0, nil,
	)
	require.ErrorIs(t, err, llm.ErrOperationAuthorityRequired)
	events := stream.collected()
	require.Len(t, events, 2)
	require.Equal(t, EventKindAssistantGenerationStarted, events[0].Kind)
	require.Equal(t, EventKindAssistantGenerationFinished, events[1].Kind)
	for _, event := range events {
		require.Empty(t, event.Content)
		require.Equal(t, "run-r8-preflight", event.RunID)
		require.NotNil(t, event.CanonicalTranscriptIndex)
		require.Equal(t, index, *event.CanonicalTranscriptIndex)
	}
	require.Zero(t, model.calls, "a missing operation authority must fail before provider dispatch")
}

func TestGuardedGenerationLifecycleClosesWhenCandidateIsRejected(t *testing.T) {
	release := make(chan struct{})
	close(release)
	model := &heldLifecycleGuardModel{
		candidate: guardedGenerationCandidateMarker,
		release:   release,
		started:   make(chan struct{}),
		result:    &CandidateOutputResult{Allowed: false},
	}
	stream := &collectingEventStream{}
	index := uint64(12)
	ctx := withAgentEventCoordinates(context.Background(), agentEventCoordinates{
		runID:                    "run-r8-rejected",
		canonicalTranscriptIndex: &index,
	})
	_, err := NewLoopAgent().generateWithContinuationAndCompactionState(
		ctx, model, nil, 0, nil, stream, slog.Default(), nil, nil, nil, nil, 0, 0, nil,
	)
	require.ErrorContains(t, err, "complete-response guard denied candidate")
	events := stream.collected()
	require.Equal(t, []string{
		EventKindAssistantGenerationStarted,
		EventKindAssistantGenerationFinished,
	}, eventKinds(events))
	for _, event := range events {
		require.Empty(t, event.Content)
		require.Equal(t, "run-r8-rejected", event.RunID)
		require.NotNil(t, event.CanonicalTranscriptIndex)
		require.Equal(t, index, *event.CanonicalTranscriptIndex)
	}
	require.NotContains(t, eventContents(events), guardedGenerationCandidateMarker)
}

func TestGuardedGenerationLifecycleClosesOnCancellation(t *testing.T) {
	model := &blockingLifecycleGuardModel{started: make(chan struct{})}
	stream := &collectingEventStream{}
	index := uint64(9)
	ctx, cancel := context.WithCancel(withAgentEventCoordinates(context.Background(), agentEventCoordinates{
		runID:                    "run-r8-cancel",
		canonicalTranscriptIndex: &index,
	}))
	done := make(chan error, 1)
	go func() {
		_, err := NewLoopAgent().generateWithContinuationAndCompactionState(
			ctx, model, nil, 0, nil, stream, slog.Default(), nil, nil, nil, nil, 0, 0, nil,
		)
		done <- err
	}()
	select {
	case <-model.started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("guarded provider did not start")
	}
	require.Equal(t, []string{EventKindAssistantGenerationStarted}, eventKinds(stream.collected()))
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("guarded generation did not return after cancellation")
	}
	require.Equal(t, []string{
		EventKindAssistantGenerationStarted,
		EventKindAssistantGenerationFinished,
	}, eventKinds(stream.collected()))
}

type heldLifecycleGuardModel struct {
	candidate string
	release   <-chan struct{}
	started   chan struct{}
	result    *CandidateOutputResult
	checks    []string
}

func (*heldLifecycleGuardModel) Generate(context.Context, []llm.Message, ...llm.Option) (*llm.Message, error) {
	return nil, errors.New("unexpected Generate call")
}

func (m *heldLifecycleGuardModel) Stream(ctx context.Context, _ []llm.Message, _ ...llm.Option) (<-chan llm.MessageChunk, error) {
	chunks := make(chan llm.MessageChunk)
	go func() {
		defer close(chunks)
		select {
		case chunks <- llm.MessageChunk{Role: llm.RoleAssistant, Content: m.candidate}:
		case <-ctx.Done():
			return
		}
		close(m.started)
		select {
		case <-m.release:
		case <-ctx.Done():
			return
		}
		select {
		case chunks <- llm.MessageChunk{Role: llm.RoleAssistant, Final: true, FinishReason: "stop"}:
		case <-ctx.Done():
		}
	}()
	return chunks, nil
}

func (*heldLifecycleGuardModel) MaxCandidateBytes() int { return llm.GuardedStreamMaxBytes }

func (m *heldLifecycleGuardModel) CheckCandidate(_ context.Context, candidate string) (*CandidateOutputResult, error) {
	m.checks = append(m.checks, candidate)
	return m.result, nil
}

func eventContents(events []AgentEvent) []string {
	contents := make([]string, len(events))
	for index, event := range events {
		contents[index] = event.Content
	}
	return contents
}

func eventKinds(events []AgentEvent) []string {
	kinds := make([]string, len(events))
	for index, event := range events {
		kinds[index] = event.Kind
	}
	return kinds
}
