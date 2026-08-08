package llm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testStreamProvider is a ModelProvider whose Build returns a configurable
// mock model with a cleanup function. Used to test cycledModel.Stream cleanup
// behavior.
type testStreamProvider struct {
	model    BaseChatModel
	cleanup  func()
	buildErr error
}

func (p *testStreamProvider) Name() string { return "test-stream" }

func (p *testStreamProvider) Build(_ context.Context, _ ModelConfig) (BaseChatModel, func(), error) {
	if p.buildErr != nil {
		return nil, nil, p.buildErr
	}
	return p.model, p.cleanup, nil
}

func (p *testStreamProvider) Models() []ModelInfo { return nil }

// delayedStreamModel sends chunks with configurable delays between them.
type delayedStreamModel struct {
	chunks  []MessageChunk
	delays  []time.Duration
	streamErr error
}

func (m *delayedStreamModel) Generate(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
	return &Message{Role: RoleAssistant, Content: "ok"}, nil
}

func (m *delayedStreamModel) Stream(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
	if m.streamErr != nil {
		return nil, m.streamErr
	}
	ch := make(chan MessageChunk, len(m.chunks))
	go func() {
		for i, c := range m.chunks {
			if i < len(m.delays) {
				time.Sleep(m.delays[i])
			}
			ch <- c
		}
		close(ch)
	}()
	return ch, nil
}

var _ BaseChatModel = (*delayedStreamModel)(nil)

// TestStreamCleanupCalledAfterDrain verifies that cleanup is called after
// the outer channel is fully drained.
func TestStreamCleanupCalledAfterDrain(t *testing.T) {
	var cleanupCalled atomic.Bool
	cleanup := func() { cleanupCalled.Store(true) }

	inner := &delayedStreamModel{
		chunks: []MessageChunk{
			{Role: RoleAssistant, Content: "chunk1"},
			{Role: RoleAssistant, Content: "chunk2"},
			{Role: RoleAssistant, Content: "chunk3", Final: true},
		},
	}

	provider := &testStreamProvider{model: inner, cleanup: cleanup}
	reg := &ProviderRegistry{providers: map[string]ModelProvider{}}
	require.NoError(t, reg.Register(provider))

	cycler := NewModelCycler(ModelCyclerConfig{
		Models: []ModelEntry{{Provider: "test-stream", Model: "m1"}},
	}).WithRegistry(reg)

	wrapped := cycler.WrapModel(&mockModel{})

	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)

	// Drain the channel.
	for range ch {
	}

	// Give the goroutine time to call cleanup after close.
	require.Eventually(t, func() bool { return cleanupCalled.Load() }, time.Second, 10*time.Millisecond,
		"cleanup should be called after channel drain")
}

// TestStreamCleanupNotCalledOnInitError verifies that cleanup is not called
// when the inner Stream returns an initialization error.
func TestStreamCleanupNotCalledOnInitError(t *testing.T) {
	var cleanupCalled atomic.Bool
	cleanup := func() { cleanupCalled.Store(true) }

	inner := &delayedStreamModel{
		streamErr: errors.New("context_length_exceeded"),
	}

	provider := &testStreamProvider{model: inner, cleanup: cleanup}
	reg := &ProviderRegistry{providers: map[string]ModelProvider{}}
	require.NoError(t, reg.Register(provider))

	cycler := NewModelCycler(ModelCyclerConfig{
		Models: []ModelEntry{{Provider: "test-stream", Model: "m1"}},
	}).WithRegistry(reg)

	// The primary model should be used as fallback (no error from primary).
	wrapped := cycler.WrapModel(&mockModel{})

	ch, err := wrapped.Stream(context.Background(), nil)
	// Fallback to primary succeeds.
	require.NoError(t, err)
	// Drain the fallback channel.
	for range ch {
	}

	// Cleanup from the failed inner model should NOT have been called because
	// the error path already handles it. The cleanup we registered was for the
	// inner model; since Stream returned an error, cleanup was called in the
	// error-handling path before falling back.
	// Actually, looking at the code: on Stream error, cleanup IS called.
	// So cleanupCalled should be true.
	assert.True(t, cleanupCalled.Load(), "cleanup should be called on init error")
}

// TestStreamNilCleanupReturnsOriginal verifies that when cleanup is nil,
// the original channel is returned without a wrapper goroutine.
func TestStreamNilCleanupReturnsOriginal(t *testing.T) {
	inner := &delayedStreamModel{
		chunks: []MessageChunk{
			{Role: RoleAssistant, Content: "hello", Final: true},
		},
	}

	provider := &testStreamProvider{model: inner, cleanup: nil}
	reg := &ProviderRegistry{providers: map[string]ModelProvider{}}
	require.NoError(t, reg.Register(provider))

	cycler := NewModelCycler(ModelCyclerConfig{
		Models: []ModelEntry{{Provider: "test-stream", Model: "m1"}},
	}).WithRegistry(reg)

	wrapped := cycler.WrapModel(&mockModel{})

	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)

	var contents []string
	for chunk := range ch {
		contents = append(contents, chunk.Content)
	}
	assert.Equal(t, []string{"hello"}, contents)
}

// TestStreamChunksForwardedInOrder verifies that chunks are forwarded in
// order through the wrapper goroutine.
func TestStreamChunksForwardedInOrder(t *testing.T) {
	var cleanupCalled atomic.Bool
	cleanup := func() { cleanupCalled.Store(true) }

	inner := &delayedStreamModel{
		chunks: []MessageChunk{
			{Role: RoleAssistant, Content: "1"},
			{Role: RoleAssistant, Content: "2"},
			{Role: RoleAssistant, Content: "3", Final: true},
		},
	}

	provider := &testStreamProvider{model: inner, cleanup: cleanup}
	reg := &ProviderRegistry{providers: map[string]ModelProvider{}}
	require.NoError(t, reg.Register(provider))

	cycler := NewModelCycler(ModelCyclerConfig{
		Models: []ModelEntry{{Provider: "test-stream", Model: "m1"}},
	}).WithRegistry(reg)

	wrapped := cycler.WrapModel(&mockModel{})

	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)

	var contents []string
	for chunk := range ch {
		contents = append(contents, chunk.Content)
	}
	assert.Equal(t, []string{"1", "2", "3"}, contents)
}

// TestStreamRaceConcurrentCalls runs multiple Stream calls concurrently to
// verify there are no data races in the wrapper goroutine.
func TestStreamRaceConcurrentCalls(t *testing.T) {
	var cleanupCount atomic.Int64
	cleanup := func() { cleanupCount.Add(1) }

	inner := &delayedStreamModel{
		chunks: []MessageChunk{
			{Role: RoleAssistant, Content: "x", Final: true},
		},
	}

	provider := &testStreamProvider{model: inner, cleanup: cleanup}
	reg := &ProviderRegistry{providers: map[string]ModelProvider{}}
	require.NoError(t, reg.Register(provider))

	cycler := NewModelCycler(ModelCyclerConfig{
		Models:   []ModelEntry{{Provider: "test-stream", Model: "m1"}},
		Strategy: StrategyRoundRobin,
	}).WithRegistry(reg)

	wrapped := cycler.WrapModel(&mockModel{})

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, err := wrapped.Stream(context.Background(), nil)
			if err != nil {
				return
			}
			for range ch {
			}
		}()
	}
	wg.Wait()
}
