// Package llm middleware_retry.go - RetryModelMiddleware wraps a BaseChatModel
// with retry logic driven by a RetryPolicy.
//
// The RetryPolicy interface is declared locally in this package to avoid an
// import cycle (production depends on core, which depends on llm). It is
// structurally compatible with production.RetryPolicy, so any production policy
// implementation can be passed directly via WithRetryPolicy.
package llm

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"log/slog"
	"strings"
	"time"
)

// RetryPolicy decides whether and how long to back off between retry attempts.
// It is structurally compatible with production.RetryPolicy so that any
// production policy implementation can be used directly.
type RetryPolicy interface {
	// ShouldRetry reports whether attempt (0-based) should be retried after err.
	ShouldRetry(ctx context.Context, err error, attempt int) bool
	// NextBackoff returns the delay before the next attempt.
	NextBackoff(ctx context.Context, attempt int) time.Duration
	// Name returns the policy identifier.
	Name() string
}

// RetryConfig tunes the default exponential-backoff retry policy.
type RetryConfig struct {
	// MaxAttempts is the total number of attempts allowed.
	MaxAttempts int
	// BaseDelay is the starting backoff for the first retry.
	BaseDelay time.Duration
	// MaxDelay caps the computed backoff.
	MaxDelay time.Duration
	// Jitter adds a random delay in [0, Jitter) to each backoff.
	Jitter time.Duration
}

// defaultRetryPolicy implements a jittered exponential-backoff policy.
type defaultRetryPolicy struct {
	cfg  RetryConfig
	name string
}

// Compile-time assertion that defaultRetryPolicy satisfies RetryPolicy.
var _ RetryPolicy = (*defaultRetryPolicy)(nil)

// newDefaultRetryPolicy returns a defaultRetryPolicy backed by cfg with sensible
// defaults for zero-valued fields.
func newDefaultRetryPolicy(cfg RetryConfig) *defaultRetryPolicy {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = 100 * time.Millisecond
	}
	return &defaultRetryPolicy{cfg: cfg, name: "default-retry"}
}

// errorCategory classifies an error's retryability within the llm package.
type errorCategory int

const (
	categoryNone errorCategory = iota
	categoryFatal
	categoryRateLimit
	categoryTimeout
	categoryTransient
)

func (c errorCategory) retryable() bool {
	return c != categoryNone && c != categoryFatal
}

// classifyError determines if an error is retryable. It first attempts a
// structured ProviderError type assertion (errors.As traverses the Unwrap
// chain). For non-ProviderError errors (e.g. net.Error timeout) it falls back
// to keyword matching for backward compatibility. Unknown errors default to
// categoryTransient (retryable).
// When the production RetryPolicy is wired in via WithRetryPolicy, its
// Classify method is used instead.
func classifyError(err error) errorCategory {
	if err == nil {
		return categoryNone
	}
	// Prefer structured ProviderError (errors.As traverses the Unwrap chain).
	var pe *ProviderError
	if errors.As(err, &pe) {
		switch pe.ErrorType {
		case ErrTypeRateLimit:
			return categoryRateLimit
		case ErrTypeAuth, ErrTypeOverflow:
			return categoryFatal
		case ErrTypeServer, ErrTypeNetwork:
			return categoryTransient
		}
	}
	// Fallback: keyword matching for non-ProviderError errors (e.g. net.Error
	// timeout, connection refused) to preserve backward compatibility.
	msg := strings.ToLower(err.Error())
	// Check the full error chain for more robust matching.
	for e := errors.Unwrap(err); e != nil; e = errors.Unwrap(e) {
		msg += " " + strings.ToLower(e.Error())
	}
	if strings.Contains(msg, "busy") || strings.Contains(msg, "already running") {
		return categoryFatal
	}
	if strings.Contains(msg, "hook") && (strings.Contains(msg, "reject") || strings.Contains(msg, "halt")) {
		return categoryFatal
	}
	if strings.Contains(msg, "permission denied") || strings.Contains(msg, "unauthorized") || strings.Contains(msg, "invalid api key") {
		return categoryFatal
	}
	if strings.Contains(msg, "rate limit") || strings.Contains(msg, "429") || strings.Contains(msg, "too many requests") {
		return categoryRateLimit
	}
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") {
		return categoryTimeout
	}
	if strings.Contains(msg, "connection reset") || strings.Contains(msg, "connection refused") || strings.Contains(msg, "transient") {
		return categoryTransient
	}
	return categoryTransient // default: retry unknown errors
}

// ShouldRetry reports whether attempt may be retried: true when the attempt
// count is below MaxAttempts and the error is classified as retryable.
func (p *defaultRetryPolicy) ShouldRetry(_ context.Context, err error, attempt int) bool {
	if err == nil {
		return false
	}
	if attempt >= p.cfg.MaxAttempts {
		return false
	}
	return classifyError(err).retryable()
}

// NextBackoff computes min(baseDelay*2^attempt + rand[0,jitter), MaxDelay).
func (p *defaultRetryPolicy) NextBackoff(_ context.Context, attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	d := p.cfg.BaseDelay
	for i := 0; i < attempt; i++ {
		d *= 2
		if p.cfg.MaxDelay > 0 && d >= p.cfg.MaxDelay {
			d = p.cfg.MaxDelay
			break
		}
	}
	if p.cfg.Jitter > 0 {
		if j, ok := retryRandomJitter(p.cfg.Jitter); ok {
			d += j
		}
	}
	if p.cfg.MaxDelay > 0 && d > p.cfg.MaxDelay {
		d = p.cfg.MaxDelay
	}
	return d
}

// Name returns the policy identifier.
func (p *defaultRetryPolicy) Name() string { return p.name }

// retryRandomJitter returns a crypto-derived random duration in [0, jitter).
func retryRandomJitter(jitter time.Duration) (time.Duration, bool) {
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

// RetryModelMiddleware retries transient failures using a RetryPolicy. It wraps
// a BaseChatModel so that Generate and Stream calls are retried according to the
// policy's ShouldRetry / NextBackoff decisions.
type RetryModelMiddleware struct {
	policy RetryPolicy
	logger *slog.Logger
}

// Compile-time assertion that RetryModelMiddleware satisfies ModelMiddleware.
var _ ModelMiddleware = (*RetryModelMiddleware)(nil)

// RetryOption configures RetryModelMiddleware at construction time.
type RetryOption func(*RetryModelMiddleware)

// WithRetryPolicy overrides the default retry policy.
func WithRetryPolicy(policy RetryPolicy) RetryOption {
	return func(m *RetryModelMiddleware) { m.policy = policy }
}

// WithRetryLogger overrides the default logger.
func WithRetryLogger(logger *slog.Logger) RetryOption {
	return func(m *RetryModelMiddleware) { m.logger = logger }
}

// NewRetryModelMiddleware creates a RetryModelMiddleware with sensible defaults:
// 3 attempts, 100ms base delay, exponential backoff with jitter.
func NewRetryModelMiddleware(opts ...RetryOption) *RetryModelMiddleware {
	m := &RetryModelMiddleware{
		policy: newDefaultRetryPolicy(RetryConfig{
			MaxAttempts: 3,
			BaseDelay:   100 * time.Millisecond,
			Jitter:      50 * time.Millisecond,
		}),
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Name returns "retry".
func (m *RetryModelMiddleware) Name() string { return "retry" }

// WrapModel returns a BaseChatModel that retries Generate and Stream calls on
// failure according to the configured policy.
func (m *RetryModelMiddleware) WrapModel(next BaseChatModel) BaseChatModel {
	return &retryModel{
		next:   next,
		policy: m.policy,
		logger: m.logger,
	}
}

// retryModel is the wrapped BaseChatModel produced by RetryModelMiddleware.
type retryModel struct {
	next   BaseChatModel
	policy RetryPolicy
	logger *slog.Logger
}

// Compile-time assertion that retryModel satisfies BaseChatModel.
var _ BaseChatModel = (*retryModel)(nil)

// Generate retries the underlying call on failure. The attempt counter is
// 0-based: attempt 0 is the initial call. After each failure the policy decides
// whether to retry and how long to back off.
func (r *retryModel) Generate(ctx context.Context, msgs []Message, opts ...Option) (*Message, error) {
	for attempt := 0; ; attempt++ {
		msg, err := r.next.Generate(ctx, msgs, opts...)
		if err == nil {
			if attempt > 0 {
				r.logger.Debug("middleware.retry",
					"status", "recovered",
					"attempt", attempt,
				)
			}
			return msg, nil
		}
		if !r.policy.ShouldRetry(ctx, err, attempt) {
			r.logger.Debug("middleware.retry",
				"status", "giveup",
				"attempt", attempt,
				"err", err,
			)
			return nil, err
		}
		backoff := r.policy.NextBackoff(ctx, attempt)
		r.logger.Debug("middleware.retry",
			"status", "retry",
			"attempt", attempt,
			"backoff", backoff.String(),
			"err", err,
		)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
}

// Stream retries the underlying call only when the initial Stream() call returns
// an error. Mid-stream errors (chunks that stop arriving after the channel is
// successfully returned) are not retried.
func (r *retryModel) Stream(ctx context.Context, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	for attempt := 0; ; attempt++ {
		ch, err := r.next.Stream(ctx, msgs, opts...)
		if err == nil {
			if attempt > 0 {
				r.logger.Debug("middleware.retry",
					"status", "recovered_stream",
					"attempt", attempt,
				)
			}
			return ch, nil
		}
		if !r.policy.ShouldRetry(ctx, err, attempt) {
			r.logger.Debug("middleware.retry",
				"status", "giveup_stream",
				"attempt", attempt,
				"err", err,
			)
			return nil, err
		}
		backoff := r.policy.NextBackoff(ctx, attempt)
		r.logger.Debug("middleware.retry",
			"status", "retry_stream",
			"attempt", attempt,
			"backoff", backoff.String(),
			"err", err,
		)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
}
