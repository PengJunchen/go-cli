// Package llm middleware_timeout.go - TimeoutModelMiddleware enforces total and
// per-chunk timeouts on a BaseChatModel.
package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// TimeoutModelMiddleware enforces a total deadline on Generate/Stream and a
// per-chunk idle timeout on Stream. When either timeout fires the context is
// canceled so the underlying model can clean up promptly.
type TimeoutModelMiddleware struct {
	totalTimeout       time.Duration
	streamChunkTimeout time.Duration
	logger             *slog.Logger
}

// Compile-time assertion that TimeoutModelMiddleware satisfies ModelMiddleware.
var _ ModelMiddleware = (*TimeoutModelMiddleware)(nil)

// TimeoutOption configures TimeoutModelMiddleware at construction time.
type TimeoutOption func(*TimeoutModelMiddleware)

// WithTotalTimeout sets the overall deadline for a single Generate or Stream
// call.
func WithTotalTimeout(d time.Duration) TimeoutOption {
	return func(m *TimeoutModelMiddleware) { m.totalTimeout = d }
}

// WithStreamChunkTimeout sets the maximum idle period between consecutive
// stream chunks.
func WithStreamChunkTimeout(d time.Duration) TimeoutOption {
	return func(m *TimeoutModelMiddleware) { m.streamChunkTimeout = d }
}

// WithTimeoutLogger overrides the default logger.
func WithTimeoutLogger(logger *slog.Logger) TimeoutOption {
	return func(m *TimeoutModelMiddleware) { m.logger = logger }
}

// NewTimeoutModelMiddleware creates a TimeoutModelMiddleware with sensible
// defaults: 30s total timeout, 10s chunk timeout.
func NewTimeoutModelMiddleware(opts ...TimeoutOption) *TimeoutModelMiddleware {
	m := &TimeoutModelMiddleware{
		totalTimeout:       30 * time.Second,
		streamChunkTimeout: 10 * time.Second,
		logger:             slog.Default(),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Name returns "timeout".
func (m *TimeoutModelMiddleware) Name() string { return "timeout" }

// WrapModel returns a BaseChatModel that enforces timeouts.
func (m *TimeoutModelMiddleware) WrapModel(next BaseChatModel) BaseChatModel {
	return &timeoutModel{
		next:               next,
		totalTimeout:       m.totalTimeout,
		streamChunkTimeout: m.streamChunkTimeout,
		logger:             m.logger,
	}
}

// timeoutModel is the wrapped BaseChatModel produced by TimeoutModelMiddleware.
type timeoutModel struct {
	next               BaseChatModel
	totalTimeout       time.Duration
	streamChunkTimeout time.Duration
	logger             *slog.Logger
}

// Compile-time assertion that timeoutModel satisfies BaseChatModel.
var _ BaseChatModel = (*timeoutModel)(nil)

// Generate wraps the context with totalTimeout before calling next. If the
// context deadline is exceeded, a wrapped timeout error is returned.
func (t *timeoutModel) Generate(ctx context.Context, msgs []Message, opts ...Option) (*Message, error) {
	ctx, cancel := context.WithTimeout(ctx, t.totalTimeout)
	defer cancel()

	msg, err := t.next.Generate(ctx, msgs, opts...)
	if ctx.Err() != nil {
		t.logger.Debug("middleware.timeout",
			"op", "generate",
			"status", "timeout",
			"total", t.totalTimeout.String(),
		)
		return nil, fmt.Errorf("timeout: generate exceeded %s: %w", t.totalTimeout, ctx.Err())
	}
	return msg, err
}

// Stream starts the totalTimeout budget before calling the wrapped model and
// monitors chunk arrival times. If no chunk arrives within streamChunkTimeout
// the context is canceled and the output channel is closed. Initial setup errors
// are returned directly; mid-stream timeouts manifest as a closed output channel.
func (t *timeoutModel) Stream(ctx context.Context, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	parentCtx := ctx
	timeoutError := func(kind string) error {
		if kind == "total" {
			return fmt.Errorf("timeout: stream exceeded %s: %w", t.totalTimeout, context.DeadlineExceeded)
		}
		return fmt.Errorf("timeout: stream idle exceeded %s: %w", t.streamChunkTimeout, context.DeadlineExceeded)
	}
	budgetCtx, stopBudget := context.WithTimeoutCause(
		parentCtx,
		t.totalTimeout,
		NewLocalStreamAbortCause(timeoutError("total")),
	)
	streamCtx, cancel := context.WithCancelCause(budgetCtx)
	abortSetup := func(source <-chan MessageChunk, failure error) error {
		if parentCtx.Err() != nil {
			cause := context.Cause(parentCtx)
			if cause == nil {
				cause = parentCtx.Err()
			}
			cancelStreamAttempt(parentCtx, cancel, cause)
			drainMessageChunks(source)
			cancel(nil)
			stopBudget()
			return cause
		}
		if budgetCtx.Err() != nil {
			cause := context.Cause(budgetCtx)
			if cause == nil {
				cause = budgetCtx.Err()
			}
			cancelStreamAttempt(parentCtx, cancel, cause)
			drainMessageChunks(source)
			cancel(nil)
			stopBudget()
			return cause
		}
		cancelStreamAttempt(parentCtx, cancel, failure)
		drainMessageChunks(source)
		cancel(nil)
		stopBudget()
		return failure
	}

	ch, err := t.next.Stream(streamCtx, msgs, opts...)
	if err != nil {
		return nil, abortSetup(ch, err)
	}
	if ch == nil {
		err = fmt.Errorf("timeout: model returned a nil stream")
		return nil, abortSetup(nil, err)
	}
	if parentCtx.Err() != nil || budgetCtx.Err() != nil {
		return nil, abortSetup(ch, timeoutError("total"))
	}

	out := make(chan MessageChunk, 64)
	go func() {
		defer func() {
			cancel(nil)
			stopBudget()
			close(out)
		}()
		var finalChunk *MessageChunk
		terminalErrorSent := false
		emitError := func(err error) {
			if err == nil || terminalErrorSent {
				return
			}
			terminalErrorSent = true
			// On cancellation, retain the failure marker even when a slow
			// consumer left the bounded output queue full. Buffered partial
			// chunks may be dropped, but a timeout can never look successful.
			if len(out) == cap(out) {
				select {
				case <-out:
				default:
				}
			}
			select {
			case out <- MessageChunk{Error: err}:
			default:
			}
		}

		chunkTimer := time.NewTimer(t.streamChunkTimeout)
		defer chunkTimer.Stop()
		fail := func(failure error) {
			emitError(failure)
			cancelStreamAttempt(parentCtx, cancel, failure)
			drainMessageChunks(ch)
		}

		for {
			select {
			case <-parentCtx.Done():
				t.logger.Debug("middleware.timeout",
					"op", "stream",
					"status", "canceled",
				)
				cause := context.Cause(parentCtx)
				if cause == nil {
					cause = parentCtx.Err()
				}
				fail(fmt.Errorf("timeout: stream canceled: %w", cause))
				return
			case <-budgetCtx.Done():
				if parentCtx.Err() != nil {
					cause := context.Cause(parentCtx)
					if cause == nil {
						cause = parentCtx.Err()
					}
					fail(fmt.Errorf("timeout: stream canceled: %w", cause))
					return
				}
				t.logger.Debug("middleware.timeout",
					"op", "stream",
					"status", "timeout",
					"reason", "total",
				)
				fail(timeoutError("total"))
				return
			case <-chunkTimer.C:
				t.logger.Debug("middleware.timeout",
					"op", "stream",
					"status", "timeout",
					"reason", "chunk_idle",
				)
				fail(timeoutError("idle"))
				return
			case chunk, ok := <-ch:
				if !ok {
					if finalChunk == nil {
						emitError(fmt.Errorf("timeout: stream closed without a final chunk"))
						cancelStreamAttempt(parentCtx, cancel, errors.New("stream closed without a final chunk"))
						return
					}
					cancel(nil)
					select {
					case out <- *finalChunk:
					case <-parentCtx.Done():
						cause := context.Cause(parentCtx)
						if cause == nil {
							cause = parentCtx.Err()
						}
						emitError(fmt.Errorf("timeout: stream canceled: %w", cause))
					case <-budgetCtx.Done():
						if parentCtx.Err() != nil {
							cause := context.Cause(parentCtx)
							if cause == nil {
								cause = parentCtx.Err()
							}
							emitError(fmt.Errorf("timeout: stream canceled: %w", cause))
						} else {
							emitError(timeoutError("total"))
						}
					}
					return
				}
				// Reset the chunk timer for the next chunk.
				if !chunkTimer.Stop() {
					select {
					case <-chunkTimer.C:
					default:
					}
				}
				chunkTimer.Reset(t.streamChunkTimeout)
				if chunk.Error != nil {
					fail(chunk.Error)
					return
				}
				if finalChunk != nil {
					fail(fmt.Errorf("timeout: stream emitted data after its final chunk"))
					return
				}
				if chunk.Final {
					final := chunk
					final.Content = ""
					finalChunk = &final
					if chunk.Content == "" {
						continue
					}
					contentOnly := chunk
					contentOnly.Final = false
					contentOnly.ToolCalls = nil
					contentOnly.FinishReason = ""
					contentOnly.Usage = nil
					chunk = contentOnly
				}
				select {
				case out <- chunk:
				case <-parentCtx.Done():
					cause := context.Cause(parentCtx)
					if cause == nil {
						cause = parentCtx.Err()
					}
					fail(fmt.Errorf("timeout: stream canceled: %w", cause))
					return
				case <-budgetCtx.Done():
					if parentCtx.Err() != nil {
						cause := context.Cause(parentCtx)
						if cause == nil {
							cause = parentCtx.Err()
						}
						fail(fmt.Errorf("timeout: stream canceled: %w", cause))
						return
					}
					fail(timeoutError("total"))
					return
				}
			}
		}
	}()

	return out, nil
}
