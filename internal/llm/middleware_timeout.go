// Package llm middleware_timeout.go - TimeoutModelMiddleware enforces total and
// per-chunk timeouts on a BaseChatModel.
package llm

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// TimeoutModelMiddleware enforces a total deadline on Generate/Stream and a
// per-chunk idle timeout on Stream. When either timeout fires the context is
// cancelled so the underlying model can clean up promptly.
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
	if err != nil && ctx.Err() != nil {
		t.logger.Debug("middleware.timeout",
			"op", "generate",
			"status", "timeout",
			"total", t.totalTimeout.String(),
		)
		return nil, fmt.Errorf("timeout: generate exceeded %s: %w", t.totalTimeout, ctx.Err())
	}
	return msg, err
}

// Stream wraps the context with totalTimeout and monitors chunk arrival times.
// If no chunk arrives within streamChunkTimeout the context is cancelled and the
// output channel is closed. The initial Stream() call error (if any) is returned
// directly; mid-stream timeouts manifest as a closed output channel.
func (t *timeoutModel) Stream(ctx context.Context, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	ctx, cancel := context.WithTimeout(ctx, t.totalTimeout)

	ch, err := t.next.Stream(ctx, msgs, opts...)
	if err != nil {
		cancel()
		return nil, err
	}

	out := make(chan MessageChunk)
	go func() {
		defer cancel()
		defer close(out)

		chunkTimer := time.NewTimer(t.streamChunkTimeout)
		defer chunkTimer.Stop()

		for {
			select {
			case <-ctx.Done():
				t.logger.Debug("middleware.timeout",
					"op", "stream",
					"status", "timeout",
					"reason", "total",
				)
				return
			case <-chunkTimer.C:
				t.logger.Debug("middleware.timeout",
					"op", "stream",
					"status", "timeout",
					"reason", "chunk_idle",
				)
				cancel()
				return
			case chunk, ok := <-ch:
				if !ok {
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
				select {
				case out <- chunk:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out, nil
}
