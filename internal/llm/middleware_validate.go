// Package llm middleware_validate.go - ValidateModelMiddleware validates
// responses from a BaseChatModel before returning them to the caller.
package llm

import (
	"context"
	"errors"
	"log/slog"
)

// Sentinel errors returned by the validate middleware.
var (
	// errValidateEmptyResponse is returned when Generate returns a nil message.
	errValidateEmptyResponse = errors.New("validate: empty response")
	// errValidateEmptyContent is returned when the message has neither content
	// nor tool calls.
	errValidateEmptyContent = errors.New("validate: empty response content")
	// errValidateEmptyStream is returned when Stream closes with zero chunks.
	errValidateEmptyStream = errors.New("validate: empty stream")
)

// ValidateModelMiddleware validates model responses before returning them to
// the caller. It checks that Generate returns a non-nil message with content or
// tool calls, and that Stream produces at least one chunk.
type ValidateModelMiddleware struct {
	logger *slog.Logger
}

// Compile-time assertion that ValidateModelMiddleware satisfies ModelMiddleware.
var _ ModelMiddleware = (*ValidateModelMiddleware)(nil)

// ValidateOption configures ValidateModelMiddleware at construction time.
type ValidateOption func(*ValidateModelMiddleware)

// WithValidateLogger overrides the default logger.
func WithValidateLogger(logger *slog.Logger) ValidateOption {
	return func(m *ValidateModelMiddleware) { m.logger = logger }
}

// NewValidateModelMiddleware creates a ValidateModelMiddleware.
func NewValidateModelMiddleware(opts ...ValidateOption) *ValidateModelMiddleware {
	m := &ValidateModelMiddleware{
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Name returns "validate".
func (m *ValidateModelMiddleware) Name() string { return "validate" }

// WrapModel returns a BaseChatModel that validates responses.
func (m *ValidateModelMiddleware) WrapModel(next BaseChatModel) BaseChatModel {
	return &validateModel{
		next:   next,
		logger: m.logger,
	}
}

// validateModel is the wrapped BaseChatModel produced by ValidateModelMiddleware.
type validateModel struct {
	next   BaseChatModel
	logger *slog.Logger
}

// Compile-time assertion that validateModel satisfies BaseChatModel.
var _ BaseChatModel = (*validateModel)(nil)

// Generate validates the returned message: it must be non-nil and contain either
// content or tool calls. Errors from the underlying model are passed through
// unchanged.
func (v *validateModel) Generate(ctx context.Context, msgs []Message, opts ...Option) (*Message, error) {
	msg, err := v.next.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	if msg == nil {
		v.logger.Debug("middleware.validate",
			"status", "fail",
			"reason", "nil_response",
		)
		return nil, errValidateEmptyResponse
	}
	if msg.Content == "" && len(msg.ToolCalls) == 0 {
		v.logger.Debug("middleware.validate",
			"status", "fail",
			"reason", "empty_content",
		)
		return nil, errValidateEmptyContent
	}
	v.logger.Debug("middleware.validate", "status", "pass")
	return msg, nil
}

// Stream validates that at least one chunk is received. If the inner channel
// closes with zero chunks an error is returned from the Stream call itself.
// Otherwise the first chunk and all subsequent chunks are forwarded to the
// caller.
func (v *validateModel) Stream(ctx context.Context, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	streamCtx, cancel := context.WithCancelCause(ctx)
	ch, err := v.next.Stream(streamCtx, msgs, opts...)
	if err != nil {
		cancelStreamAttempt(ctx, cancel, err)
		drainMessageChunks(ch)
		return nil, err
	}
	if ch == nil {
		err = errors.New("validate: model returned a nil stream")
		cancelStreamAttempt(ctx, cancel, err)
		return nil, err
	}

	// Peek at the first chunk; if the channel closes immediately, return an error.
	// Use select with ctx.Done() so the call does not block forever if the
	// upstream provider hangs without sending or closing the channel.
	var first MessageChunk
	var ok bool
	select {
	case first, ok = <-ch:
	case <-streamCtx.Done():
		cause := context.Cause(ctx)
		if cause == nil {
			cause = context.Cause(streamCtx)
		}
		cancelStreamAttempt(ctx, cancel, cause)
		drainMessageChunks(ch)
		if cause == nil {
			cause = ctx.Err()
		}
		return nil, cause
	}
	if !ok {
		cancelStreamAttempt(ctx, cancel, errValidateEmptyStream)
		v.logger.Debug("middleware.validate",
			"status", "fail",
			"reason", "empty_stream",
		)
		return nil, errValidateEmptyStream
	}
	if first.Error != nil {
		cancelStreamAttempt(ctx, cancel, first.Error)
		drainMessageChunks(ch)
		return nil, first.Error
	}
	if streamCtx.Err() != nil {
		cause := context.Cause(ctx)
		if cause == nil {
			cause = context.Cause(streamCtx)
		}
		cancelStreamAttempt(ctx, cancel, cause)
		drainMessageChunks(ch)
		if cause == nil {
			cause = ctx.Err()
		}
		return nil, cause
	}

	v.logger.Debug("middleware.validate", "status", "pass")

	out := make(chan MessageChunk, 1)
	out <- first

	go func() {
		defer close(out)
		for {
			var chunk MessageChunk
			var ok bool
			select {
			case <-streamCtx.Done():
				cause := context.Cause(ctx)
				if cause == nil {
					cause = context.Cause(streamCtx)
				}
				cancelStreamAttempt(ctx, cancel, cause)
				drainMessageChunks(ch)
				return
			case chunk, ok = <-ch:
			}
			if !ok {
				cancel(nil)
				return
			}
			if chunk.Error != nil {
				sendStreamError(streamCtx, out, chunk.Error)
				cancelStreamAttempt(ctx, cancel, chunk.Error)
				drainMessageChunks(ch)
				return
			}
			select {
			case out <- chunk:
			case <-streamCtx.Done():
				cause := context.Cause(ctx)
				if cause == nil {
					cause = context.Cause(streamCtx)
				}
				cancelStreamAttempt(ctx, cancel, cause)
				drainMessageChunks(ch)
				return
			}
		}
	}()

	return out, nil
}
