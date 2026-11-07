package llm

import (
	"context"
	"errors"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

type requestAttemptKey struct{}

func withRequestAttempt(ctx context.Context, attempt int) context.Context {
	return context.WithValue(ctx, requestAttemptKey{}, attempt)
}

func requestAttemptFromContext(ctx context.Context) (int, bool) {
	attempt, ok := ctx.Value(requestAttemptKey{}).(int)
	return attempt, ok
}

func startLLMRequestSpan(ctx context.Context, provider, model string) (tracing.TraceSpan, context.Context, time.Time) {
	span, spanCtx := tracing.SpanFromContext(ctx, spanNameLLMRequest, tracing.SpanKindClient)
	started := time.Now()
	span.SetAttributes(
		tracing.Attribute{Key: "provider", Value: provider},
		tracing.Attribute{Key: "model", Value: model},
		tracing.Attribute{Key: "task_type", Value: string(TaskTypeFromContext(ctx))},
		tracing.Attribute{Key: "purpose", Value: string(RequestPurposeFromContext(ctx))},
	)
	span.SetAttributes(requestAttemptAttributes(ctx)...)
	return span, spanCtx, started
}

func requestAttemptAttributes(ctx context.Context) []tracing.Attribute {
	if proposal, ok := CandidateProposalFromContext(ctx); ok {
		return []tracing.Attribute{{Key: "attempt_reason", Value: candidateReasonName(proposal.Reason)}}
	}
	if attempt, ok := requestAttemptFromContext(ctx); ok {
		reason := "initial"
		if attempt > 0 {
			reason = "retry"
		}
		return []tracing.Attribute{
			{Key: "attempt_reason", Value: reason},
			{Key: "attempt_index", Value: attempt},
		}
	}
	return []tracing.Attribute{{Key: "attempt_reason", Value: "initial"}}
}

func candidateReasonName(reason CandidateReason) string {
	switch reason {
	case CandidateInitial:
		return "initial"
	case CandidateRetry:
		return "retry"
	case CandidateFailover:
		return "failover"
	case CandidateOverflow:
		return "overflow"
	case CandidateCyclerSelected:
		return "cycler_selected"
	case CandidateCyclerPrimary:
		return "cycler_primary"
	default:
		return "unknown"
	}
}

func finishLLMRequestSpan(span tracing.TraceSpan, started time.Time, err error, usage *Usage) {
	if err != nil {
		setProviderErrorSpanAttributes(span, err)
		span.SetStatus(tracing.SpanStatusError, safeLLMTraceError(err))
	} else {
		span.SetStatus(tracing.SpanStatusOK, "")
	}
	span.SetAttributes(tracing.Attribute{Key: "usage_state", Value: usageStateName(usage)})
	if usage != nil {
		span.SetAttributes(
			tracing.Attribute{Key: "tokens_input", Value: usage.InputTokens},
			tracing.Attribute{Key: "tokens_output", Value: usage.OutputTokens},
			tracing.Attribute{Key: "primary_coverage", Value: string(usage.PrimaryCoverage.Normalized())},
		)
		if usage.CacheReadInputTokensReported && validUsageDetail(usage.CacheReadInputTokens, usage.InputTokens, true) {
			span.SetAttributes(tracing.Attribute{Key: "tokens_cache_read_input", Value: usage.CacheReadInputTokens})
		}
		if usage.ReasoningOutputTokensReported && validUsageDetail(usage.ReasoningOutputTokens, usage.OutputTokens, true) {
			span.SetAttributes(tracing.Attribute{Key: "tokens_reasoning_output", Value: usage.ReasoningOutputTokens})
		}
	}
	span.SetAttributes(tracing.Attribute{Key: "latency_ms", Value: time.Since(started).Milliseconds()})
	span.End()
}

func usageStateName(usage *Usage) string {
	if usage == nil {
		return "unknown"
	}
	return "known"
}

func safeLLMTraceError(err error) string {
	var providerErr *ProviderError
	if errors.As(err, &providerErr) && providerErr != nil {
		return providerErr.Error()
	}
	if errors.Is(err, context.Canceled) {
		return "LLM request canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "LLM request deadline exceeded"
	}
	return "LLM request failed"
}

func traceLLMMessageStream(ctx context.Context, span tracing.TraceSpan, source <-chan MessageChunk, started time.Time) <-chan MessageChunk {
	bufferSize := cap(source)
	if bufferSize == 0 {
		bufferSize = 1
	}
	out := make(chan MessageChunk, bufferSize)
	if source == nil {
		close(out)
		finishLLMRequestSpan(span, started, errors.New("llm: provider returned a nil stream"), nil)
		return out
	}
	go func() {
		defer close(out)
		var usage *Usage
		var terminalErr error
		contextDone := ctx.Done()
		consumerActive := true
		for {
			select {
			case <-contextDone:
				consumerActive = false
				terminalErr = context.Cause(ctx)
				if terminalErr == nil {
					terminalErr = ctx.Err()
				}
				contextDone = nil
			case chunk, ok := <-source:
				if !ok {
					if terminalErr == nil && ctx.Err() != nil {
						terminalErr = context.Cause(ctx)
						if terminalErr == nil {
							terminalErr = ctx.Err()
						}
					}
					finishLLMRequestSpan(span, started, terminalErr, usage)
					return
				}
				if chunk.Usage != nil {
					copyUsage := *chunk.Usage
					usage = &copyUsage
				}
				if chunk.Error != nil && terminalErr == nil {
					terminalErr = chunk.Error
				}
				if consumerActive {
					select {
					case out <- chunk:
					case <-contextDone:
						consumerActive = false
						terminalErr = context.Cause(ctx)
						if terminalErr == nil {
							terminalErr = ctx.Err()
						}
						contextDone = nil
					}
				}
				if !consumerActive && chunk.Error != nil {
					// Preserve the stream's terminal failure for callers that keep
					// draining after cancellation, including when cancellation wins
					// the send select for this same chunk.
					sendStreamError(ctx, out, chunk.Error)
				}
			}
		}
	}()
	return out
}
