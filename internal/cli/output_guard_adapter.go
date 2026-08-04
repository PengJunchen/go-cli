package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/production"
)

// outputGuardModel wraps an llm.BaseChatModel with an OutputGuard chain.
// It applies the guard to the Content field of the Generate response,
// replacing it with sanitized text when the guard flags a violation.
// Stream calls pass through unchanged (guards operate on complete text only).
type outputGuardModel struct {
	inner llm.BaseChatModel
	guard production.OutputGuard
}

var _ llm.BaseChatModel = (*outputGuardModel)(nil)

func (m *outputGuardModel) Generate(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (*llm.Message, error) {
	resp, err := m.inner.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	if resp != nil && resp.Content != "" && m.guard != nil {
		result, gErr := m.guard.Check(ctx, resp.Content)
		if gErr == nil && result != nil {
			if result.Sanitized != "" && result.Sanitized != resp.Content {
				slog.InfoContext(ctx, "output_guard_sanitized",
					"allowed", result.Allowed,
					"severity", string(result.Severity),
					"reason", result.Reason,
				)
				resp.Content = result.Sanitized
			} else if !result.Allowed {
				slog.WarnContext(ctx, "output_guard_blocked",
					"severity", string(result.Severity),
					"reason", result.Reason,
				)
				resp.Content = "[output blocked by safety guard]"
			}
		}
	}
	return resp, nil
}

func (m *outputGuardModel) Stream(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	return m.inner.Stream(ctx, msgs, opts...)
}

// circuitBreakerModel wraps an llm.BaseChatModel with CircuitBreaker protection.
// When the circuit is Open, Generate returns a fallback response instead of
// calling the underlying model. Stream calls pass through directly (the breaker
// guards the synchronous Generate path only).
type circuitBreakerModel struct {
	inner   llm.BaseChatModel
	breaker production.CircuitBreaker
}

var _ llm.BaseChatModel = (*circuitBreakerModel)(nil)

func (m *circuitBreakerModel) Generate(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (*llm.Message, error) {
	result, err := m.breaker.Execute(ctx, func() (any, error) {
		return m.inner.Generate(ctx, msgs, opts...)
	})
	if err != nil {
		if errors.Is(err, production.ErrCircuitOpen) {
			slog.WarnContext(ctx, "circuit_breaker_open_fallback",
				"breaker", m.breaker.Name(),
			)
			return &llm.Message{
				Role:    llm.RoleAssistant,
				Content: "Service temporarily unavailable, please retry later.",
			}, nil
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
	return m.inner.Stream(ctx, msgs, opts...)
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
