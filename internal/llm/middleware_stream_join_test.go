package llm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type joinSetupModel struct {
	streamCalls atomic.Int32
	streamFn    func(context.Context, int32) (<-chan MessageChunk, error)
}

func (*joinSetupModel) Generate(context.Context, []Message, ...Option) (*Message, error) {
	return nil, errors.New("unexpected Generate call")
}

func (m *joinSetupModel) Stream(ctx context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
	call := m.streamCalls.Add(1)
	return m.streamFn(ctx, call)
}

func TestLocalStreamAbortCauseKeepsClassificationWithoutExposingProviderText(t *testing.T) {
	providerErr := errors.New("sensitive provider response body")
	cause := NewLocalStreamAbortCause(providerErr)

	require.EqualError(t, cause, "local stream abort")
	require.ErrorIs(t, cause, providerErr)
	var localAbort *LocalStreamAbortCause
	require.ErrorAs(t, cause, &localAbort)
}

func TestFailoverStreamJoinsChannelReturnedWithSetupErrorBeforeFallback(t *testing.T) {
	setupErr := errors.New("primary setup failed")
	release := make(chan struct{})
	cleanupDone := make(chan struct{})
	observedCause := make(chan error, 1)
	primary := &joinSetupModel{streamFn: func(ctx context.Context, _ int32) (<-chan MessageChunk, error) {
		ch := make(chan MessageChunk)
		go func() {
			<-ctx.Done()
			observedCause <- context.Cause(ctx)
			<-release
			close(cleanupDone)
			close(ch)
		}()
		return ch, setupErr
	}}
	fallback := &fbMockModel{streamText: "fallback"}
	wrapped := NewFailoverModelMiddleware(WithFallbackModels(fallback)).WrapModel(primary)
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
	case <-cleanupDone:
		t.Fatal("failed setup source should wait for explicit release")
	case <-time.After(20 * time.Millisecond):
	}
	require.Zero(t, fallback.streamCalls, "fallback must wait until the failed source is joined")

	close(release)
	require.NoError(t, <-finished)
	require.Equal(t, 1, fallback.streamCalls)
	select {
	case cause := <-observedCause:
		var localAbort *LocalStreamAbortCause
		require.ErrorAs(t, cause, &localAbort)
	case <-time.After(time.Second):
		t.Fatal("primary source did not observe a typed local abort cause")
	}
}

func TestRetryStreamJoinsChannelReturnedWithSetupErrorBeforeRetry(t *testing.T) {
	setupErr := errors.New("transient setup failure")
	release := make(chan struct{})
	cleanupDone := make(chan struct{})
	observedCause := make(chan error, 1)
	model := &joinSetupModel{streamFn: func(ctx context.Context, call int32) (<-chan MessageChunk, error) {
		if call > 1 {
			ch := make(chan MessageChunk, 1)
			ch <- MessageChunk{Role: RoleAssistant, Final: true}
			close(ch)
			return ch, nil
		}
		ch := make(chan MessageChunk)
		go func() {
			<-ctx.Done()
			observedCause <- context.Cause(ctx)
			<-release
			close(cleanupDone)
			close(ch)
		}()
		return ch, setupErr
	}}
	wrapped := NewRetryModelMiddleware(WithRetryPolicy(&testRetryPolicy{maxRetries: 1})).WrapModel(model)
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
	case <-cleanupDone:
		t.Fatal("failed setup source should wait for explicit release")
	case <-time.After(20 * time.Millisecond):
	}
	require.EqualValues(t, 1, model.streamCalls.Load(), "retry must wait until the failed source is joined")
	close(release)
	require.NoError(t, <-finished)
	require.EqualValues(t, 2, model.streamCalls.Load())
	select {
	case cause := <-observedCause:
		var localAbort *LocalStreamAbortCause
		require.ErrorAs(t, cause, &localAbort)
	case <-time.After(time.Second):
		t.Fatal("failed setup source did not observe a typed local abort cause")
	}
}

func TestFailoverAndRetryRejectNilStreamWithoutError(t *testing.T) {
	t.Run("failover", func(t *testing.T) {
		primary := &joinSetupModel{streamFn: func(context.Context, int32) (<-chan MessageChunk, error) {
			return nil, nil
		}}
		fallback := &fbMockModel{streamText: "must not run"}
		wrapped := NewFailoverModelMiddleware(WithFallbackModels(fallback)).WrapModel(primary)

		ch, err := wrapped.Stream(context.Background(), nil)
		require.ErrorContains(t, err, "nil stream")
		require.Nil(t, ch)
		require.Zero(t, fallback.streamCalls)
	})

	t.Run("retry", func(t *testing.T) {
		model := &joinSetupModel{streamFn: func(context.Context, int32) (<-chan MessageChunk, error) {
			return nil, nil
		}}
		wrapped := NewRetryModelMiddleware(WithRetryPolicy(&testRetryPolicy{maxRetries: 2})).WrapModel(model)

		ch, err := wrapped.Stream(context.Background(), nil)
		require.ErrorContains(t, err, "nil stream")
		require.Nil(t, ch)
		require.EqualValues(t, 1, model.streamCalls.Load())
	})
}

func TestFailoverAndRetryJoinSuccessfulSourceAfterCallerCancel(t *testing.T) {
	layers := []struct {
		name string
		wrap func(BaseChatModel) BaseChatModel
	}{
		{name: "failover", wrap: func(next BaseChatModel) BaseChatModel {
			return NewFailoverModelMiddleware().WrapModel(next)
		}},
		{name: "retry", wrap: func(next BaseChatModel) BaseChatModel {
			return NewRetryModelMiddleware(WithRetryPolicy(&testRetryPolicy{maxRetries: 1})).WrapModel(next)
		}},
	}
	for _, layer := range layers {
		t.Run(layer.name, func(t *testing.T) {
			release := make(chan struct{})
			source := &heldJoinReaderModel{
				release:      release,
				cleanupDone:  make(chan struct{}),
				cancelCause:  make(chan error, 1),
				initialChunk: []MessageChunk{{Role: RoleAssistant, Content: "partial"}},
			}
			ctx, cancel := context.WithCancel(context.Background())
			out, err := layer.wrap(source).Stream(ctx, nil)
			require.NoError(t, err)
			cancel()
			select {
			case cause := <-source.cancelCause:
				require.ErrorIs(t, cause, context.Canceled)
				var localAbort *LocalStreamAbortCause
				require.NotErrorAs(t, cause, &localAbort, "caller cancellation must not be reclassified as a local abort")
			case <-time.After(time.Second):
				t.Fatal("source did not observe caller cancellation")
			}
			finished := make(chan struct{})
			go func() {
				for range out {
				}
				close(finished)
			}()
			select {
			case <-finished:
				t.Fatal("forwarder closed before source close")
			case <-time.After(25 * time.Millisecond):
			}
			close(release)
			select {
			case <-source.cleanupDone:
			case <-time.After(time.Second):
				t.Fatal("source cleanup did not finish after release")
			}
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("forwarder did not close after source cleanup")
			}
		})
	}
}

func TestFailoverAndRetryStopOnErrorAndJoinSource(t *testing.T) {
	providerErr := errors.New("provider stream failed")
	layers := []struct {
		name string
		wrap func(BaseChatModel) BaseChatModel
	}{
		{name: "failover", wrap: func(next BaseChatModel) BaseChatModel {
			return NewFailoverModelMiddleware().WrapModel(next)
		}},
		{name: "retry", wrap: func(next BaseChatModel) BaseChatModel {
			return NewRetryModelMiddleware(WithRetryPolicy(&testRetryPolicy{maxRetries: 1})).WrapModel(next)
		}},
	}
	for _, layer := range layers {
		t.Run(layer.name, func(t *testing.T) {
			release := make(chan struct{})
			source := &heldJoinReaderModel{
				release:      release,
				cleanupDone:  make(chan struct{}),
				cancelCause:  make(chan error, 1),
				initialChunk: []MessageChunk{{Error: providerErr}, {Role: RoleAssistant, Content: "late"}},
			}
			out, err := layer.wrap(source).Stream(context.Background(), nil)
			require.NoError(t, err)
			cause := <-source.cancelCause
			var localAbort *LocalStreamAbortCause
			require.ErrorAs(t, cause, &localAbort)
			require.ErrorIs(t, cause, providerErr)
			pending := requireChannelOpenUntil(t, out, 25*time.Millisecond)
			close(release)
			select {
			case <-source.cleanupDone:
			case <-time.After(time.Second):
				t.Fatal("source cleanup did not finish after release")
			}
			for chunk := range out {
				pending = append(pending, chunk)
			}
			require.Len(t, pending, 1)
			require.ErrorIs(t, pending[0].Error, providerErr)
		})
	}
}

func TestFailoverAndRetryRejectDataAfterFinalAndJoinSource(t *testing.T) {
	layers := []struct {
		name string
		wrap func(BaseChatModel) BaseChatModel
	}{
		{name: "failover", wrap: func(next BaseChatModel) BaseChatModel {
			return NewFailoverModelMiddleware().WrapModel(next)
		}},
		{name: "retry", wrap: func(next BaseChatModel) BaseChatModel {
			return NewRetryModelMiddleware(WithRetryPolicy(&testRetryPolicy{maxRetries: 1})).WrapModel(next)
		}},
	}
	for _, layer := range layers {
		t.Run(layer.name, func(t *testing.T) {
			release := make(chan struct{})
			source := &heldJoinReaderModel{
				release:     release,
				cleanupDone: make(chan struct{}),
				cancelCause: make(chan error, 1),
				initialChunk: []MessageChunk{
					{Role: RoleAssistant, Final: true},
					{Role: RoleAssistant, Content: "late"},
				},
			}
			out, err := layer.wrap(source).Stream(context.Background(), nil)
			require.NoError(t, err)
			cause := <-source.cancelCause
			var localAbort *LocalStreamAbortCause
			require.ErrorAs(t, cause, &localAbort)
			pending := requireChannelOpenUntil(t, out, 25*time.Millisecond)
			close(release)
			select {
			case <-source.cleanupDone:
			case <-time.After(time.Second):
				t.Fatal("source cleanup did not finish after release")
			}
			for chunk := range out {
				pending = append(pending, chunk)
			}
			require.Len(t, pending, 1)
			require.False(t, pending[0].Final)
			require.ErrorContains(t, pending[0].Error, "after its final chunk")
		})
	}
}

func TestFailoverAndRetryHoldFinalUntilSourceClose(t *testing.T) {
	layers := []struct {
		name string
		wrap func(BaseChatModel) BaseChatModel
	}{
		{name: "failover", wrap: func(next BaseChatModel) BaseChatModel {
			return NewFailoverModelMiddleware().WrapModel(next)
		}},
		{name: "retry", wrap: func(next BaseChatModel) BaseChatModel {
			return NewRetryModelMiddleware(WithRetryPolicy(&testRetryPolicy{maxRetries: 1})).WrapModel(next)
		}},
	}
	for _, layer := range layers {
		t.Run(layer.name, func(t *testing.T) {
			release := make(chan struct{})
			contextSeen := make(chan context.Context, 1)
			source := &heldUntilReleaseModel{
				initialChunk: []MessageChunk{{Role: RoleAssistant, Content: "final", Final: true}},
				release:      release,
				cleanupDone:  make(chan struct{}),
				contextSeen:  contextSeen,
			}
			out, err := layer.wrap(source).Stream(context.Background(), nil)
			require.NoError(t, err)
			attemptCtx := <-contextSeen
			pending := requireChannelOpenUntil(t, out, 25*time.Millisecond)
			select {
			case <-attemptCtx.Done():
				t.Fatal("attempt context was released before source close")
			default:
			}
			for _, chunk := range pending {
				require.False(t, chunk.Final)
			}
			close(release)
			select {
			case <-source.cleanupDone:
			case <-time.After(time.Second):
				t.Fatal("source cleanup did not finish after release")
			}
			for chunk := range out {
				pending = append(pending, chunk)
			}
			require.Len(t, pending, 1)
			require.True(t, pending[0].Final)
			select {
			case <-attemptCtx.Done():
				require.ErrorIs(t, context.Cause(attemptCtx), context.Canceled)
				var localAbort *LocalStreamAbortCause
				require.NotErrorAs(t, context.Cause(attemptCtx), &localAbort, "normal source close must not be labeled a local abort")
			case <-time.After(time.Second):
				t.Fatal("attempt context was not released after source close")
			}
		})
	}
}

func TestOwningMiddlewareReadersRejectNilStreamWithoutError(t *testing.T) {
	for _, layer := range streamJoinReaderLayers() {
		t.Run(layer.name, func(t *testing.T) {
			model := &joinSetupModel{streamFn: func(context.Context, int32) (<-chan MessageChunk, error) {
				return nil, nil
			}}
			ch, err := layer.wrap(model).Stream(context.Background(), nil)
			require.ErrorContains(t, err, "nil")
			require.Nil(t, ch)
		})
	}
}

type heldJoinReaderModel struct {
	release      <-chan struct{}
	cleanupDone  chan struct{}
	cancelCause  chan error
	initialChunk []MessageChunk
	setupErr     error
}

func (*heldJoinReaderModel) Generate(context.Context, []Message, ...Option) (*Message, error) {
	return nil, errors.New("unexpected Generate call")
}

func (m *heldJoinReaderModel) Stream(ctx context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
	ch := make(chan MessageChunk, len(m.initialChunk))
	for _, chunk := range m.initialChunk {
		ch <- chunk
	}
	go func() {
		<-ctx.Done()
		m.cancelCause <- context.Cause(ctx)
		<-m.release
		close(m.cleanupDone)
		close(ch)
	}()
	return ch, m.setupErr
}

func TestOwningMiddlewareReadersWaitForSourceCloseAfterCallerCancel(t *testing.T) {
	layers := streamJoinReaderLayers()
	for _, layer := range layers {
		t.Run(layer.name, func(t *testing.T) {
			release := make(chan struct{})
			source := &heldJoinReaderModel{
				release:      release,
				cleanupDone:  make(chan struct{}),
				cancelCause:  make(chan error, 1),
				initialChunk: []MessageChunk{{Role: RoleAssistant, Content: "partial"}},
			}
			ctx, cancel := context.WithCancel(context.Background())
			out, err := layer.wrap(source).Stream(ctx, nil)
			require.NoError(t, err)
			cancel()

			select {
			case cause := <-source.cancelCause:
				require.ErrorIs(t, cause, context.Canceled)
				var localAbort *LocalStreamAbortCause
				require.NotErrorAs(t, cause, &localAbort, "caller cancellation must remain distinguishable from local abort")
			case <-time.After(time.Second):
				t.Fatal("source did not observe caller cancellation")
			}
			requireChannelOpenUntil(t, out, 30*time.Millisecond)
			close(release)
			select {
			case <-source.cleanupDone:
			case <-time.After(time.Second):
				t.Fatal("source cleanup did not finish after release")
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
				t.Fatal("wrapper did not close after source cleanup")
			}
		})
	}
}

func streamJoinReaderLayers() []struct {
	name string
	wrap func(BaseChatModel) BaseChatModel
} {
	return []struct {
		name string
		wrap func(BaseChatModel) BaseChatModel
	}{
		{
			name: "timeout",
			wrap: func(next BaseChatModel) BaseChatModel {
				return NewTimeoutModelMiddleware(
					WithTotalTimeout(5*time.Second),
					WithStreamChunkTimeout(5*time.Second),
				).WrapModel(next)
			},
		},
		{name: "sanitize", wrap: NewSanitizeModelMiddleware().WrapModel},
		{name: "loopdetect", wrap: NewLoopDetectionModelMiddleware().WrapModel},
		{name: "validate", wrap: NewValidateModelMiddleware().WrapModel},
		{name: "overflow", wrap: NewOverflowRecoveryMiddleware().WrapModel},
	}
}

func TestOwningMiddlewareReadersAbortAndJoinOnErrorChunk(t *testing.T) {
	providerErr := errors.New("provider stream failed")
	for _, layer := range streamJoinReaderLayers() {
		t.Run(layer.name, func(t *testing.T) {
			release := make(chan struct{})
			source := &heldJoinReaderModel{
				release:      release,
				cleanupDone:  make(chan struct{}),
				cancelCause:  make(chan error, 1),
				initialChunk: []MessageChunk{{Error: providerErr}},
			}
			type result struct {
				out <-chan MessageChunk
				err error
			}
			returned := make(chan result, 1)
			go func() {
				out, err := layer.wrap(source).Stream(context.Background(), nil)
				returned <- result{out: out, err: err}
			}()
			select {
			case cause := <-source.cancelCause:
				var localAbort *LocalStreamAbortCause
				require.ErrorAs(t, cause, &localAbort)
				require.ErrorIs(t, cause, providerErr)
				require.EqualError(t, cause, "local stream abort")
			case <-time.After(time.Second):
				t.Fatal("reader did not cancel source after its error chunk")
			}

			var got result
			var pending []MessageChunk
			select {
			case got = <-returned:
			case <-time.After(20 * time.Millisecond):
			}
			if got.out != nil {
				pending = requireChannelOpenUntil(t, got.out, 25*time.Millisecond)
			}
			close(release)
			select {
			case <-source.cleanupDone:
			case <-time.After(time.Second):
				t.Fatal("source cleanup did not finish after release")
			}
			if got.out == nil && got.err == nil {
				select {
				case got = <-returned:
				case <-time.After(time.Second):
					t.Fatal("synchronous peek did not return after source close")
				}
			}
			if layer.name == "validate" {
				require.ErrorIs(t, got.err, providerErr)
				require.Nil(t, got.out)
				return
			}
			require.NoError(t, got.err)
			terminal := make([]MessageChunk, 0, 1)
			for _, chunk := range pending {
				if chunk.Error != nil || chunk.Final {
					terminal = append(terminal, chunk)
				}
			}
			for chunk := range got.out {
				if chunk.Error != nil || chunk.Final {
					terminal = append(terminal, chunk)
				}
			}
			require.Len(t, terminal, 1)
			require.ErrorIs(t, terminal[0].Error, providerErr)
		})
	}
}

func TestOwningMiddlewareReadersJoinChannelReturnedWithSetupError(t *testing.T) {
	setupErr := errors.New("provider setup failed")
	for _, layer := range streamJoinReaderLayers() {
		t.Run(layer.name, func(t *testing.T) {
			release := make(chan struct{})
			source := &heldJoinReaderModel{
				release:     release,
				cleanupDone: make(chan struct{}),
				cancelCause: make(chan error, 1),
				setupErr:    setupErr,
			}
			type result struct {
				out <-chan MessageChunk
				err error
			}
			returned := make(chan result, 1)
			go func() {
				out, err := layer.wrap(source).Stream(context.Background(), nil)
				returned <- result{out: out, err: err}
			}()
			select {
			case cause := <-source.cancelCause:
				var localAbort *LocalStreamAbortCause
				require.ErrorAs(t, cause, &localAbort)
				require.ErrorIs(t, cause, setupErr)
			case <-time.After(time.Second):
				t.Fatal("reader did not cancel the setup-error source")
			}
			select {
			case got := <-returned:
				t.Fatalf("Stream returned before joining the setup-error channel: %v", got.err)
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			select {
			case <-source.cleanupDone:
			case <-time.After(time.Second):
				t.Fatal("source cleanup did not finish after release")
			}
			select {
			case got := <-returned:
				require.ErrorIs(t, got.err, setupErr)
				require.Nil(t, got.out)
			case <-time.After(time.Second):
				t.Fatal("Stream did not return after joining the setup-error source")
			}
		})
	}
}

func TestMiddlewareReadersAbortAndJoinOnLimitOrTimeout(t *testing.T) {
	layers := []struct {
		name       string
		wrap       func(BaseChatModel) BaseChatModel
		chunks     []MessageChunk
		wantCause  error
		wantReason string
	}{
		{
			name: "timeout",
			wrap: func(next BaseChatModel) BaseChatModel {
				return NewTimeoutModelMiddleware(WithTotalTimeout(20*time.Millisecond), WithStreamChunkTimeout(time.Second)).WrapModel(next)
			},
			chunks:     []MessageChunk{{Role: RoleAssistant, Content: "partial"}},
			wantCause:  context.DeadlineExceeded,
			wantReason: "timeout",
		},
		{
			name:       "loopdetect",
			wrap:       NewLoopDetectionModelMiddleware().WrapModel,
			chunks:     make([]MessageChunk, GuardedStreamMaxChunks+1),
			wantReason: "chunks",
		},
		{
			name:       "overflow",
			wrap:       NewOverflowRecoveryMiddleware().WrapModel,
			chunks:     make([]MessageChunk, GuardedStreamMaxChunks+1),
			wantReason: "chunks",
		},
	}
	for _, layer := range layers {
		t.Run(layer.name, func(t *testing.T) {
			release := make(chan struct{})
			source := &heldJoinReaderModel{
				release:      release,
				cleanupDone:  make(chan struct{}),
				cancelCause:  make(chan error, 1),
				initialChunk: layer.chunks,
			}
			out, err := layer.wrap(source).Stream(context.Background(), nil)
			require.NoError(t, err)
			outputDone := make(chan []MessageChunk, 1)
			go func() {
				var chunks []MessageChunk
				for chunk := range out {
					chunks = append(chunks, chunk)
				}
				outputDone <- chunks
			}()
			select {
			case cause := <-source.cancelCause:
				var localAbort *LocalStreamAbortCause
				require.ErrorAs(t, cause, &localAbort)
				if layer.wantCause != nil {
					require.ErrorIs(t, cause, layer.wantCause)
				}
			case <-time.After(time.Second):
				t.Fatal("local limit did not cancel its source")
			}
			select {
			case chunks := <-outputDone:
				t.Fatalf("wrapper closed before source close: %v", chunks)
			case <-time.After(25 * time.Millisecond):
			}
			close(release)
			select {
			case <-source.cleanupDone:
			case <-time.After(time.Second):
				t.Fatal("source cleanup did not finish after release")
			}
			var collected []MessageChunk
			select {
			case collected = <-outputDone:
			case <-time.After(time.Second):
				t.Fatal("wrapper did not close after source cleanup")
			}
			var terminal []MessageChunk
			for _, chunk := range collected {
				if chunk.Error != nil || chunk.Final {
					terminal = append(terminal, chunk)
				}
			}
			require.Len(t, terminal, 1)
			require.False(t, terminal[0].Final)
			require.ErrorContains(t, terminal[0].Error, layer.wantReason)
		})
	}
}

func TestMiddlewareReadersHoldFinalUntilInputClose(t *testing.T) {
	for _, layer := range streamJoinReaderLayers() {
		t.Run(layer.name, func(t *testing.T) {
			release := make(chan struct{})
			source := &heldUntilReleaseModel{
				initialChunk: []MessageChunk{{Role: RoleAssistant, Content: "final-content", Final: true}},
				release:      release,
				cleanupDone:  make(chan struct{}),
			}
			out, err := layer.wrap(source).Stream(context.Background(), nil)
			require.NoError(t, err)
			pending := requireChannelOpenUntil(t, out, 25*time.Millisecond)
			if layer.name == "timeout" || layer.name == "loopdetect" || layer.name == "overflow" {
				for _, chunk := range pending {
					require.False(t, chunk.Final, "terminal-validating wrappers must hold final until source close")
				}
			}
			close(release)
			select {
			case <-source.cleanupDone:
			case <-time.After(time.Second):
				t.Fatal("source cleanup did not finish after release")
			}
			for chunk := range out {
				pending = append(pending, chunk)
			}
			finals := 0
			var content string
			for _, chunk := range pending {
				if chunk.Final {
					finals++
				}
				content += chunk.Content
			}
			require.Equal(t, 1, finals)
			require.Equal(t, "final-content", content)
		})
	}
}

func TestMiddlewareReadersAbortAndJoinOnDataAfterFinal(t *testing.T) {
	layers := []struct {
		name string
		wrap func(BaseChatModel) BaseChatModel
	}{
		{name: "timeout", wrap: func(next BaseChatModel) BaseChatModel {
			return NewTimeoutModelMiddleware(WithTotalTimeout(5*time.Second), WithStreamChunkTimeout(5*time.Second)).WrapModel(next)
		}},
		{name: "loopdetect", wrap: NewLoopDetectionModelMiddleware().WrapModel},
		{name: "overflow", wrap: NewOverflowRecoveryMiddleware().WrapModel},
	}
	for _, layer := range layers {
		t.Run(layer.name, func(t *testing.T) {
			release := make(chan struct{})
			source := &heldJoinReaderModel{
				release:      release,
				cleanupDone:  make(chan struct{}),
				cancelCause:  make(chan error, 1),
				initialChunk: []MessageChunk{{Role: RoleAssistant, Final: true}, {Role: RoleAssistant, Content: "late"}},
			}
			out, err := layer.wrap(source).Stream(context.Background(), nil)
			require.NoError(t, err)
			cause := <-source.cancelCause
			var localAbort *LocalStreamAbortCause
			require.ErrorAs(t, cause, &localAbort)
			pending := requireChannelOpenUntil(t, out, 25*time.Millisecond)
			close(release)
			select {
			case <-source.cleanupDone:
			case <-time.After(time.Second):
				t.Fatal("source cleanup did not finish after release")
			}
			for chunk := range out {
				pending = append(pending, chunk)
			}
			require.Len(t, pending, 1)
			require.False(t, pending[0].Final)
			require.ErrorContains(t, pending[0].Error, "after its final chunk")
		})
	}
}

func TestMiddlewareReadersRejectClosedSourceWithoutFinal(t *testing.T) {
	layers := []struct {
		name string
		wrap func(BaseChatModel) BaseChatModel
	}{
		{name: "timeout", wrap: func(next BaseChatModel) BaseChatModel {
			return NewTimeoutModelMiddleware(WithTotalTimeout(time.Second), WithStreamChunkTimeout(time.Second)).WrapModel(next)
		}},
		{name: "loopdetect", wrap: NewLoopDetectionModelMiddleware().WrapModel},
		{name: "overflow", wrap: NewOverflowRecoveryMiddleware().WrapModel},
	}
	for _, layer := range layers {
		t.Run(layer.name, func(t *testing.T) {
			model := &joinSetupModel{streamFn: func(context.Context, int32) (<-chan MessageChunk, error) {
				ch := make(chan MessageChunk, 1)
				ch <- MessageChunk{Role: RoleAssistant, Content: "partial"}
				close(ch)
				return ch, nil
			}}
			out, err := layer.wrap(model).Stream(context.Background(), nil)
			require.NoError(t, err)
			var terminal error
			for chunk := range out {
				if chunk.Error != nil {
					terminal = chunk.Error
				}
				require.False(t, chunk.Final)
			}
			require.ErrorContains(t, terminal, "without a final chunk")
		})
	}
}

type heldUntilReleaseModel struct {
	initialChunk []MessageChunk
	release      <-chan struct{}
	cleanupDone  chan struct{}
	contextSeen  chan context.Context
}

func (*heldUntilReleaseModel) Generate(context.Context, []Message, ...Option) (*Message, error) {
	return nil, errors.New("unexpected Generate call")
}

func (m *heldUntilReleaseModel) Stream(ctx context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
	if m.contextSeen != nil {
		m.contextSeen <- ctx
	}
	ch := make(chan MessageChunk, len(m.initialChunk))
	for _, chunk := range m.initialChunk {
		ch <- chunk
	}
	go func() {
		<-m.release
		close(m.cleanupDone)
		close(ch)
	}()
	return ch, nil
}

func requireChannelOpenUntil(t *testing.T, ch <-chan MessageChunk, wait time.Duration) []MessageChunk {
	t.Helper()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	var received []MessageChunk
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				t.Fatal("wrapper output closed before its input source")
			}
			received = append(received, chunk)
		case <-timer.C:
			return received
		}
	}
}
