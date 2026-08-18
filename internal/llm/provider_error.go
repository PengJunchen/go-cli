// Package llm provider_error.go - ProviderError is a structured error type
// returned by native model providers. It lets middleware (retry, overflow,
// failover) classify errors via type assertions instead of fragile string
// matching.
package llm

import (
	"fmt"
	"strings"
)

// ErrorType classifies the kind of error returned by a model provider.
type ErrorType string

const (
	// ErrTypeRateLimit indicates a 429 Too Many Requests response.
	ErrTypeRateLimit ErrorType = "rate_limit"
	// ErrTypeAuth indicates a 401/403 authentication or authorization failure.
	ErrTypeAuth ErrorType = "auth"
	// ErrTypeOverflow indicates a 400 response whose body contains a
	// context-length-overflow indicator.
	ErrTypeOverflow ErrorType = "overflow"
	// ErrTypeServer indicates a 5xx server error (or any other retryable HTTP
	// error that does not fit the categories above).
	ErrTypeServer ErrorType = "server"
	// ErrTypeNetwork indicates a transport-level failure (dial, DNS, etc.).
	ErrTypeNetwork ErrorType = "network"
)

// ProviderError is a structured error returned by a model provider. It carries
// the HTTP status code, a typed error category, the provider name and the
// original error message so that middleware can classify errors without
// fragile string matching.
type ProviderError struct {
	StatusCode int
	ErrorType  ErrorType
	Provider   string
	Message    string
}

// Error implements the error interface.
func (e *ProviderError) Error() string {
	return fmt.Sprintf("%s error from %s (HTTP %d): %s", e.ErrorType, e.Provider, e.StatusCode, e.Message)
}

// Unwrap returns nil: ProviderError is a leaf node in the error chain.
func (e *ProviderError) Unwrap() error {
	return nil
}

// overflowIndicators are substrings that signal a context-length overflow in a
// provider error response body. They are shared by newProviderError (for HTTP
// 400 responses) and isOverflowError (as a backward-compatible fallback for
// non-ProviderError errors).
var overflowIndicators = []string{
	"context_length_exceeded",
	"maximum context length",
	"context window",
	"token limit exceeded",
	"context_length",
}

// containsOverflowIndicator reports whether msg contains any known overflow
// indicator (case-insensitive).
func containsOverflowIndicator(msg string) bool {
	lower := strings.ToLower(msg)
	for _, ind := range overflowIndicators {
		if strings.Contains(lower, ind) {
			return true
		}
	}
	return false
}

// newProviderError constructs a ProviderError from an HTTP status code, provider
// name and response body. The error type is derived from the status code:
//   - 429 → ErrTypeRateLimit
//   - 401/403 → ErrTypeAuth
//   - 400 with overflow indicators in the body → ErrTypeOverflow
//   - 5xx → ErrTypeServer
//   - other → ErrTypeServer (default, retryable)
func newProviderError(statusCode int, provider, payload string) *ProviderError {
	msg := strings.TrimSpace(payload)
	errType := ErrTypeServer //nolint:ineffassign
	switch {
	case statusCode == 429:
		errType = ErrTypeRateLimit
	case statusCode == 401 || statusCode == 403:
		errType = ErrTypeAuth
	case statusCode == 400 && containsOverflowIndicator(msg):
		errType = ErrTypeOverflow
	case statusCode >= 500:
		errType = ErrTypeServer
	default:
		errType = ErrTypeServer
	}
	return &ProviderError{
		StatusCode: statusCode,
		ErrorType:  errType,
		Provider:   provider,
		Message:    msg,
	}
}
