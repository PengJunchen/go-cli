package production

import (
	"context"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// This file implements OutputGuardMiddleware, an extension.ModelMiddleware that
// runs an OutputGuard on every model response and, when the guard denies the
// output, replaces it with a sanitized/blocked text so a blocked response does
// not propagate to the caller.

// blockedOutputMessage is the safe text substituted for a denied output.
var blockedOutputMessage = "Blocked: the model output was rejected by the output guard."

// OutputGuardMiddleware wraps a ModelFunc to guard the model response text.
type OutputGuardMiddleware struct {
	name  string
	guard OutputGuard
}

// Compile-time assertion that OutputGuardMiddleware satisfies the extension
// ModelMiddleware contract.
var _ extension.ModelMiddleware = (*OutputGuardMiddleware)(nil)

// NewOutputGuardMiddleware returns an OutputGuardMiddleware that runs guard on
// every model response. Options may override the middleware name.
func NewOutputGuardMiddleware(guard OutputGuard, opts ...Option) *OutputGuardMiddleware {
	o := applyOptions(opts)
	name := o.name
	if name == "" {
		name = "output-guard-middleware"
	}
	return &OutputGuardMiddleware{name: name, guard: guard}
}

// Name returns the middleware identifier.
func (m *OutputGuardMiddleware) Name() string { return m.name }

// WrapModel wraps next so that its response text is passed through the guard.
// If the guard denies the output, ModelResponse.Text is replaced with a safe
// (sanitized or blocked) value and returned, so the original blocked text never
// reaches the caller.
func (m *OutputGuardMiddleware) WrapModel(next extension.ModelFunc) extension.ModelFunc {
	return func(ctx context.Context, req extension.ModelRequest) (extension.ModelResponse, error) {
		span, _ := tracing.SpanFromContext(ctx, "middleware.output_guard", tracing.SpanKindInternal)
		defer span.End()
		logger := tracing.NewTraceLogger(span, slog.Default())

		resp, err := next(ctx, req)
		if err != nil {
			span.SetAttributes(tracing.Attribute{Key: "guard_name", Value: m.Name()})
			span.SetStatus(tracing.SpanStatusError, err.Error())
			logger.WarnContext(ctx, "output_guard_middleware",
				"guard_name", m.Name(),
				"model_error", err.Error(),
			)
			return resp, err
		}

		result, guardErr := m.guard.Check(ctx, resp.Text)
		if guardErr != nil {
			span.SetAttributes(tracing.Attribute{Key: "guard_name", Value: m.Name()})
			span.SetStatus(tracing.SpanStatusError, guardErr.Error())
			logger.WarnContext(ctx, "output_guard_middleware",
				"guard_name", m.Name(),
				"guard_error", guardErr.Error(),
			)
			// Fail closed: a guard that cannot run must not leak raw output.
			resp.Text = blockedOutputMessage
			return resp, guardErr
		}

		if !result.Allowed {
			// Replace the response with the sanitized text, falling back to a
			// generic blocked message when the guard produced no replacement.
			if result.Sanitized == "" {
				resp.Text = blockedOutputMessage
			} else {
				resp.Text = result.Sanitized
			}
			span.SetAttributes(
				tracing.Attribute{Key: "guard_name", Value: m.Name()},
				tracing.Attribute{Key: "allowed", Value: false},
				tracing.Attribute{Key: "severity", Value: string(result.Severity)},
				tracing.Attribute{Key: "reason", Value: result.Reason},
				tracing.Attribute{Key: "sanitized", Value: result.Sanitized != ""},
			)
			span.SetStatus(tracing.SpanStatusOK, "")
			logger.InfoContext(ctx, "output_guard_middleware",
				"guard_name", m.Name(),
				"allowed", false,
				"severity", string(result.Severity),
				"reason", result.Reason,
			)
			return resp, nil
		}

		span.SetAttributes(
			tracing.Attribute{Key: "guard_name", Value: m.Name()},
			tracing.Attribute{Key: "allowed", Value: true},
			tracing.Attribute{Key: "severity", Value: string(result.Severity)},
			tracing.Attribute{Key: "reason", Value: result.Reason},
		)
		span.SetStatus(tracing.SpanStatusOK, "")
		logger.InfoContext(ctx, "output_guard_middleware",
			"guard_name", m.Name(),
			"allowed", true,
			"severity", string(result.Severity),
			"reason", result.Reason,
		)
		return resp, nil
	}
}
