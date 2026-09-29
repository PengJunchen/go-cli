package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/production"
)

// retryPolicyAdapter wraps a production.RetryPolicy so it can be used where an
// llm.RetryPolicy is expected. The two interfaces are structurally compatible
// but declared separately to avoid an import cycle; this adapter bridges them.
type retryPolicyAdapter struct {
	inner production.RetryPolicy
}

func (a *retryPolicyAdapter) ShouldRetry(ctx context.Context, err error, attempt int) bool {
	return a.inner.ShouldRetry(ctx, err, attempt)
}

func (a *retryPolicyAdapter) NextBackoff(ctx context.Context, attempt int) time.Duration {
	return a.inner.NextBackoff(ctx, attempt)
}

func (a *retryPolicyAdapter) Name() string { return a.inner.Name() }

// outputGuardModel wraps an llm.BaseChatModel with an OutputGuard chain.
// It applies the guard to the Content field of the Generate response,
// replacing it with sanitized text when the guard flags a violation. Stream
// candidates are buffered until their complete text has passed the guard.
type outputGuardModel struct {
	inner llm.BaseChatModel
	guard production.OutputGuard
}

var _ llm.BaseChatModel = (*outputGuardModel)(nil)
var _ llm.ProviderOperationRunner = (*outputGuardModel)(nil)

func (m *outputGuardModel) Generate(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (*llm.Message, error) {
	resp, err := m.inner.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("output guard: model returned a nil response")
	}
	if m.guard != nil {
		result, checkErr := m.checkCandidate(ctx, resp.Content)
		if checkErr != nil {
			return nil, checkErr
		}
		if result.Sanitized != "" {
			resp.Content = result.Sanitized
		} else if !result.Allowed {
			return nil, errors.New("output guard: candidate denied")
		}
	}
	return resp, nil
}

func (m *outputGuardModel) Stream(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	if m.guard == nil {
		return m.inner.Stream(ctx, msgs, opts...)
	}
	if llm.LoopOwnsContinuation(ctx) {
		return m.inner.Stream(llm.WithLoopOwnedContinuation(ctx), msgs, opts...)
	}
	return m.streamAndGuardCompleteResponse(ctx, msgs, opts...)
}

func (m *outputGuardModel) MaxCandidateBytes() int { return llm.GuardedStreamMaxBytes }

func (m *outputGuardModel) ProviderCandidates() []llm.ResolvedCandidateHandle {
	return llm.ProviderCandidatesFromModel(m.inner)
}

// RunProviderOperation forwards the Core-owned schedule through outer model
// decorators so a breaker can admit the whole operation once. Core applies
// this guard to the completed candidate after each provider dispatch joins.
func (m *outputGuardModel) RunProviderOperation(ctx context.Context, run func(context.Context, llm.BaseChatModel) error) error {
	if runner, ok := m.inner.(llm.ProviderOperationRunner); ok {
		return runner.RunProviderOperation(ctx, run)
	}
	return run(ctx, m.inner)
}

func (m *outputGuardModel) CheckCandidate(ctx context.Context, text string) (*core.CandidateOutputResult, error) {
	result, err := m.checkCandidate(ctx, text)
	if err != nil {
		return nil, err
	}
	return &core.CandidateOutputResult{Allowed: result.Allowed, Sanitized: result.Sanitized}, nil
}

func (m *outputGuardModel) checkCandidate(ctx context.Context, text string) (*production.GuardResult, error) {
	if m.guard == nil {
		return &production.GuardResult{Allowed: true}, nil
	}
	if len(text) > llm.GuardedStreamMaxBytes {
		return nil, fmt.Errorf("output guard: candidate exceeds %d-byte limit", llm.GuardedStreamMaxBytes)
	}
	result, err := m.guard.Check(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("output guard: check candidate: %w", err)
	}
	if result == nil {
		return nil, errors.New("output guard: checker returned nil result")
	}
	if result.Sanitized != "" && result.Sanitized != text {
		slog.InfoContext(ctx, "output_guard_sanitized",
			"allowed", result.Allowed,
			"severity", string(result.Severity),
			"reason", result.Reason,
		)
	} else if !result.Allowed {
		slog.WarnContext(ctx, "output_guard_blocked",
			"severity", string(result.Severity),
			"reason", result.Reason,
		)
	}
	return result, nil
}

func (m *outputGuardModel) streamAndGuardCompleteResponse(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	streamCtx, cancel := context.WithCancelCause(ctx)
	inner, err := m.inner.Stream(streamCtx, msgs, opts...)
	if err != nil {
		cause := context.Cause(ctx)
		if cause == nil {
			cancel(llm.NewLocalStreamAbortCause(err))
		} else {
			cancel(cause)
		}
		if inner != nil {
			for range inner {
			}
		}
		return nil, err
	}
	if inner == nil {
		err = errors.New("output guard: model returned a nil stream")
		cancel(llm.NewLocalStreamAbortCause(err))
		return nil, err
	}
	out := make(chan llm.MessageChunk, 2)
	go func() {
		defer close(out)
		var content strings.Builder
		var contentBytes int
		var chunkCount int
		var final *llm.MessageChunk
		sourceClosed := false
		errorSent := false
		fail := func(err error) {
			if errorSent {
				return
			}
			errorSent = true
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
			if !sourceClosed {
				cause := context.Cause(ctx)
				if cause == nil {
					cancel(llm.NewLocalStreamAbortCause(err))
				} else {
					cancel(cause)
				}
				for range inner {
				}
				sourceClosed = true
			}
		}
		for {
			var chunk llm.MessageChunk
			var ok bool
			select {
			case <-ctx.Done():
				cause := context.Cause(ctx)
				if cause == nil {
					cause = ctx.Err()
				}
				fail(fmt.Errorf("output guard: stream canceled: %w", cause))
				return
			case chunk, ok = <-inner:
			}
			if !ok {
				sourceClosed = true
				break
			}
			if chunk.Error != nil {
				fail(chunk.Error)
				return
			}
			chunkCount++
			if chunkCount > llm.GuardedStreamMaxChunks {
				fail(fmt.Errorf("output guard: stream exceeds %d chunks", llm.GuardedStreamMaxChunks))
				return
			}
			textBytes := llm.RetainedStreamChunkTextBytes(chunk)
			if textBytes > llm.GuardedStreamMaxBytes-contentBytes {
				fail(fmt.Errorf("output guard: stream exceeds %d-byte limit", llm.GuardedStreamMaxBytes))
				return
			}
			contentBytes += textBytes
			if final != nil {
				fail(errors.New("output guard: stream emitted data after its final chunk"))
				return
			}
			if chunk.Content != "" {
				content.WriteString(chunk.Content)
			}
			if chunk.Final {
				copy := chunk
				final = &copy
			}
		}
		if ctx.Err() != nil {
			cause := context.Cause(ctx)
			if cause == nil {
				cause = ctx.Err()
			}
			fail(fmt.Errorf("output guard: stream canceled: %w", cause))
			return
		}
		if final == nil {
			fail(errors.New("output guard: stream ended without a final chunk"))
			return
		}
		result, checkErr := m.checkCandidate(streamCtx, content.String())
		if checkErr != nil {
			fail(checkErr)
			return
		}
		text := content.String()
		if result.Sanitized != "" {
			text = result.Sanitized
		} else if !result.Allowed {
			fail(errors.New("output guard: candidate denied"))
			return
		}
		if len(text) > llm.GuardedStreamMaxBytes {
			fail(fmt.Errorf("output guard: sanitized candidate exceeds %d-byte limit", llm.GuardedStreamMaxBytes))
			return
		}
		complete := &llm.Message{
			Role:         llm.RoleAssistant,
			Content:      text,
			ToolCalls:    final.ToolCalls,
			FinishReason: final.FinishReason,
			Usage:        final.Usage,
		}
		retainedBytes, sizeErr := llm.RetainedMessagePayloadBytes(complete)
		if sizeErr != nil {
			fail(fmt.Errorf("output guard: encode complete response: %w", sizeErr))
			return
		}
		if retainedBytes > llm.GuardedStreamMaxBytes {
			fail(fmt.Errorf("output guard: complete response exceeds %d-byte limit", llm.GuardedStreamMaxBytes))
			return
		}
		if text != "" {
			select {
			case out <- llm.MessageChunk{Role: llm.RoleAssistant, Content: text}:
			case <-ctx.Done():
				cause := context.Cause(ctx)
				if cause == nil {
					cause = ctx.Err()
				}
				fail(fmt.Errorf("output guard: stream canceled: %w", cause))
				return
			}
		}
		final.Content = ""
		select {
		case out <- *final:
		case <-ctx.Done():
			cause := context.Cause(ctx)
			if cause == nil {
				cause = ctx.Err()
			}
			fail(fmt.Errorf("output guard: stream canceled: %w", cause))
			return
		}
	}()
	return out, nil
}

// telemetryModel wraps an llm.BaseChatModel to record LLM token usage metrics
// into a production.Telemetry instance after each successful Generate call.
// Streaming responses are not instrumented because MessageChunk does not carry
// Usage information.
type telemetryModel struct {
	inner     llm.BaseChatModel
	telemetry production.Telemetry
}

var _ llm.BaseChatModel = (*telemetryModel)(nil)

func (m *telemetryModel) Generate(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (*llm.Message, error) {
	resp, err := m.inner.Generate(ctx, msgs, opts...)
	if err == nil && resp != nil && resp.Usage != nil && m.telemetry != nil {
		_ = m.telemetry.Record(ctx, production.TelemetryMetric{ //nolint:errcheck
			Name:  "llm.tokens.input",
			Value: float64(resp.Usage.InputTokens),
		})
		_ = m.telemetry.Record(ctx, production.TelemetryMetric{ //nolint:errcheck
			Name:  "llm.tokens.output",
			Value: float64(resp.Usage.OutputTokens),
		})
	}
	return resp, err
}

func (m *telemetryModel) Stream(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	return m.inner.Stream(ctx, msgs, opts...)
}

func (m *telemetryModel) ProviderCandidates() []llm.ResolvedCandidateHandle {
	return llm.ProviderCandidatesFromModel(m.inner)
}

// newModelWrapper creates a core.ModelWrapper that applies ProductionModelWrapper
// (retry + cost tracking) as the inner layer, a Telemetry recording layer, a
// CircuitBreaker, and an OutputGuard as the outer layer. The wrapper is
// consumed by core.WithModelWrapper in LoopAgent.
//
// The optional telemetry argument, when non-nil, adds a telemetryModel layer
// between the production wrapper and the circuit breaker so that LLM token
// usage is recorded as metrics.
func newModelWrapper(pw *production.ProductionModelWrapper, breaker production.CircuitBreaker, guard production.OutputGuard, telemetry ...production.Telemetry) core.ModelWrapper {
	var tel production.Telemetry
	if len(telemetry) > 0 {
		tel = telemetry[0]
	}
	return func(model any) any {
		baseModel, ok := model.(llm.BaseChatModel)
		if !ok {
			return model
		}
		wrapped := pw.WrapModel(baseModel)
		if tel != nil {
			wrapped = &telemetryModel{inner: wrapped, telemetry: tel}
		}
		if breaker != nil {
			wrapped = &circuitBreakerModel{inner: wrapped, breaker: breaker}
		}
		if guard != nil {
			return &outputGuardModel{inner: wrapped, guard: guard}
		}
		return wrapped
	}
}

// newModelWrapperWithChain creates a core.ModelWrapper that applies the
// ProductionModelWrapper (cost tracking only, no retry) as the innermost layer,
// then the 7-layer ModelMiddlewareChain (failover, retry, timeout, sanitize,
// loop detection, validate, overflow), then optional Telemetry, CircuitBreaker,
// and OutputGuard layers from inner to outer.
//
// The optional telemetry argument, when non-nil, adds a telemetryModel layer
// between the middleware chain and the circuit breaker so that LLM token usage
// is recorded as metrics.
func newModelWrapperWithChain(pw *production.ProductionModelWrapper, chain *llm.DefaultModelMiddlewareChain, breaker production.CircuitBreaker, guard production.OutputGuard, telemetry ...production.Telemetry) core.ModelWrapper {
	var tel production.Telemetry
	if len(telemetry) > 0 {
		tel = telemetry[0]
	}
	return func(model any) any {
		baseModel, ok := model.(llm.BaseChatModel)
		if !ok {
			return model
		}
		// 1. Innermost: cost tracking + stats (no retry, since the chain handles it).
		wrapped := pw.WrapModel(baseModel)
		// 2. 7-layer middleware chain (failover -> retry -> timeout -> ...).
		wrapped = chain.Wrap(wrapped)
		// 3. Telemetry recording.
		if tel != nil {
			wrapped = &telemetryModel{inner: wrapped, telemetry: tel}
		}
		// 4. Circuit breaker.
		if breaker != nil {
			wrapped = &circuitBreakerModel{inner: wrapped, breaker: breaker}
		}
		// 5. Outermost: output guard.
		if guard != nil {
			return &outputGuardModel{inner: wrapped, guard: guard}
		}
		return wrapped
	}
}
