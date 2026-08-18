package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
)

// blockingModel wraps a mock LLM server and blocks on a channel before each
// Generate call, allowing tests to control when the LLM responds. This is
// used to test pause/resume behavior.
type blockingModel struct {
	inner    llm.BaseChatModel
	proceed  chan struct{}
	callDone chan struct{}
	mu       sync.Mutex
	started  bool
}

func newBlockingModel(inner llm.BaseChatModel) *blockingModel {
	return &blockingModel{
		inner:    inner,
		proceed:  make(chan struct{}, 1),
		callDone: make(chan struct{}, 1),
	}
}

func (m *blockingModel) Generate(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (*llm.Message, error) {
	select {
	case <-m.proceed:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return m.inner.Generate(ctx, msgs, opts...)
}

func (m *blockingModel) Stream(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	select {
	case <-m.proceed:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return m.inner.Stream(ctx, msgs, opts...)
}

// TestLoopAgentPauseResume verifies that Pause blocks the loop and Resume
// unblocks it, allowing the loop to continue.
func TestLoopAgentPauseResume(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"L-P1", "pause",
		mock.ConversationTurn{AssistantContent: "hello after resume"},
	))
	loop := NewLoopAgent(WithLLM(model))

	// Pause before running.
	loop.Pause()
	// Pause again is a no-op.
	loop.Pause()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		events []AgentEvent
		runErr error
		wg     sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		events, runErr = loop.Run(ctx, Submission{Content: "hi"})
	}()

	// Give the loop a moment to reach the pause point.
	time.Sleep(50 * time.Millisecond)

	// Resume should unblock the loop.
	loop.Resume()
	// Resume again is a no-op.
	loop.Resume()

	wg.Wait()

	require.NoError(t, runErr)
	messages := findEvents(events, "message")
	require.Len(t, messages, 1)
	assert.Equal(t, "hello after resume", messages[0])
}

// TestLoopAgentPauseNotPaused verifies that pauseWait returns immediately when
// the loop is not paused.
func TestLoopAgentPauseNotPaused(t *testing.T) {
	loop := NewLoopAgent(WithLLM(mock.NewMockLLMServer(mock.NewConversationTemplate(
		"L-P2", "nopause",
		mock.ConversationTurn{AssistantContent: "ok"},
	))))

	err := loop.pauseWait(context.Background())
	require.NoError(t, err)
}

// TestLoopAgentPauseCanceledDuringPause verifies that canceling the context
// while paused causes the loop to return with the context error.
func TestLoopAgentPauseCanceledDuringPause(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"L-P3", "cancel_pause",
		mock.ConversationTurn{AssistantContent: "should not reach"},
	))
	loop := NewLoopAgent(WithLLM(model))
	loop.Pause()

	ctx, cancel := context.WithCancel(context.Background())

	var runErr error
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, runErr = loop.Run(ctx, Submission{Content: "hi"})
	}()

	// Give the loop a moment to reach the pause point.
	time.Sleep(50 * time.Millisecond)

	// Cancel while paused.
	cancel()
	wg.Wait()

	require.Error(t, runErr)
	assert.ErrorIs(t, runErr, context.Canceled)
}

// TestTokenUsageEvent verifies that a token_usage AgentEvent can carry
// TokenUsage data.
func TestTokenUsageEvent(t *testing.T) {
	ev := AgentEvent{
		Kind: "token_usage",
		TokenUsage: &TokenUsage{
			InputTokens:  100,
			OutputTokens: 50,
			MaxTokens:    8000,
			Cost:         0.02,
		},
	}
	require.NotNil(t, ev.TokenUsage)
	assert.Equal(t, "token_usage", ev.Kind)
	assert.Equal(t, 100, ev.TokenUsage.InputTokens)
	assert.Equal(t, 50, ev.TokenUsage.OutputTokens)
	assert.Equal(t, 8000, ev.TokenUsage.MaxTokens)
	assert.Equal(t, 0.02, ev.TokenUsage.Cost)
}
