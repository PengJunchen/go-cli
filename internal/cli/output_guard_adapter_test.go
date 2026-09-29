package cli

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/production"
)

// stubModel is a test double for llm.BaseChatModel.
type stubModel struct {
	resp *llm.Message
	err  error
}

type streamFuncModel struct {
	streamFn func(context.Context, []llm.Message, ...llm.Option) (<-chan llm.MessageChunk, error)
}

func (m *streamFuncModel) Generate(context.Context, []llm.Message, ...llm.Option) (*llm.Message, error) {
	return nil, errors.New("unexpected Generate call")
}

func (m *streamFuncModel) Stream(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	return m.streamFn(ctx, msgs, opts...)
}

func (m *stubModel) Generate(_ context.Context, _ []llm.Message, _ ...llm.Option) (*llm.Message, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.resp, nil
}

func (m *stubModel) Stream(_ context.Context, _ []llm.Message, _ ...llm.Option) (<-chan llm.MessageChunk, error) {
	return nil, nil
}

func TestOutputGuardModel_DeniesPIIWithoutSanitizedCandidate(t *testing.T) {
	inner := &stubModel{
		resp: &llm.Message{Role: llm.RoleAssistant, Content: "Contact me at john@example.com"},
	}
	m := &outputGuardModel{inner: inner, guard: production.NewPIIOutputGuard()}

	resp, err := m.Generate(context.Background(), nil)
	require.ErrorContains(t, err, "candidate denied")
	assert.Nil(t, resp)
}

func TestOutputGuardModel_PassthroughSafe(t *testing.T) {
	inner := &stubModel{
		resp: &llm.Message{Role: llm.RoleAssistant, Content: "Hello, world!"},
	}
	m := &outputGuardModel{inner: inner, guard: production.NewPIIOutputGuard()}

	resp, err := m.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "Hello, world!", resp.Content)
}

func TestOutputGuardStreamFragmentationDoesNotConsumePayloadBudget(t *testing.T) {
	logicalText := strings.Repeat("x", 10_000)
	for _, chunked := range []bool{false, true} {
		t.Run(map[bool]string{false: "one_chunk", true: "many_chunks"}[chunked], func(t *testing.T) {
			streamFn := func(context.Context, []llm.Message, ...llm.Option) (<-chan llm.MessageChunk, error) {
				count := 1
				if chunked {
					count = len(logicalText)
				}
				ch := make(chan llm.MessageChunk, count+1)
				for i := 0; i < count; i++ {
					part := logicalText
					if chunked {
						part = logicalText[i : i+1]
					}
					ch <- llm.MessageChunk{Role: llm.RoleAssistant, Content: part}
				}
				ch <- llm.MessageChunk{Role: llm.RoleAssistant, Final: true, FinishReason: "stop"}
				close(ch)
				return ch, nil
			}
			wrapped := &outputGuardModel{inner: &streamFuncModel{streamFn: streamFn}, guard: production.NewLengthGuard(20_000)}
			ch, err := wrapped.Stream(context.Background(), nil)
			require.NoError(t, err)
			var content strings.Builder
			var gotErr error
			finalSeen := false
			for chunk := range ch {
				if chunk.Error != nil {
					gotErr = chunk.Error
					continue
				}
				content.WriteString(chunk.Content)
				finalSeen = finalSeen || chunk.Final
			}
			require.NoError(t, gotErr)
			require.True(t, finalSeen)
			require.Equal(t, logicalText, content.String())
		})
	}
}

func TestOutputGuardStreamRejectsClosedStreamWithoutFinalChunk(t *testing.T) {
	inner := &streamFuncModel{streamFn: func(context.Context, []llm.Message, ...llm.Option) (<-chan llm.MessageChunk, error) {
		ch := make(chan llm.MessageChunk)
		close(ch)
		return ch, nil
	}}
	wrapped := &outputGuardModel{inner: inner, guard: production.NewLengthGuard(100)}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	ch, err := wrapped.Stream(ctx, nil)
	require.NoError(t, err)
	var streamErr error
	for chunk := range ch {
		if chunk.Error != nil {
			streamErr = chunk.Error
		}
	}
	require.NoError(t, ctx.Err(), "closed input stream must terminate without waiting for cancellation")
	require.ErrorContains(t, streamErr, "ended without a final chunk")
}

func TestOutputGuardRejectsNilStreamWithoutError(t *testing.T) {
	inner := &streamFuncModel{streamFn: func(context.Context, []llm.Message, ...llm.Option) (<-chan llm.MessageChunk, error) {
		return nil, nil
	}}
	wrapped := &outputGuardModel{inner: inner, guard: production.NewLengthGuard(100)}
	out, err := wrapped.Stream(context.Background(), nil)
	require.Nil(t, out)
	require.ErrorContains(t, err, "nil stream")
}

func TestOutputGuardStreamCancelsInnerAfterChunkLimitFailure(t *testing.T) {
	innerStopped := make(chan struct{})
	inner := &streamFuncModel{streamFn: func(ctx context.Context, _ []llm.Message, _ ...llm.Option) (<-chan llm.MessageChunk, error) {
		ch := make(chan llm.MessageChunk)
		go func() {
			defer close(innerStopped)
			defer close(ch)
			for {
				select {
				case ch <- llm.MessageChunk{}:
				case <-ctx.Done():
					return
				}
			}
		}()
		return ch, nil
	}}
	wrapped := &outputGuardModel{inner: inner, guard: production.NewPIIOutputGuard()}

	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)
	var streamErr error
	for chunk := range ch {
		if chunk.Error != nil {
			streamErr = chunk.Error
		}
	}
	require.ErrorContains(t, streamErr, "chunks")
	select {
	case <-innerStopped:
	case <-time.After(time.Second):
		t.Fatal("inner stream did not stop after output guard failure")
	}
}

type heldGuardStreamModel struct {
	chunks        []llm.MessageChunk
	setupErr      error
	release       <-chan struct{}
	cancelCause   chan error
	cleanupDone   chan struct{}
	waitForCancel bool
}

func (m *heldGuardStreamModel) Generate(context.Context, []llm.Message, ...llm.Option) (*llm.Message, error) {
	return nil, errors.New("unexpected Generate call")
}

func (m *heldGuardStreamModel) Stream(ctx context.Context, _ []llm.Message, _ ...llm.Option) (<-chan llm.MessageChunk, error) {
	ch := make(chan llm.MessageChunk, len(m.chunks))
	for _, chunk := range m.chunks {
		ch <- chunk
	}
	go func() {
		if m.waitForCancel {
			<-ctx.Done()
			m.cancelCause <- context.Cause(ctx)
		}
		<-m.release
		close(m.cleanupDone)
		close(ch)
	}()
	return ch, m.setupErr
}

func TestOutputGuardWaitsForCanceledSourceClose(t *testing.T) {
	release := make(chan struct{})
	model := &heldGuardStreamModel{
		chunks:        []llm.MessageChunk{{Role: llm.RoleAssistant, Content: "partial"}},
		release:       release,
		cancelCause:   make(chan error, 1),
		cleanupDone:   make(chan struct{}),
		waitForCancel: true,
	}
	wrapped := &outputGuardModel{inner: model, guard: production.NewLengthGuard(100)}
	ctx, cancel := context.WithCancel(context.Background())
	out, err := wrapped.Stream(ctx, nil)
	require.NoError(t, err)
	cancel()
	select {
	case cause := <-model.cancelCause:
		require.ErrorIs(t, cause, context.Canceled)
		var localAbort *llm.LocalStreamAbortCause
		require.NotErrorAs(t, cause, &localAbort)
	case <-time.After(time.Second):
		t.Fatal("source did not observe caller cancellation")
	}
	_ = requireGuardOutputOpenUntil(t, out, 25*time.Millisecond)
	close(release)
	select {
	case <-model.cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("source cleanup did not finish")
	}
	collectGuardOutput(t, out)
}

func TestOutputGuardLocalCapAbortJoinsBeforeOutputClose(t *testing.T) {
	release := make(chan struct{})
	chunks := make([]llm.MessageChunk, llm.GuardedStreamMaxChunks+1)
	model := &heldGuardStreamModel{
		chunks:        chunks,
		release:       release,
		cancelCause:   make(chan error, 1),
		cleanupDone:   make(chan struct{}),
		waitForCancel: true,
	}
	wrapped := &outputGuardModel{inner: model, guard: production.NewLengthGuard(100)}
	out, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)
	select {
	case cause := <-model.cancelCause:
		var localAbort *llm.LocalStreamAbortCause
		require.ErrorAs(t, cause, &localAbort)
		require.EqualError(t, cause, "local stream abort")
	case <-time.After(time.Second):
		t.Fatal("output guard did not cancel the capped source")
	}
	pending := requireGuardOutputOpenUntil(t, out, 25*time.Millisecond)
	close(release)
	select {
	case <-model.cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("source cleanup did not finish")
	}
	chunksOut := append(pending, collectGuardOutput(t, out)...)
	require.Len(t, chunksOut, 1)
	require.ErrorContains(t, chunksOut[0].Error, "chunks")
}

func TestOutputGuardJoinsErrorChunkSourceBeforeOutputClose(t *testing.T) {
	providerErr := errors.New("provider stream failed")
	release := make(chan struct{})
	model := &heldGuardStreamModel{
		chunks:        []llm.MessageChunk{{Error: providerErr}},
		release:       release,
		cancelCause:   make(chan error, 1),
		cleanupDone:   make(chan struct{}),
		waitForCancel: true,
	}
	wrapped := &outputGuardModel{inner: model, guard: production.NewLengthGuard(100)}
	out, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)
	select {
	case cause := <-model.cancelCause:
		var localAbort *llm.LocalStreamAbortCause
		require.ErrorAs(t, cause, &localAbort)
		require.ErrorIs(t, cause, providerErr)
	case <-time.After(time.Second):
		t.Fatal("provider error did not locally abort the source")
	}
	pending := requireGuardOutputOpenUntil(t, out, 25*time.Millisecond)
	close(release)
	select {
	case <-model.cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("source cleanup did not finish")
	}
	chunksOut := append(pending, collectGuardOutput(t, out)...)
	require.Len(t, chunksOut, 1)
	require.ErrorIs(t, chunksOut[0].Error, providerErr)
}

func TestOutputGuardJoinsSetupErrorChannelBeforeReturning(t *testing.T) {
	setupErr := errors.New("provider setup failed")
	release := make(chan struct{})
	model := &heldGuardStreamModel{
		chunks:        []llm.MessageChunk{{Role: llm.RoleAssistant, Content: "partial"}},
		setupErr:      setupErr,
		release:       release,
		cancelCause:   make(chan error, 1),
		cleanupDone:   make(chan struct{}),
		waitForCancel: true,
	}
	wrapped := &outputGuardModel{inner: model, guard: production.NewLengthGuard(100)}
	returned := make(chan error, 1)
	go func() {
		out, err := wrapped.Stream(context.Background(), nil)
		if out != nil {
			for range out {
			}
		}
		returned <- err
	}()
	select {
	case cause := <-model.cancelCause:
		var localAbort *llm.LocalStreamAbortCause
		require.ErrorAs(t, cause, &localAbort)
		require.ErrorIs(t, cause, setupErr)
	case <-time.After(time.Second):
		t.Fatal("setup error did not locally abort its returned source")
	}
	select {
	case err := <-returned:
		t.Fatalf("output guard returned before joining setup-error source: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	select {
	case <-model.cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("source cleanup did not finish")
	}
	select {
	case err := <-returned:
		require.ErrorIs(t, err, setupErr)
	case <-time.After(time.Second):
		t.Fatal("output guard did not return after source close")
	}
}

func TestOutputGuardMalformedAfterFinalJoinsSourceBeforeOutputClose(t *testing.T) {
	release := make(chan struct{})
	model := &heldGuardStreamModel{
		chunks: []llm.MessageChunk{
			{Role: llm.RoleAssistant, Final: true},
			{Role: llm.RoleAssistant, Content: "late"},
		},
		release:       release,
		cancelCause:   make(chan error, 1),
		cleanupDone:   make(chan struct{}),
		waitForCancel: true,
	}
	wrapped := &outputGuardModel{inner: model, guard: production.NewLengthGuard(100)}
	out, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)
	select {
	case cause := <-model.cancelCause:
		var localAbort *llm.LocalStreamAbortCause
		require.ErrorAs(t, cause, &localAbort)
	case <-time.After(time.Second):
		t.Fatal("malformed stream did not locally abort its source")
	}
	pending := requireGuardOutputOpenUntil(t, out, 25*time.Millisecond)
	close(release)
	select {
	case <-model.cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("source cleanup did not finish")
	}
	chunksOut := append(pending, collectGuardOutput(t, out)...)
	require.Len(t, chunksOut, 1)
	require.ErrorContains(t, chunksOut[0].Error, "after its final chunk")
}

func TestOutputGuardHoldsFinalUntilSourceClose(t *testing.T) {
	release := make(chan struct{})
	model := &heldGuardStreamModel{
		chunks:      []llm.MessageChunk{{Role: llm.RoleAssistant, Content: "complete", Final: true}},
		release:     release,
		cleanupDone: make(chan struct{}),
	}
	wrapped := &outputGuardModel{inner: model, guard: production.NewLengthGuard(100)}
	out, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)
	_ = requireGuardOutputOpenUntil(t, out, 25*time.Millisecond)
	close(release)
	select {
	case <-model.cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("source cleanup did not finish")
	}
	chunksOut := collectGuardOutput(t, out)
	require.Len(t, chunksOut, 2)
	require.Equal(t, "complete", chunksOut[0].Content)
	require.True(t, chunksOut[1].Final)
}

func TestOutputGuardDenialAfterSourceCloseDoesNotCancelProviderContext(t *testing.T) {
	providerCtx := make(chan context.Context, 1)
	inner := &streamFuncModel{streamFn: func(ctx context.Context, _ []llm.Message, _ ...llm.Option) (<-chan llm.MessageChunk, error) {
		providerCtx <- ctx
		ch := make(chan llm.MessageChunk, 2)
		ch <- llm.MessageChunk{Role: llm.RoleAssistant, Content: "john@example.com"}
		ch <- llm.MessageChunk{Role: llm.RoleAssistant, Final: true}
		close(ch)
		return ch, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	wrapped := &outputGuardModel{inner: inner, guard: production.NewPIIOutputGuard()}
	out, err := wrapped.Stream(ctx, nil)
	require.NoError(t, err)
	chunksOut := collectGuardOutput(t, out)
	require.Len(t, chunksOut, 1)
	require.ErrorContains(t, chunksOut[0].Error, "candidate denied")
	var innerCtx context.Context
	select {
	case innerCtx = <-providerCtx:
	case <-time.After(time.Second):
		t.Fatal("provider context was not captured")
	}
	select {
	case <-innerCtx.Done():
		t.Fatal("guard denial after source close canceled the completed provider context")
	default:
	}
	require.NoError(t, innerCtx.Err())
	require.Nil(t, context.Cause(innerCtx), "post-close guard denial must leave the provider cause unchanged")
	cancel()
}

func requireGuardOutputOpenUntil(t *testing.T, ch <-chan llm.MessageChunk, wait time.Duration) []llm.MessageChunk {
	t.Helper()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	var received []llm.MessageChunk
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				t.Fatal("output guard closed before its input source")
			}
			received = append(received, chunk)
		case <-timer.C:
			return received
		}
	}
}

func collectGuardOutput(t *testing.T, ch <-chan llm.MessageChunk) []llm.MessageChunk {
	t.Helper()
	collected := make(chan []llm.MessageChunk, 1)
	go func() {
		var chunks []llm.MessageChunk
		for chunk := range ch {
			chunks = append(chunks, chunk)
		}
		collected <- chunks
	}()
	select {
	case chunks := <-collected:
		return chunks
	case <-time.After(time.Second):
		t.Fatal("output guard did not close after source cleanup")
		return nil
	}
}

func TestOutputGuardModel_LengthTruncation(t *testing.T) {
	long := string(make([]byte, 200))
	inner := &stubModel{
		resp: &llm.Message{Role: llm.RoleAssistant, Content: long},
	}
	m := &outputGuardModel{inner: inner, guard: production.NewLengthGuard(100)}

	resp, err := m.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(resp.Content), 100)
}

func TestOutputGuardModel_NilGuard(t *testing.T) {
	inner := &stubModel{
		resp: &llm.Message{Role: llm.RoleAssistant, Content: "untouched"},
	}
	m := &outputGuardModel{inner: inner, guard: nil}

	resp, err := m.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "untouched", resp.Content)
}

func TestNewModelWrapper_AppliesBothWrappers(t *testing.T) {
	inner := &stubModel{
		resp: &llm.Message{Role: llm.RoleAssistant, Content: "Contact john@example.com"},
	}
	pw := production.NewProductionModelWrapper()
	guard := production.NewPIIOutputGuard()
	wrapper := newModelWrapper(pw, nil, guard)

	wrapped := wrapper(inner)
	require.NotNil(t, wrapped)

	baseModel, ok := wrapped.(llm.BaseChatModel)
	require.True(t, ok)

	resp, err := baseModel.Generate(context.Background(), nil)
	require.ErrorContains(t, err, "candidate denied")
	assert.Nil(t, resp)
}

func TestNewModelWrapper_NilGuardReturnsProductionWrapped(t *testing.T) {
	inner := &stubModel{
		resp: &llm.Message{Role: llm.RoleAssistant, Content: "safe content"},
	}
	pw := production.NewProductionModelWrapper()
	wrapper := newModelWrapper(pw, nil, nil)

	wrapped := wrapper(inner)
	require.NotNil(t, wrapped)

	_, ok := wrapped.(llm.BaseChatModel)
	require.True(t, ok)
}

func TestNewModelWrapper_NonBaseChatModelReturnsUnchanged(t *testing.T) {
	pw := production.NewProductionModelWrapper()
	wrapper := newModelWrapper(pw, nil, nil)

	result := wrapper("not a model")
	assert.Equal(t, "not a model", result)
}

// countingModel is a test double that counts Generate calls and returns err
// for the first failCount calls, then returns resp.
type countingModel struct {
	calls     int32
	failCount int32
	resp      *llm.Message
	err       error
}

func (m *countingModel) Generate(_ context.Context, _ []llm.Message, _ ...llm.Option) (*llm.Message, error) {
	n := atomic.AddInt32(&m.calls, 1)
	if n <= m.failCount {
		return nil, m.err
	}
	return m.resp, nil
}

func (m *countingModel) Stream(_ context.Context, _ []llm.Message, _ ...llm.Option) (<-chan llm.MessageChunk, error) {
	return nil, nil
}

// TestNewModelWrapperWithChain_AppliesGuard verifies that the chain-based
// wrapper still applies the output guard as the outermost layer.
func TestNewModelWrapperWithChain_AppliesGuard(t *testing.T) {
	inner := &stubModel{
		resp: &llm.Message{Role: llm.RoleAssistant, Content: "Contact john@example.com"},
	}
	pw := production.NewProductionModelWrapper()
	chain := llm.NewStandardMiddlewareChain(
		llm.NewFailoverModelMiddleware(),
		llm.NewRetryModelMiddleware(),
		llm.NewTimeoutModelMiddleware(),
		llm.NewSanitizeModelMiddleware(),
		llm.NewLoopDetectionModelMiddleware(),
		llm.NewValidateModelMiddleware(),
		llm.NewOverflowRecoveryMiddleware(),
	)
	guard := production.NewPIIOutputGuard()
	wrapper := newModelWrapperWithChain(pw, chain, nil, guard)

	wrapped := wrapper(inner)
	require.NotNil(t, wrapped)

	baseModel, ok := wrapped.(llm.BaseChatModel)
	require.True(t, ok)

	resp, err := baseModel.Generate(context.Background(), nil)
	require.ErrorContains(t, err, "candidate denied")
	assert.Nil(t, resp)
}

// TestNewModelWrapperWithChain_NilGuardReturnsChainWrapped verifies that with a
// nil guard the wrapper still returns a usable BaseChatModel.
func TestNewModelWrapperWithChain_NilGuardReturnsChainWrapped(t *testing.T) {
	inner := &stubModel{
		resp: &llm.Message{Role: llm.RoleAssistant, Content: "safe content"},
	}
	pw := production.NewProductionModelWrapper()
	chain := llm.NewStandardMiddlewareChain(
		llm.NewFailoverModelMiddleware(),
		llm.NewRetryModelMiddleware(),
		llm.NewTimeoutModelMiddleware(),
		llm.NewSanitizeModelMiddleware(),
		llm.NewLoopDetectionModelMiddleware(),
		llm.NewValidateModelMiddleware(),
		llm.NewOverflowRecoveryMiddleware(),
	)
	wrapper := newModelWrapperWithChain(pw, chain, nil, nil)

	wrapped := wrapper(inner)
	require.NotNil(t, wrapped)

	_, ok := wrapped.(llm.BaseChatModel)
	require.True(t, ok)
}

// TestNewModelWrapperWithChain_NonBaseChatModelReturnsUnchanged verifies the
// wrapper passes through non-model values unchanged.
func TestNewModelWrapperWithChain_NonBaseChatModelReturnsUnchanged(t *testing.T) {
	pw := production.NewProductionModelWrapper()
	chain := llm.NewStandardMiddlewareChain()
	wrapper := newModelWrapperWithChain(pw, chain, nil, nil)

	result := wrapper("not a model")
	assert.Equal(t, "not a model", result)
}

// TestNoDoubleRetry verifies that when the production wrapper is constructed
// WITHOUT a retry policy and the 7-layer chain provides the retry middleware,
// the underlying model is retried exactly the number of times dictated by the
// chain's retry policy — not doubled by a second retry layer.
func TestNoDoubleRetry(t *testing.T) {
	inner := &countingModel{
		failCount: 2, // fail twice, succeed on 3rd call
		resp:      &llm.Message{Role: llm.RoleAssistant, Content: "recovered"},
		err:       errors.New("transient"),
	}

	// Production wrapper WITHOUT retry policy — cost tracking only.
	pw := production.NewProductionModelWrapper()

	// Chain with a retry policy that allows up to 3 attempts (0, 1, 2).
	chain := llm.NewStandardMiddlewareChain(
		llm.NewRetryModelMiddleware(llm.WithRetryPolicy(&countingRetryPolicy{maxAttempts: 3})),
	)

	wrapper := newModelWrapperWithChain(pw, chain, nil, nil)
	wrapped := wrapper(inner)
	baseModel, ok := wrapped.(llm.BaseChatModel)
	require.True(t, ok)

	resp, err := baseModel.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "recovered", resp.Content)

	// The model should have been called exactly 3 times: initial + 2 retries.
	// If the production wrapper also applied retry, we would see 9 calls
	// (3 × 3) instead of 3.
	assert.Equal(t, int32(3), atomic.LoadInt32(&inner.calls))
}

// countingRetryPolicy is a simple RetryPolicy for tests: retries any non-nil
// error up to maxAttempts times (0-based attempt index).
type countingRetryPolicy struct {
	maxAttempts int
}

func (p *countingRetryPolicy) ShouldRetry(_ context.Context, err error, attempt int) bool {
	if err == nil {
		return false
	}
	return attempt < p.maxAttempts
}

func (p *countingRetryPolicy) NextBackoff(_ context.Context, _ int) time.Duration {
	return 0
}

func (p *countingRetryPolicy) Name() string { return "counting-retry" }
