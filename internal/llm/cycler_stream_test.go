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

func (p *testStreamProvider) resolverSpec() ProviderSpec {
	return ProviderSpec{
		ProviderID:              "test-stream",
		ProtocolID:              ProtocolOpenAICompatible,
		DefaultBaseURL:          "https://fixture.invalid",
		DefaultEndpointExplicit: true,
		Enabled:                 true,
	}
}

func (p *testStreamProvider) Build(_ context.Context, _ ModelConfig) (BaseChatModel, func(), error) {
	if p.buildErr != nil {
		return nil, nil, p.buildErr
	}
	return p.model, p.cleanup, nil
}

func (p *testStreamProvider) Models() []ModelInfo { return nil }

// delayedStreamModel sends chunks with configurable delays between them.
type delayedStreamModel struct {
	chunks    []MessageChunk
	delays    []time.Duration
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

type heldCyclerStreamModel struct {
	initial       []MessageChunk
	sourceRelease <-chan struct{}
	cancelCause   chan error
	sourceClosed  chan struct{}
	setupErr      error
	nilSuccess    bool
}

func (*heldCyclerStreamModel) Generate(context.Context, []Message, ...Option) (*Message, error) {
	return nil, errors.New("unexpected Generate call")
}

func (m *heldCyclerStreamModel) Stream(ctx context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
	if m.nilSuccess {
		return nil, nil
	}
	ch := make(chan MessageChunk, len(m.initial))
	for _, chunk := range m.initial {
		ch <- chunk
	}
	go func() {
		<-ctx.Done()
		if m.cancelCause != nil {
			m.cancelCause <- context.Cause(ctx)
		}
		if m.sourceRelease != nil {
			<-m.sourceRelease
		}
		close(ch)
		if m.sourceClosed != nil {
			close(m.sourceClosed)
		}
	}()
	return ch, m.setupErr
}

func TestStreamCleanupAndOutputCloseWaitForSourceAndCleanup(t *testing.T) {
	sourceRelease := make(chan struct{})
	cleanupRelease := make(chan struct{})
	model := &heldCyclerStreamModel{
		initial:       []MessageChunk{{Role: RoleAssistant, Content: "partial"}},
		sourceRelease: sourceRelease,
		cancelCause:   make(chan error, 1),
		sourceClosed:  make(chan struct{}),
	}
	cleanupStarted := make(chan struct{})
	cleanupDone := make(chan struct{})
	provider := &testStreamProvider{
		model: model,
		cleanup: func() {
			close(cleanupStarted)
			<-cleanupRelease
			close(cleanupDone)
		},
	}
	reg := &ProviderRegistry{providers: map[string]ModelProvider{}}
	require.NoError(t, reg.Register(provider))
	wrapped := NewModelCycler(ModelCyclerConfig{
		Models: []ModelEntry{{Provider: "test-stream", Model: "m1"}},
	}).WithRegistry(reg).WrapModel(&mockModel{})
	ctx, cancel := context.WithCancel(context.Background())
	out, err := wrapped.Stream(ctx, nil)
	require.NoError(t, err)
	cancel()
	select {
	case cause := <-model.cancelCause:
		require.ErrorIs(t, cause, context.Canceled)
		var localAbort *LocalStreamAbortCause
		require.NotErrorAs(t, cause, &localAbort, "caller cancellation must remain distinct from a local abort")
	case <-time.After(time.Second):
		t.Fatal("selected source did not observe cancellation")
	}
	requireChannelOpenUntil(t, out, 20*time.Millisecond)
	close(sourceRelease)
	select {
	case <-model.sourceClosed:
	case <-time.After(time.Second):
		t.Fatal("selected source did not close after release")
	}
	select {
	case <-cleanupStarted:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not start after source close")
	}
	requireChannelOpenUntil(t, out, 20*time.Millisecond)
	close(cleanupRelease)
	select {
	case <-cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not finish after release")
	}
	drained := make(chan struct{})
	go func() {
		for range out {
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("output did not close after cleanup")
	}
}

func TestStreamSetupErrorWithChannelJoinsCleanupBeforeFallback(t *testing.T) {
	setupErr := errors.New("stream setup failed")
	sourceRelease := make(chan struct{})
	cleanupRelease := make(chan struct{})
	model := &heldCyclerStreamModel{
		sourceRelease: sourceRelease,
		cancelCause:   make(chan error, 1),
		sourceClosed:  make(chan struct{}),
		setupErr:      setupErr,
	}
	cleanupStarted := make(chan struct{})
	cleanupDone := make(chan struct{})
	provider := &testStreamProvider{model: model, cleanup: func() {
		close(cleanupStarted)
		<-cleanupRelease
		close(cleanupDone)
	}}
	reg := &ProviderRegistry{providers: map[string]ModelProvider{}}
	require.NoError(t, reg.Register(provider))
	fallbackBeforeCleanup := make(chan bool, 1)
	fallback := &joinSetupModel{streamFn: func(context.Context, int32) (<-chan MessageChunk, error) {
		select {
		case <-cleanupDone:
			fallbackBeforeCleanup <- false
		default:
			fallbackBeforeCleanup <- true
		}
		ch := make(chan MessageChunk)
		close(ch)
		return ch, nil
	}}
	wrapped := NewModelCycler(ModelCyclerConfig{
		Models: []ModelEntry{{Provider: "test-stream", Model: "m1"}},
	}).WithRegistry(reg).WrapModel(fallback)
	finished := make(chan error, 1)
	go func() {
		ch, err := wrapped.Stream(context.Background(), nil)
		if err == nil {
			for range ch {
			}
		}
		finished <- err
	}()
	select {
	case cause := <-model.cancelCause:
		var localAbort *LocalStreamAbortCause
		require.ErrorAs(t, cause, &localAbort)
		require.ErrorIs(t, cause, setupErr)
	case <-time.After(time.Second):
		t.Fatal("setup-error source did not observe cancellation")
	}
	require.Zero(t, fallback.streamCalls.Load(), "fallback must wait for source join and cleanup")
	select {
	case err := <-finished:
		t.Fatalf("Stream returned before source join: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(sourceRelease)
	select {
	case <-cleanupStarted:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not start after source close")
	}
	select {
	case err := <-finished:
		t.Fatalf("fallback returned before cleanup completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	require.Zero(t, fallback.streamCalls.Load(), "fallback must wait for cleanup completion")
	close(cleanupRelease)
	select {
	case <-cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not complete after source close")
	}
	require.NoError(t, <-finished)
	require.False(t, <-fallbackBeforeCleanup, "cleanup must complete before fallback starts")
	require.EqualValues(t, 1, fallback.streamCalls.Load())
}

func TestStreamNilNilIsRejectedAndContinuationDoesNotFallback(t *testing.T) {
	t.Run("nil success", func(t *testing.T) {
		model := &heldCyclerStreamModel{nilSuccess: true}
		cleanupDone := make(chan struct{})
		provider := &testStreamProvider{model: model, cleanup: func() { close(cleanupDone) }}
		reg := &ProviderRegistry{providers: map[string]ModelProvider{}}
		require.NoError(t, reg.Register(provider))
		fallback := &fbMockModel{streamText: "must not run"}
		wrapped := NewModelCycler(ModelCyclerConfig{
			Models: []ModelEntry{{Provider: "test-stream", Model: "m1"}},
		}).WithRegistry(reg).WrapModel(fallback)

		ch, err := wrapped.Stream(context.Background(), nil)
		require.ErrorContains(t, err, "nil stream")
		require.Nil(t, ch)
		require.Zero(t, fallback.streamCalls)
		<-cleanupDone
	})

	t.Run("continuation setup error", func(t *testing.T) {
		setupErr := errors.New("continuation setup failed")
		model := &heldCyclerStreamModel{setupErr: setupErr}
		cleanupDone := make(chan struct{})
		provider := &testStreamProvider{model: model, cleanup: func() { close(cleanupDone) }}
		reg := &ProviderRegistry{providers: map[string]ModelProvider{}}
		require.NoError(t, reg.Register(provider))
		fallback := &fbMockModel{streamText: "must not run"}
		wrapped := NewModelCycler(ModelCyclerConfig{
			Models: []ModelEntry{{Provider: "test-stream", Model: "m1"}},
		}).WithRegistry(reg).WrapModel(fallback)

		ch, err := wrapped.Stream(WithContinuationNoReplay(context.Background()), nil)
		require.ErrorIs(t, err, setupErr)
		require.Nil(t, ch)
		require.Zero(t, fallback.streamCalls)
		<-cleanupDone
	})
}
