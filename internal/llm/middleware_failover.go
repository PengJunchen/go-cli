// Package llm middleware_failover.go - FailoverModelMiddleware tries the
// primary model first and falls back to alternative models in priority order
// when the primary returns an error.
package llm

import (
	"context"
	"errors"
	"log/slog"
)

// FailoverOption configures a FailoverModelMiddleware.
type FailoverOption func(*FailoverModelMiddleware)

// WithFallbackModels appends ordered fallback models tried after the primary
// model fails. Models are attempted in the order provided.
func WithFallbackModels(models ...BaseChatModel) FailoverOption {
	return func(f *FailoverModelMiddleware) {
		f.fallbacks = append(f.fallbacks, models...)
	}
}

// WithFailoverLogger sets the logger used for failover tracing. If logger is
// nil the option is a no-op (the default logger is kept).
func WithFailoverLogger(logger *slog.Logger) FailoverOption {
	return func(f *FailoverModelMiddleware) {
		if logger != nil {
			f.logger = logger
		}
	}
}

// FailoverModelMiddleware tries the wrapped primary model first; on error it
// tries each fallback model in order. If all models fail it returns the last
// error. For streaming, only the initial Stream call error triggers a
// fallback; errors that occur mid-stream are surfaced to the caller as-is.
type FailoverModelMiddleware struct {
	fallbacks []BaseChatModel
	logger    *slog.Logger
}

var _ ModelMiddleware = (*FailoverModelMiddleware)(nil)

// NewFailoverModelMiddleware creates a FailoverModelMiddleware with the given
// options. The default logger is slog.Default().
func NewFailoverModelMiddleware(opts ...FailoverOption) *FailoverModelMiddleware {
	m := &FailoverModelMiddleware{logger: slog.Default()}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Name returns "failover".
func (f *FailoverModelMiddleware) Name() string { return "failover" }

// WrapModel returns a BaseChatModel that tries the primary model first and
// falls back to the configured fallback models on error.
func (f *FailoverModelMiddleware) WrapModel(primary BaseChatModel) BaseChatModel {
	return &failoverModel{
		primary:   primary,
		fallbacks: f.fallbacks,
		logger:    f.logger,
	}
}

// failoverModel is the wrapped BaseChatModel produced by
// FailoverModelMiddleware.
type failoverModel struct {
	primary   BaseChatModel
	fallbacks []BaseChatModel
	logger    *slog.Logger
}

var _ BaseChatModel = (*failoverModel)(nil)

// orderedModels returns a fresh slice of models to try: primary first, then
// fallbacks in registration order.
func (m *failoverModel) orderedModels() []BaseChatModel {
	out := make([]BaseChatModel, 0, 1+len(m.fallbacks))
	out = append(out, m.primary)
	out = append(out, m.fallbacks...)
	return out
}

// Generate tries each model in order, returning the first successful response.
// If all models fail it returns the last error. Fatal errors (auth,
// permission) short-circuit: the error is returned immediately without trying
// fallbacks. Each failover attempt is traced at debug level.
func (m *failoverModel) Generate(ctx context.Context, msgs []Message, opts ...Option) (*Message, error) {
	if _, operationActive := OperationDispatchAuthorityFromContext(ctx); operationActive {
		return m.generateWithOperation(ctx, msgs, opts...)
	}
	models := m.orderedModels()
	var lastErr error
	for i, model := range models {
		resp, err := model.Generate(ctx, msgs, opts...)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if ContinuationNoReplay(ctx) {
			return nil, lastErr
		}
		// Fatal errors (auth, permission) should not trigger a fallback.
		if classifyError(err) == categoryFatal {
			return nil, err
		}
		if i < len(models)-1 {
			m.logger.Debug("failover.generate.failed_trying_next",
				"index", i, "error", err)
		}
	}
	return nil, lastErr
}

// Stream tries each model in order, returning the first successful stream.
// Only the initial Stream call error triggers a fallback; errors that occur
// mid-stream (after a channel has been returned) are not retried. Fatal errors
// (auth, permission) short-circuit: the error is returned immediately without
// trying fallbacks. Each failover attempt is traced at debug level.
func (m *failoverModel) Stream(ctx context.Context, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	if _, operationActive := OperationDispatchAuthorityFromContext(ctx); operationActive {
		return m.streamWithOperation(ctx, msgs, opts...)
	}
	models := m.orderedModels()
	var lastErr error
	for i, model := range models {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attemptCtx, cancel := context.WithCancelCause(ctx)
		ch, err := model.Stream(attemptCtx, msgs, opts...)
		if err == nil && ch != nil {
			return forwardMessageStreamAttempt(ctx, cancel, ch), nil
		}
		if err == nil {
			err = errors.New("failover: model returned a nil stream without an error")
			cancelStreamAttempt(ctx, cancel, err)
			return nil, err
		}
		cancelStreamAttempt(ctx, cancel, err)
		if ch != nil {
			drainMessageChunks(ch)
		}
		lastErr = err
		if ContinuationNoReplay(ctx) {
			return nil, lastErr
		}
		// Fatal errors (auth, permission) should not trigger a fallback.
		if classifyError(err) == categoryFatal {
			return nil, err
		}
		if i < len(models)-1 {
			m.logger.Debug("failover.stream.failed_trying_next",
				"index", i, "error", err)
		}
	}
	return nil, lastErr
}

func (m *failoverModel) generateWithOperation(ctx context.Context, msgs []Message, opts ...Option) (*Message, error) {
	models := m.orderedModels()
	if len(models) == 0 {
		return nil, errors.New("llm: failover has no candidate models")
	}
	start := 0
	proposal, hasProposal := CandidateProposalFromContext(ctx)
	if hasProposal {
		var found bool
		start, found = failoverModelIndex(models, proposal.Candidate)
		if !found {
			return nil, ErrDispatchPermitRequired
		}
	} else if len(ProviderCandidatesFromModel(models[0])) == 0 {
		return nil, ErrOperationAuthorityRequired
	}

	// One active call corresponds to one Core-authorized proposal. Candidate
	// order and subsequent calls belong to the Loop scheduler.
	return models[start].Generate(ctx, msgs, opts...)
}

func (m *failoverModel) streamWithOperation(ctx context.Context, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	models := m.orderedModels()
	if len(models) == 0 {
		return nil, errors.New("llm: failover has no candidate models")
	}
	start := 0
	proposal, hasProposal := CandidateProposalFromContext(ctx)
	if hasProposal {
		var found bool
		start, found = failoverModelIndex(models, proposal.Candidate)
		if !found {
			return nil, ErrDispatchPermitRequired
		}
	} else if len(ProviderCandidatesFromModel(models[0])) == 0 {
		return nil, ErrOperationAuthorityRequired
	}

	// The caller has attached either the initial candidate proposal or a
	// Loop-planned follow-up. Dispatch only that candidate once.
	attemptCtx, cancel := context.WithCancelCause(ctx)
	stream, err := models[start].Stream(attemptCtx, msgs, opts...)
	if err == nil && stream != nil {
		return forwardMessageStreamAttempt(ctx, cancel, stream), nil
	}
	if err == nil {
		err = errors.New("failover: model returned a nil stream without an error")
	}
	cancelStreamAttempt(ctx, cancel, err)
	drainMessageChunks(stream)
	return nil, err
}

func failoverModelIndex(models []BaseChatModel, candidate ResolvedCandidateHandle) (int, bool) {
	if candidate == nil {
		return 0, false
	}
	for index, model := range models {
		for _, current := range ProviderCandidatesFromModel(model) {
			if current == candidate {
				return index, true
			}
		}
	}
	return 0, false
}
