package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/production"
)

// circuitBreakerModel wraps the complete selected model chain in one breaker
// admission. Streaming tickets settle only after the chain's output closes.
type circuitBreakerModel struct {
	inner   llm.BaseChatModel
	breaker production.CircuitBreaker
}

var _ llm.BaseChatModel = (*circuitBreakerModel)(nil)

func (m *circuitBreakerModel) Generate(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (*llm.Message, error) {
	var once sync.Once
	var called bool
	var innerResult any
	var innerErr error
	invoke := func() (any, error) {
		once.Do(func() {
			called = true
			var msg *llm.Message
			msg, innerErr = m.inner.Generate(ctx, msgs, opts...)
			innerResult = msg
		})
		return innerResult, innerErr
	}

	var result any
	var err error
	if strict, ok := m.breaker.(production.CircuitBreakerWithoutFallback); ok {
		result, err = strict.ExecuteWithoutFallback(ctx, invoke)
	} else {
		result, err = m.breaker.Execute(ctx, invoke)
	}
	if !called && err == nil {
		// Custom breakers may implement an open fallback through Execute. A
		// successful result with no provider invocation remains an explicit
		// refusal at the CLI model boundary.
		err = production.ErrCircuitOpen
	}
	if innerErr != nil {
		return nil, innerErr
	}
	if err != nil {
		if errors.Is(err, production.ErrCircuitOpen) {
			slog.WarnContext(ctx, "circuit_breaker_open_refused", "breaker", m.breaker.Name())
		}
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	msg, ok := result.(*llm.Message)
	if !ok {
		return nil, fmt.Errorf("circuit breaker: unexpected result type %T", result)
	}
	return msg, nil
}

func (m *circuitBreakerModel) Stream(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var ticket production.CircuitBreakerStreamTicket
	if streaming, ok := m.breaker.(production.StreamingCircuitBreaker); ok {
		var err error
		ticket, err = streaming.BeginStream(ctx)
		if err != nil {
			return nil, err
		}
		if ticket == nil {
			return nil, errors.New("circuit breaker: streaming admission returned a nil ticket")
		}
	}

	attemptCtx, cancel := context.WithCancelCause(ctx)
	var source <-chan llm.MessageChunk
	var setupErr error
	if ticket != nil {
		source, setupErr = m.inner.Stream(attemptCtx, msgs, opts...)
	} else {
		source, setupErr = m.streamWithLegacyBreaker(ctx, attemptCtx, msgs, opts...)
	}

	if setupErr != nil {
		outcome := classifySetupOutcome(ctx, setupErr)
		cancelStreamAttempt(ctx, cancel, setupErr, outcome)
		if source != nil {
			for range source {
			}
		}
		cancel(nil)
		if ticket != nil {
			ticket.Complete(outcome)
		}
		return nil, setupErr
	}
	if source == nil {
		outcome := production.StreamFailed
		streamErr := errors.New("circuit breaker: stream returned a nil channel without an error")
		if ctx.Err() != nil {
			outcome = callerCancellationOutcome(ctx)
			streamErr = callerCause(ctx)
		}
		cancelStreamAttempt(ctx, cancel, streamErr, outcome)
		cancel(nil)
		if ticket != nil {
			ticket.Complete(outcome)
		}
		return nil, streamErr
	}
	if ctx.Err() != nil {
		cause := callerCause(ctx)
		cancel(cause)
		for range source {
		}
		cancel(nil)
		if ticket != nil {
			ticket.Complete(callerCancellationOutcome(ctx))
		}
		return nil, cause
	}

	out := make(chan llm.MessageChunk, 64)
	go m.forwardStream(ctx, cancel, source, out, ticket)
	return out, nil
}

// streamWithLegacyBreaker preserves the mandatory interface path for custom
// breakers without streaming tickets. Their accounting ends at setup return;
// the adapter still owns and joins the returned channel.
func (m *circuitBreakerModel) streamWithLegacyBreaker(callerCtx, attemptCtx context.Context, msgs []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	var once sync.Once
	var called bool
	var source <-chan llm.MessageChunk
	var innerResult any
	var innerErr error
	invoke := func() (any, error) {
		once.Do(func() {
			called = true
			source, innerErr = m.inner.Stream(attemptCtx, msgs, opts...)
			if source == nil && innerErr == nil {
				innerErr = errors.New("circuit breaker: stream returned a nil channel without an error")
			}
			if source != nil {
				innerResult = source
			}
		})
		return innerResult, innerErr
	}

	var result any
	var err error
	if strict, ok := m.breaker.(production.CircuitBreakerWithoutFallback); ok {
		result, err = strict.ExecuteWithoutFallback(callerCtx, invoke)
	} else {
		result, err = m.breaker.Execute(callerCtx, invoke)
	}
	if innerErr != nil {
		return source, innerErr
	}
	if called {
		if result == nil && source != nil {
			return source, nil
		}
		if result != nil {
			stream, ok := result.(<-chan llm.MessageChunk)
			if !ok {
				return source, fmt.Errorf("circuit breaker: unexpected stream result type %T", result)
			}
			source = stream
		}
		if err != nil {
			return source, err
		}
		return source, nil
	}
	if err == nil {
		return nil, production.ErrCircuitOpen
	}
	return nil, err
}

func (m *circuitBreakerModel) forwardStream(
	callerCtx context.Context,
	cancel context.CancelCauseFunc,
	source <-chan llm.MessageChunk,
	out chan llm.MessageChunk,
	ticket production.CircuitBreakerStreamTicket,
) {
	outcome := production.StreamFailed
	var terminalErr error
	var final *llm.MessageChunk
	var sourceClosed bool

	callerCanceled := func() bool { return callerCtx.Err() != nil }
	callerCancellation := func() {
		cause := callerCause(callerCtx)
		outcome = callerCancellationOutcome(callerCtx)
		terminalErr = fmt.Errorf("circuit breaker: stream canceled: %w", cause)
		cancel(cause)
		for range source {
		}
		sourceClosed = true
	}
	localFailure := func(err error) {
		if err == nil {
			err = errors.New("circuit breaker: invalid stream terminal")
		}
		outcome = production.StreamFailed
		terminalErr = err
		cancel(llm.NewLocalStreamAbortCause(err))
		for range source {
		}
		sourceClosed = true
	}

	for !sourceClosed {
		if callerCanceled() {
			callerCancellation()
			break
		}
		select {
		case <-callerCtx.Done():
			callerCancellation()
			sourceClosed = true
		case chunk, ok := <-source:
			// If both were ready, cancellation wins when it is observable at
			// this read boundary. A failure observed first is retained while
			// draining even if caller cancellation arrives later.
			if callerCanceled() {
				callerCancellation()
				sourceClosed = true
				break
			}
			if !ok {
				sourceClosed = true
				if final == nil {
					localFailure(errors.New("circuit breaker: stream ended without a final chunk"))
				} else {
					outcome = production.StreamSucceeded
				}
				break
			}
			if chunk.Error != nil {
				localFailure(chunk.Error)
				break
			}
			if final != nil {
				localFailure(errors.New("circuit breaker: stream emitted data after its final chunk"))
				break
			}
			if chunk.Final {
				copy := chunk
				final = &copy
				continue
			}
			select {
			case out <- chunk:
			case <-callerCtx.Done():
				callerCancellation()
				sourceClosed = true
			}
		}
	}

	if outcome == production.StreamSucceeded {
		if callerCanceled() {
			callerCancellation()
		} else {
			select {
			case out <- *final:
			case <-callerCtx.Done():
				callerCancellation()
			}
		}
	}
	if terminalErr != nil {
		emitCircuitStreamError(out, terminalErr)
	}
	cancel(nil)
	// The source has joined. Settle before exposing output closure so an outer
	// OutputGuard cannot evaluate post-close policy before provider accounting.
	if ticket != nil {
		ticket.Complete(outcome)
	}
	close(out)
}

func classifySetupOutcome(ctx context.Context, err error) production.StreamOutcome {
	var callerAbort *llm.LocalStreamAbortCause
	if ctx.Err() != nil && errors.As(context.Cause(ctx), &callerAbort) {
		return production.StreamFailed
	}
	var localAbort *llm.LocalStreamAbortCause
	if errors.As(err, &localAbort) {
		return production.StreamFailed
	}
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return production.StreamCanceledNeutral
	}
	return production.StreamFailed
}

func cancelStreamAttempt(ctx context.Context, cancel context.CancelCauseFunc, err error, outcome production.StreamOutcome) {
	if outcome == production.StreamCanceledNeutral {
		cancel(callerCause(ctx))
		return
	}
	cancel(llm.NewLocalStreamAbortCause(err))
}

func callerCause(ctx context.Context) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return context.Canceled
}

func callerCancellationOutcome(ctx context.Context) production.StreamOutcome {
	var localAbort *llm.LocalStreamAbortCause
	if errors.As(context.Cause(ctx), &localAbort) {
		// In-chain local failures use the caller context as their abort signal;
		// only an ordinary caller-first cancellation is neutral.
		return production.StreamFailed
	}
	return production.StreamCanceledNeutral
}

func emitCircuitStreamError(out chan llm.MessageChunk, err error) {
	if len(out) == cap(out) {
		select {
		case <-out:
		default:
		}
	}
	select {
	case out <- llm.MessageChunk{Error: err}:
	default:
	}
}
