package production

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// ErrorCategory classifies an error's retryability.
type ErrorCategory string

// Retryable error categories.
const (
	// ErrorTransient marks temporary, typically retryable failures.
	ErrorTransient ErrorCategory = "transient"
	// ErrorRateLimit marks throttling responses that benefit from backoff.
	ErrorRateLimit ErrorCategory = "rate_limit"
	// ErrorTimeout marks deadline / timeout failures worth a retry.
	ErrorTimeout ErrorCategory = "timeout"
	// ErrorFatal marks permanent failures that should never be retried.
	ErrorFatal ErrorCategory = "fatal"
)

// String returns the canonical string form of the category.
func (c ErrorCategory) String() string { return string(c) }

// RetryConfig tunes the exponential-backoff retry policy.
type RetryConfig struct {
	// MaxAttempts is the total number of attempts allowed; ShouldRetry returns
	// false once attempt >= MaxAttempts.
	MaxAttempts int
	// BaseDelay is the starting backoff for the first retry.
	BaseDelay time.Duration
	// MaxDelay caps the computed backoff.
	MaxDelay time.Duration
	// Jitter adds a random delay in [0, Jitter) to each backoff.
	Jitter time.Duration
}

// categorized is implemented by errors that carry an explicit category.
type categorized interface{ ErrorCategory() ErrorCategory }

// Compile-time assertion that CategorizedError satisfies categorized.
var _ categorized = (*CategorizedError)(nil) //nolint:errcheck // interface conformance check

// CategorizedError is an error tagged with an explicit ErrorCategory so that
// Classify can identify it without string matching.
type CategorizedError struct {
	category ErrorCategory
	err      error
}

// NewError wraps err with an explicit category.
func NewError(category ErrorCategory, err error) error {
	if err == nil {
		return nil
	}
	return &CategorizedError{category: category, err: err}
}

// ErrorCategory returns the explicit category of the wrapped error.
func (e *CategorizedError) ErrorCategory() ErrorCategory { return e.category }

// Error returns the underlying error message.
func (e *CategorizedError) Error() string {
	if e.err == nil {
		return string(e.category)
	}
	return e.err.Error()
}

// Unwrap returns the underlying error for errors.Is / errors.As compatibility.
func (e *CategorizedError) Unwrap() error { return e.err }

// RetryPolicy decides whether and how long to back off between attempts.
type RetryPolicy interface {
	// ShouldRetry reports whether attempt (0-based) should be retried after err.
	ShouldRetry(ctx context.Context, err error, attempt int) bool
	// NextBackoff returns the delay before the next attempt, exponential with
	// jitter and capped at MaxDelay. It is never negative.
	NextBackoff(ctx context.Context, attempt int) time.Duration
	// Name returns the policy identifier.
	Name() string
}

// DefaultRetryPolicy implements a jittered exponential-backoff policy driven by
// error classification.
type DefaultRetryPolicy struct {
	cfg  RetryConfig
	name string
}

// Compile-time assertion that DefaultRetryPolicy satisfies RetryPolicy.
var _ RetryPolicy = (*DefaultRetryPolicy)(nil)

// NewDefaultRetryPolicy returns a DefaultRetryPolicy backed by cfg with
// sensible defaults for zero-valued fields.
func NewDefaultRetryPolicy(cfg RetryConfig, opts ...Option) *DefaultRetryPolicy {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = 100 * time.Millisecond
	}
	// Jitter is left as-is: a zero value is a valid "no jitter" configuration.

	o := applyOptions(opts)
	name := o.name
	if name == "" {
		name = "default-retry-policy"
	}

	return &DefaultRetryPolicy{cfg: cfg, name: name}
}

// Classify determines the ErrorCategory of err.
func (p *DefaultRetryPolicy) Classify(err error) ErrorCategory {
	if err == nil {
		return ErrorFatal
	}
	if c, ok := err.(categorized); ok {
		return c.ErrorCategory()
	}
	switch {
	case isRateLimit(err):
		return ErrorRateLimit
	case isTimeout(err):
		return ErrorTimeout
	case isTransient(err):
		return ErrorTransient
	default:
		return ErrorFatal
	}
}

// ShouldRetry reports whether attempt (0-based) may be retried after err: it is
// true only when the attempt count is below MaxAttempts and the error category
// is retryable. It emits a production.retry span when a retry is warranted.
func (p *DefaultRetryPolicy) ShouldRetry(ctx context.Context, err error, attempt int) bool {
	if attempt >= p.cfg.MaxAttempts {
		return false
	}
	category := p.Classify(err)
	if category == ErrorFatal {
		return false
	}

	span, ctx := tracing.SpanFromContext(ctx, "production.retry", tracing.SpanKindInternal)
	defer span.End()
	backoff := p.NextBackoff(ctx, attempt)
	span.SetAttributes(
		tracing.Attribute{Key: "attempt", Value: attempt},
		tracing.Attribute{Key: "category", Value: category.String()},
		tracing.Attribute{Key: "backoff", Value: backoff.String()},
	)
	logger := tracing.NewTraceLogger(span, slog.Default())
	logger.WarnContext(ctx, "retry_decision",
		"attempt", attempt,
		"category", category.String(),
		"backoff", backoff.String(),
		"err", err,
	)
	return true
}

// NextBackoff computes min(baseDelay*2^attempt + rand[0,jitter), MaxDelay).
// It never returns a negative duration.
func (p *DefaultRetryPolicy) NextBackoff(_ context.Context, attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}

	exp := p.cfg.BaseDelay
	for i := 0; i < attempt; i++ {
		exp *= 2
		if p.cfg.MaxDelay > 0 && exp >= p.cfg.MaxDelay {
			exp = p.cfg.MaxDelay
			break
		}
	}

	delay := exp
	if p.cfg.Jitter > 0 {
		if j, ok := randomJitter(p.cfg.Jitter); ok {
			delay += j
		}
	}
	if p.cfg.MaxDelay > 0 && delay > p.cfg.MaxDelay {
		delay = p.cfg.MaxDelay
	}
	if delay < 0 {
		delay = 0
	}
	return delay
}

// Name returns the policy identifier.
func (p *DefaultRetryPolicy) Name() string { return p.name }

// randomJitter returns a crypto-derived random duration in [0, jitter), or ok
// = false if the underlying randomness source fails. It is used only for
// evenly spreading retries and is tolerant of read failure.
func randomJitter(jitter time.Duration) (time.Duration, bool) {
	if jitter <= 0 {
		return 0, false
	}
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return 0, false
	}
	n := binary.LittleEndian.Uint64(buf[:])
	return time.Duration(n % uint64(jitter)), true
}

// isRateLimit detects rate-limit shaped errors.
func isRateLimit(err error) bool {
	return errContains(err, "rate limit", "too many requests", "429", "throttl")
}

// isTimeout detects timeout / deadline shaped errors.
func isTimeout(err error) bool {
	return errContains(err, "timeout", "deadline exceeded", "context deadline")
}

// isTransient detects common transient failure text.
func isTransient(err error) bool {
	return errContains(err, "transient", "temporary", "connection reset", "connection refused")
}

// errContains reports whether any message in the error chain contains any of
// the given substrings (case-insensitive).
func errContains(err error, subs ...string) bool {
	if err == nil {
		return false
	}
	var chain []string
	for e := err; e != nil; {
		chain = append(chain, e.Error())
		e = errors.Unwrap(e)
	}
	msg := strings.ToLower(strings.Join(chain, " "))
	for _, sub := range subs {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}
