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
	ch, err := v.next.Stream(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}

	// Peek at the first chunk; if the channel closes immediately, return an error.
	first, ok := <-ch
	if !ok {
		v.logger.Debug("middleware.validate",
			"status", "fail",
			"reason", "empty_stream",
		)
		return nil, errValidateEmptyStream
	}

	v.logger.Debug("middleware.validate", "status", "pass")

	out := make(chan MessageChunk, 1)
	out <- first

	go func() {
		defer close(out)
		for chunk := range ch {
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, nil
}
