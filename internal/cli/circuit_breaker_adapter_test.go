package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/production"
)

type breakerTestModel struct {
	generateCalls atomic.Int32
	streamCalls   atomic.Int32
	generateFn    func(context.Context, []llm.Message, ...llm.Option) (*llm.Message, error)
	streamFn      func(context.Context, []llm.Message, ...llm.Option) (<-chan llm.MessageChunk, error)
}

func (m *breakerTestModel) Generate(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (*llm.Message, error) {
	m.generateCalls.Add(1)
	if m.generateFn != nil {
		return m.generateFn(ctx, msgs, opts...)
	}
	return &llm.Message{Role: llm.RoleAssistant, Content: "generated"}, nil
}

func (m *breakerTestModel) Stream(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	m.streamCalls.Add(1)
	if m.streamFn != nil {
		return m.streamFn(ctx, msgs, opts...)
	}
	out := make(chan llm.MessageChunk, 1)
	out <- llm.MessageChunk{Role: llm.RoleAssistant, Final: true}
	close(out)
	return out, nil
}

func wrapBreakerTestModel(model llm.BaseChatModel, breaker production.CircuitBreaker, guard production.OutputGuard) llm.BaseChatModel {
	wrapped := newModelWrapperWithChain(production.NewProductionModelWrapper(), llm.NewStandardMiddlewareChain(), breaker, guard)(model)
	return wrapped.(llm.BaseChatModel)
}

func openBreakerForAdapterTest(t *testing.T, breaker production.CircuitBreaker) {
	t.Helper()
	_, err := breaker.Execute(context.Background(), func() (any, error) {
		return nil, errors.New("provider failed")
	})
	require.Error(t, err)
	require.Equal(t, production.CircuitOpen, breaker.State())
}

func TestCircuitBreakerModelOpenGenerateReturnsTypedRefusal(t *testing.T) {
	fallbackCalls := atomic.Int32{}
	breaker := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{FailureThreshold: 1}, production.WithFallback(func() (any, error) {
		fallbackCalls.Add(1)
		return &llm.Message{Role: llm.RoleAssistant, Content: "fallback"}, nil
	}))
	openBreakerForAdapterTest(t, breaker)
	model := &breakerTestModel{}
	wrapped := wrapBreakerTestModel(model, breaker, nil)

	response, err := wrapped.Generate(context.Background(), nil)

	require.ErrorIs(t, err, production.ErrCircuitOpen)
	require.Nil(t, response)
	require.Zero(t, model.generateCalls.Load())
	require.Zero(t, fallbackCalls.Load(), "CLI model refusals must not become configured synthetic fallback responses")
}

func TestCircuitBreakerModelOpenStreamReturnsTypedRefusal(t *testing.T) {
	fallbackCalls := atomic.Int32{}
	breaker := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{FailureThreshold: 1}, production.WithFallback(func() (any, error) {
		fallbackCalls.Add(1)
		ch := make(chan llm.MessageChunk, 1)
		ch <- llm.MessageChunk{Role: llm.RoleAssistant, Content: "fallback", Final: true}
		close(ch)
		return (<-chan llm.MessageChunk)(ch), nil
	}))
	openBreakerForAdapterTest(t, breaker)
	model := &breakerTestModel{}
	wrapped := wrapBreakerTestModel(model, breaker, nil)

	stream, err := wrapped.Stream(context.Background(), nil)

	require.ErrorIs(t, err, production.ErrCircuitOpen)
	require.Nil(t, stream)
	require.Zero(t, model.streamCalls.Load())
	require.Zero(t, fallbackCalls.Load(), "stream admission must not invoke a generic synthetic fallback")
}

func TestCircuitBreakerModelRecordsMidstreamFailure(t *testing.T) {
	providerErr := errors.New("provider stream failed")
	breaker := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{FailureThreshold: 1})
	model := &breakerTestModel{streamFn: func(context.Context, []llm.Message, ...llm.Option) (<-chan llm.MessageChunk, error) {
		ch := make(chan llm.MessageChunk, 1)
		ch <- llm.MessageChunk{Error: providerErr}
		close(ch)
		return ch, nil
	}}
	wrapped := wrapBreakerTestModel(model, breaker, nil)

	stream, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)
	var terminal error
	for chunk := range stream {
		if chunk.Error != nil {
			terminal = chunk.Error
		}
	}

	require.ErrorIs(t, terminal, providerErr)
	require.Equal(t, production.CircuitOpen, breaker.State())
	require.EqualValues(t, 1, model.streamCalls.Load())
}

func TestCircuitBreakerModelHoldsHalfOpenProbeUntilSourceCloses(t *testing.T) {
	clockNanos := atomic.Int64{}
	clock := func() time.Time { return time.Unix(0, clockNanos.Load()) }
	breaker := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{
		FailureThreshold: 1,
		RecoveryTimeout:  time.Second,
		HalfOpenMaxCalls: 1,
	}, production.WithClock(clock))
	openBreakerForAdapterTest(t, breaker)
	clockNanos.Add(int64(time.Second))
	release := make(chan struct{})
	model := &breakerTestModel{streamFn: func(context.Context, []llm.Message, ...llm.Option) (<-chan llm.MessageChunk, error) {
		ch := make(chan llm.MessageChunk, 1)
		ch <- llm.MessageChunk{Role: llm.RoleAssistant, Final: true}
		go func() {
			<-release
			close(ch)
		}()
		return ch, nil
	}}
	wrapped := wrapBreakerTestModel(model, breaker, nil)

	first, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)
	select {
	case chunk := <-first:
		t.Fatalf("final chunk escaped before source close: %+v", chunk)
	case <-time.After(25 * time.Millisecond):
	}
	require.Equal(t, production.CircuitHalfOpen, breaker.State())

	second, err := wrapped.Stream(context.Background(), nil)
	require.Nil(t, second)
	require.ErrorIs(t, err, production.ErrCircuitOpen)
	require.EqualValues(t, 1, model.streamCalls.Load())

	close(release)
	var finals int
	for chunk := range first {
		if chunk.Final {
			finals++
		}
		if chunk.Error != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Error)
		}
	}
	require.Equal(t, 1, finals)
	require.Equal(t, production.CircuitClosed, breaker.State())
}

func TestCircuitBreakerModelRequiresOneFinalAndNoPostFinalData(t *testing.T) {
	tests := []struct {
		name   string
		chunks []llm.MessageChunk
	}{
		{name: "missing final", chunks: []llm.MessageChunk{{Role: llm.RoleAssistant, Content: "partial"}}},
		{name: "data after final", chunks: []llm.MessageChunk{{Role: llm.RoleAssistant, Final: true}, {Role: llm.RoleAssistant, Content: "late"}}},
		{name: "error after final", chunks: []llm.MessageChunk{{Role: llm.RoleAssistant, Final: true}, {Error: errors.New("late provider error")}}},
		{name: "second final", chunks: []llm.MessageChunk{{Role: llm.RoleAssistant, Final: true}, {Role: llm.RoleAssistant, Final: true}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			breaker := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{FailureThreshold: 1})
			model := &breakerTestModel{streamFn: func(context.Context, []llm.Message, ...llm.Option) (<-chan llm.MessageChunk, error) {
				ch := make(chan llm.MessageChunk, len(tt.chunks))
				for _, chunk := range tt.chunks {
					ch <- chunk
				}
				close(ch)
				return ch, nil
			}}
			wrapped := wrapBreakerTestModel(model, breaker, nil)

			stream, err := wrapped.Stream(context.Background(), nil)
			require.NoError(t, err)
			var terminal error
			for chunk := range stream {
				if chunk.Error != nil {
					terminal = chunk.Error
				}
			}
			require.Error(t, terminal)
			require.Equal(t, production.CircuitOpen, breaker.State())
		})
	}
}

type denyBreakerGuard struct{}

func (denyBreakerGuard) Name() string { return "deny" }

func (denyBreakerGuard) Check(context.Context, string) (*production.GuardResult, error) {
	return &production.GuardResult{Allowed: false, Reason: "policy denied"}, nil
}

func TestOutputGuardDenialAfterClosedProviderStreamDoesNotFailBreaker(t *testing.T) {
	breaker := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{FailureThreshold: 1})
	model := &breakerTestModel{streamFn: func(context.Context, []llm.Message, ...llm.Option) (<-chan llm.MessageChunk, error) {
		ch := make(chan llm.MessageChunk, 2)
		ch <- llm.MessageChunk{Role: llm.RoleAssistant, Content: "candidate"}
		ch <- llm.MessageChunk{Role: llm.RoleAssistant, Final: true}
		close(ch)
		return ch, nil
	}}
	wrapped := wrapBreakerTestModel(model, breaker, denyBreakerGuard{})

	stream, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)
	var terminal error
	for chunk := range stream {
		if chunk.Error != nil {
			terminal = chunk.Error
		}
	}
	require.ErrorContains(t, terminal, "candidate denied")
	require.Equal(t, production.CircuitClosed, breaker.State())
}

func TestCircuitBreakerModelRejectsNilSuccessSetup(t *testing.T) {
	breaker := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{FailureThreshold: 1})
	model := &breakerTestModel{streamFn: func(context.Context, []llm.Message, ...llm.Option) (<-chan llm.MessageChunk, error) {
		return nil, nil
	}}
	wrapped := wrapBreakerTestModel(model, breaker, nil)

	stream, err := wrapped.Stream(context.Background(), nil)
	require.Nil(t, stream)
	require.ErrorContains(t, err, "nil channel")
	require.Equal(t, production.CircuitOpen, breaker.State())
}

func TestCircuitBreakerModelCallerCancellationIsNeutralAfterSourceJoins(t *testing.T) {
	breaker := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{FailureThreshold: 1})
	callerCanceled := make(chan struct{})
	release := make(chan struct{})
	model := &breakerTestModel{streamFn: func(ctx context.Context, _ []llm.Message, _ ...llm.Option) (<-chan llm.MessageChunk, error) {
		ch := make(chan llm.MessageChunk)
		go func() {
			<-ctx.Done()
			close(callerCanceled)
			<-release
			close(ch)
		}()
		return ch, nil
	}}
	wrapped := wrapBreakerTestModel(model, breaker, nil)
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := wrapped.Stream(ctx, nil)
	require.NoError(t, err)
	cancel()
	select {
	case <-callerCanceled:
	case <-time.After(time.Second):
		t.Fatal("source did not observe caller cancellation")
	}
	require.Equal(t, production.CircuitClosed, breaker.State(), "ticket must remain pending until the source joins")
	select {
	case chunk, ok := <-stream:
		t.Fatalf("adapter output completed before source cleanup: ok=%v chunk=%+v", ok, chunk)
	case <-time.After(25 * time.Millisecond):
	}

	close(release)
	var streamErr error
	for chunk := range stream {
		if chunk.Error != nil {
			streamErr = chunk.Error
		}
	}
	require.ErrorIs(t, streamErr, context.Canceled)
	require.Equal(t, production.CircuitClosed, breaker.State(), "caller-first cancellation is neutral")
}

func TestCircuitBreakerModelLocalTypedAbortFailureSurvivesLateCallerCancel(t *testing.T) {
	breaker := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{FailureThreshold: 1})
	localCauseObserved := make(chan error, 1)
	release := make(chan struct{})
	model := &breakerTestModel{streamFn: func(ctx context.Context, _ []llm.Message, _ ...llm.Option) (<-chan llm.MessageChunk, error) {
		ch := make(chan llm.MessageChunk, 1)
		ch <- llm.MessageChunk{Error: llm.NewLocalStreamAbortCause(context.DeadlineExceeded)}
		go func() {
			<-ctx.Done()
			localCauseObserved <- context.Cause(ctx)
			<-release
			close(ch)
		}()
		return ch, nil
	}}
	wrapped := wrapBreakerTestModel(model, breaker, nil)
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := wrapped.Stream(ctx, nil)
	require.NoError(t, err)
	select {
	case cause := <-localCauseObserved:
		var localAbort *llm.LocalStreamAbortCause
		require.ErrorAs(t, cause, &localAbort)
		require.ErrorIs(t, cause, context.DeadlineExceeded)
	case <-time.After(time.Second):
		t.Fatal("adapter did not preserve its local abort cause")
	}
	require.Equal(t, production.CircuitClosed, breaker.State(), "a failure cannot settle before source cleanup")
	cancel()
	select {
	case chunk, ok := <-stream:
		t.Fatalf("adapter output completed before the held source: ok=%v chunk=%+v", ok, chunk)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	var streamErr error
	for chunk := range stream {
		if chunk.Error != nil {
			streamErr = chunk.Error
		}
	}
	var localAbort *llm.LocalStreamAbortCause
	require.ErrorAs(t, streamErr, &localAbort)
	require.ErrorIs(t, streamErr, context.DeadlineExceeded)
	require.Equal(t, production.CircuitOpen, breaker.State(), "late parent cancellation cannot erase an earlier local failure")
}

func TestCircuitBreakerModelCallerContextLocalAbortIsFailure(t *testing.T) {
	breaker := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{FailureThreshold: 1})
	release := make(chan struct{})
	model := &breakerTestModel{streamFn: func(ctx context.Context, _ []llm.Message, _ ...llm.Option) (<-chan llm.MessageChunk, error) {
		ch := make(chan llm.MessageChunk)
		go func() {
			<-ctx.Done()
			<-release
			close(ch)
		}()
		return ch, nil
	}}
	wrapped := wrapBreakerTestModel(model, breaker, nil)
	ctx, cancel := context.WithCancelCause(context.Background())
	stream, err := wrapped.Stream(ctx, nil)
	require.NoError(t, err)
	cancel(llm.NewLocalStreamAbortCause(errors.New("stream rejected by local budget")))

	select {
	case chunk, ok := <-stream:
		t.Fatalf("adapter output completed before the source joined: ok=%v chunk=%+v", ok, chunk)
	case <-time.After(25 * time.Millisecond):
	}
	require.Equal(t, production.CircuitClosed, breaker.State(), "ticket remains pending while source cleanup is held")
	close(release)
	for range stream {
	}
	require.Equal(t, production.CircuitOpen, breaker.State(), "a typed in-chain local abort is a failure, not caller cancellation")
}

type blockingStreamTicket struct {
	entered chan struct{}
	release chan struct{}
}

func (t *blockingStreamTicket) Complete(production.StreamOutcome) bool {
	close(t.entered)
	<-t.release
	return true
}

type blockingTicketBreaker struct {
	ticket *blockingStreamTicket
}

func (b *blockingTicketBreaker) Execute(ctx context.Context, fn func() (any, error)) (any, error) {
	return fn()
}
func (*blockingTicketBreaker) State() production.CircuitState { return production.CircuitClosed }
func (*blockingTicketBreaker) Reset(context.Context) error    { return nil }
func (*blockingTicketBreaker) Name() string                   { return "blocking-ticket-test-breaker" }
func (b *blockingTicketBreaker) BeginStream(context.Context) (production.CircuitBreakerStreamTicket, error) {
	return b.ticket, nil
}

func TestCircuitBreakerModelSettlesTicketBeforeClosingOutput(t *testing.T) {
	ticket := &blockingStreamTicket{entered: make(chan struct{}), release: make(chan struct{})}
	breaker := &blockingTicketBreaker{ticket: ticket}
	model := &breakerTestModel{}
	wrapped := &circuitBreakerModel{inner: model, breaker: breaker}

	stream, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)
	chunk, ok := <-stream
	require.True(t, ok)
	require.True(t, chunk.Final)
	select {
	case <-ticket.entered:
	case <-time.After(time.Second):
		t.Fatal("stream ticket was not settled after the source joined")
	}
	select {
	case chunk, ok := <-stream:
		if !ok {
			t.Fatal("adapter output closed before ticket settlement returned")
		}
		t.Fatalf("unexpected chunk while ticket settlement is pending: %+v", chunk)
	case <-time.After(25 * time.Millisecond):
	}
	close(ticket.release)
	for range stream {
	}
}

func TestCircuitBreakerModelSetupFailureWaitsForSourceJoin(t *testing.T) {
	setupErr := errors.New("provider setup failed")
	breaker := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{FailureThreshold: 1})
	localCauseObserved := make(chan error, 1)
	release := make(chan struct{})
	model := &breakerTestModel{streamFn: func(ctx context.Context, _ []llm.Message, _ ...llm.Option) (<-chan llm.MessageChunk, error) {
		ch := make(chan llm.MessageChunk)
		go func() {
			<-ctx.Done()
			localCauseObserved <- context.Cause(ctx)
			<-release
			close(ch)
		}()
		return ch, setupErr
	}}
	wrapped := wrapBreakerTestModel(model, breaker, nil)
	returned := make(chan error, 1)
	go func() {
		stream, err := wrapped.Stream(context.Background(), nil)
		if stream != nil {
			for range stream {
			}
		}
		returned <- err
	}()
	select {
	case cause := <-localCauseObserved:
		var localAbort *llm.LocalStreamAbortCause
		require.ErrorAs(t, cause, &localAbort)
		require.ErrorIs(t, cause, setupErr)
	case <-time.After(time.Second):
		t.Fatal("setup failure did not abort the held source")
	}
	require.Equal(t, production.CircuitClosed, breaker.State(), "setup failure remains pending until the source joins")
	select {
	case err := <-returned:
		t.Fatalf("Stream returned before source close: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-returned:
		require.ErrorIs(t, err, setupErr)
	case <-time.After(time.Second):
		t.Fatal("Stream did not return after source close")
	}
	require.Equal(t, production.CircuitOpen, breaker.State())
}

type singleRetryPolicy struct{}

func (singleRetryPolicy) ShouldRetry(_ context.Context, _ error, attempt int) bool {
	return attempt == 0
}
func (singleRetryPolicy) NextBackoff(context.Context, int) time.Duration { return 0 }
func (singleRetryPolicy) Name() string                                   { return "single-retry" }

func TestCircuitBreakerModelKeepsRetrySetupInsideOneLogicalTicket(t *testing.T) {
	setupErr := errors.New("first setup failed")
	var calls atomic.Int32
	model := &breakerTestModel{streamFn: func(context.Context, []llm.Message, ...llm.Option) (<-chan llm.MessageChunk, error) {
		if calls.Add(1) == 1 {
			return nil, setupErr
		}
		ch := make(chan llm.MessageChunk, 1)
		ch <- llm.MessageChunk{Role: llm.RoleAssistant, Content: "retried", Final: true}
		close(ch)
		return ch, nil
	}}
	chain := llm.NewStandardMiddlewareChain(llm.NewRetryModelMiddleware(llm.WithRetryPolicy(singleRetryPolicy{})))
	breaker := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{FailureThreshold: 1})
	wrapped := newModelWrapperWithChain(production.NewProductionModelWrapper(), chain, breaker, nil)(model).(llm.BaseChatModel)

	stream, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)
	var content string
	for chunk := range stream {
		if chunk.Error != nil {
			t.Fatalf("retry should recover the stream setup: %v", chunk.Error)
		}
		content += chunk.Content
	}
	require.Equal(t, "retried", content)
	require.EqualValues(t, 2, calls.Load(), "both setup dispatches belong to one breaker admission")
	require.Equal(t, production.CircuitClosed, breaker.State(), "the one logical call succeeded")
}

func TestCircuitBreakerModelDoesNotRetryPostHandoffFailure(t *testing.T) {
	providerErr := errors.New("stream failed after handoff")
	var calls atomic.Int32
	model := &breakerTestModel{streamFn: func(context.Context, []llm.Message, ...llm.Option) (<-chan llm.MessageChunk, error) {
		calls.Add(1)
		ch := make(chan llm.MessageChunk, 1)
		ch <- llm.MessageChunk{Error: providerErr}
		close(ch)
		return ch, nil
	}}
	chain := llm.NewStandardMiddlewareChain(llm.NewRetryModelMiddleware(llm.WithRetryPolicy(singleRetryPolicy{})))
	breaker := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{FailureThreshold: 1})
	wrapped := newModelWrapperWithChain(production.NewProductionModelWrapper(), chain, breaker, nil)(model).(llm.BaseChatModel)

	stream, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)
	var terminal error
	for chunk := range stream {
		if chunk.Error != nil {
			terminal = chunk.Error
		}
	}
	require.ErrorIs(t, terminal, providerErr)
	require.EqualValues(t, 1, calls.Load(), "once the retry layer hands out a channel, a later error is not replayed")
	require.Equal(t, production.CircuitOpen, breaker.State())
}

func TestCircuitBreakerModelCoversModelCyclerSetupFallback(t *testing.T) {
	var selectedCalls atomic.Int32
	selectedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		selectedCalls.Add(1)
		http.Error(w, "selected setup failed", http.StatusServiceUnavailable)
	}))
	defer selectedServer.Close()
	var primaryCalls atomic.Int32
	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"primary recovered\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer primaryServer.Close()
	registry := llm.NewProviderRegistryWithDefaultProviderOptions(
		llm.WithBaseURL(selectedServer.URL),
		llm.WithAPIKey("test-key"),
		llm.WithHTTPClient(selectedServer.Client()),
	)
	require.NoError(t, registry.Register(llm.NewEinoProvider(
		llm.WithProviderName("eino-primary"),
		llm.WithBaseURL(primaryServer.URL),
		llm.WithAPIKey("test-key"),
		llm.WithHTTPClient(primaryServer.Client()),
	)))
	resolver := registry.SnapshotResolver()
	selected, err := resolver.ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID: "eino", ModelID: "test-model",
	})
	require.NoError(t, err)
	primary, err := resolver.ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID: "eino-primary", ModelID: "primary-model",
	})
	require.NoError(t, err)
	primaryModel, cleanup, err := primary.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()
	cycler := llm.NewModelCycler(llm.ModelCyclerConfig{
		Models:   []llm.ModelEntry{{Provider: "eino", Model: "test-model"}},
		Strategy: llm.StrategyRoundRobin,
	}).WithResolvedModels([]llm.ResolvedModel{selected})
	chain := llm.NewStandardMiddlewareChain(cycler)
	breaker := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{FailureThreshold: 1})
	wrapped := newModelWrapperWithChain(production.NewProductionModelWrapper(), chain, breaker, nil)(primaryModel).(llm.BaseChatModel)

	loop := core.NewLoopAgent(core.WithLLM(wrapped), core.WithMaxIterations(1))
	result, err := loop.RunCanonical(context.Background(), core.Submission{Content: "recover from selected setup failure"})
	require.NoError(t, err)
	require.EqualValues(t, 1, selectedCalls.Load())
	require.EqualValues(t, 1, primaryCalls.Load())
	require.NotEmpty(t, result.Transcript)
	require.Equal(t, "primary recovered", result.Transcript[len(result.Transcript)-1].Message.Content)
	require.Equal(t, production.CircuitClosed, breaker.State(), "selected setup failure and primary recovery are one successful admission")
}

type legacyCircuitBreaker struct {
	state   production.CircuitState
	execute func(context.Context, func() (any, error)) (any, error)
}

func (b *legacyCircuitBreaker) Execute(ctx context.Context, fn func() (any, error)) (any, error) {
	return b.execute(ctx, fn)
}
func (b *legacyCircuitBreaker) State() production.CircuitState { return b.state }
func (b *legacyCircuitBreaker) Reset(context.Context) error {
	b.state = production.CircuitClosed
	return nil
}
func (b *legacyCircuitBreaker) Name() string { return "legacy-test-breaker" }

func TestCircuitBreakerModelLegacyFallbackCannotMasqueradeAsProviderCall(t *testing.T) {
	fallbackCalls := atomic.Int32{}
	breaker := &legacyCircuitBreaker{state: production.CircuitClosed, execute: func(context.Context, func() (any, error)) (any, error) {
		fallbackCalls.Add(1)
		return &llm.Message{Role: llm.RoleAssistant, Content: "synthetic"}, nil
	}}
	model := &breakerTestModel{}
	wrapped := &circuitBreakerModel{inner: model, breaker: breaker}

	response, err := wrapped.Generate(context.Background(), nil)

	require.ErrorIs(t, err, production.ErrCircuitOpen)
	require.Nil(t, response)
	require.Zero(t, model.generateCalls.Load())
	require.EqualValues(t, 1, fallbackCalls.Load())
}

func TestCircuitBreakerModelLegacyFallbackCannotMaskProviderError(t *testing.T) {
	providerErr := errors.New("actual provider failure")
	breaker := &legacyCircuitBreaker{state: production.CircuitOpen, execute: func(ctx context.Context, fn func() (any, error)) (any, error) {
		_, err := fn()
		if err != nil {
			return &llm.Message{Role: llm.RoleAssistant, Content: "synthetic"}, nil
		}
		return nil, nil
	}}
	model := &breakerTestModel{generateFn: func(context.Context, []llm.Message, ...llm.Option) (*llm.Message, error) {
		return nil, providerErr
	}}
	wrapped := &circuitBreakerModel{inner: model, breaker: breaker}

	response, err := wrapped.Generate(context.Background(), nil)

	require.ErrorIs(t, err, providerErr)
	require.Nil(t, response)
	require.EqualValues(t, 1, model.generateCalls.Load())
}

func TestCircuitBreakerModelLegacyStreamFallbackReturnsOpenRefusal(t *testing.T) {
	breaker := &legacyCircuitBreaker{state: production.CircuitClosed, execute: func(context.Context, func() (any, error)) (any, error) {
		ch := make(chan llm.MessageChunk, 1)
		ch <- llm.MessageChunk{Role: llm.RoleAssistant, Content: "synthetic", Final: true}
		close(ch)
		return (<-chan llm.MessageChunk)(ch), nil
	}}
	model := &breakerTestModel{}
	wrapped := &circuitBreakerModel{inner: model, breaker: breaker}

	stream, err := wrapped.Stream(context.Background(), nil)

	require.ErrorIs(t, err, production.ErrCircuitOpen)
	require.Nil(t, stream)
	require.Zero(t, model.streamCalls.Load())
}
