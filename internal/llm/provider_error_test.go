package llm

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProviderError_Error verifies the Error() method format.
func TestProviderError_Error(t *testing.T) {
	e := &ProviderError{
		StatusCode: 429,
		ErrorType:  ErrTypeRateLimit,
		Provider:   "openai",
		Message:    "too many requests",
	}
	assert.Equal(t, "rate_limit error from openai (HTTP 429): too many requests", e.Error())
}

// TestProviderError_Unwrap verifies Unwrap returns nil (leaf node).
func TestProviderError_Unwrap(t *testing.T) {
	e := &ProviderError{StatusCode: 500, ErrorType: ErrTypeServer, Provider: "x", Message: "boom"}
	assert.Nil(t, e.Unwrap())
}

// TestProviderError_ErrorsAs verifies errors.As finds a ProviderError both
// directly and when wrapped by fmt.Errorf("%w", ...).
func TestProviderError_ErrorsAs(t *testing.T) {
	pe := &ProviderError{StatusCode: 401, ErrorType: ErrTypeAuth, Provider: "claude", Message: "bad key"}

	// Direct.
	var target *ProviderError
	require.True(t, errors.As(pe, &target))
	assert.Equal(t, ErrTypeAuth, target.ErrorType)
	assert.Equal(t, "claude", target.Provider)

	// Wrapped.
	wrapped := fmt.Errorf("wrapped: %w", pe)
	var target2 *ProviderError
	require.True(t, errors.As(wrapped, &target2))
	assert.Equal(t, 401, target2.StatusCode)
}

// TestNewProviderError_StatusMapping verifies newProviderError maps HTTP status
// codes to the correct ErrorType.
func TestNewProviderError_StatusMapping(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		payload    string
		wantType   ErrorType
	}{
		{"429 rate limit", 429, "slow down", ErrTypeRateLimit},
		{"401 auth", 401, "unauthorized", ErrTypeAuth},
		{"403 forbidden", 403, "forbidden", ErrTypeAuth},
		{"400 overflow", 400, "context_length_exceeded: too long", ErrTypeOverflow},
		{"400 overflow variant", 400, "maximum context length exceeded", ErrTypeOverflow},
		{"400 non-overflow defaults to server", 400, "bad request", ErrTypeServer},
		{"500 server", 500, "internal error", ErrTypeServer},
		{"502 server", 502, "bad gateway", ErrTypeServer},
		{"503 server", 503, "service unavailable", ErrTypeServer},
		{"404 defaults to server", 404, "not found", ErrTypeServer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pe := newProviderError(tc.statusCode, "testprov", tc.payload)
			assert.Equal(t, tc.statusCode, pe.StatusCode)
			assert.Equal(t, tc.wantType, pe.ErrorType)
			assert.Equal(t, "testprov", pe.Provider)
			// Message should be trimmed.
			assert.Equal(t, tc.payload, pe.Message)
		})
	}
}

// TestClassifyError verifies the classifyError function correctly classifies
// ProviderError instances via errors.As, falls back to keyword matching for
// non-ProviderError errors, and defaults unknown errors to categoryTransient.
func TestClassifyError(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantCat   errorCategory
		wantType  bool // when true, also assert the category is retryable or not
		retryable bool
	}{
		// nil error.
		{"nil", nil, categoryNone, false, false},

		// ProviderError via errors.As (structured path).
		{
			"rate limit",
			&ProviderError{StatusCode: 429, ErrorType: ErrTypeRateLimit, Provider: "p", Message: "slow down"},
			categoryRateLimit, true, true,
		},
		{
			"auth fatal",
			&ProviderError{StatusCode: 401, ErrorType: ErrTypeAuth, Provider: "p", Message: "bad key"},
			categoryFatal, true, false,
		},
		{
			"forbidden fatal",
			&ProviderError{StatusCode: 403, ErrorType: ErrTypeAuth, Provider: "p", Message: "denied"},
			categoryFatal, true, false,
		},
		{
			"overflow fatal",
			&ProviderError{StatusCode: 400, ErrorType: ErrTypeOverflow, Provider: "p", Message: "too long"},
			categoryFatal, true, false,
		},
		{
			"server transient",
			&ProviderError{StatusCode: 500, ErrorType: ErrTypeServer, Provider: "p", Message: "boom"},
			categoryTransient, true, true,
		},
		{
			"network transient",
			&ProviderError{StatusCode: 0, ErrorType: ErrTypeNetwork, Provider: "p", Message: "conn reset"},
			categoryTransient, true, true,
		},

		// Wrapped ProviderError — errors.As must traverse the Unwrap chain.
		{
			"wrapped rate limit",
			fmt.Errorf("call failed: %w", &ProviderError{StatusCode: 429, ErrorType: ErrTypeRateLimit, Provider: "p", Message: "429"}),
			categoryRateLimit, true, true,
		},
		{
			"wrapped auth fatal",
			fmt.Errorf("ctx: %w", &ProviderError{StatusCode: 403, ErrorType: ErrTypeAuth, Provider: "p", Message: "forbidden"}),
			categoryFatal, true, false,
		},

		// Fallback keyword matching for non-ProviderError errors (backward compat).
		{"keyword permission", errors.New("permission denied"), categoryFatal, true, false},
		{"keyword unauthorized", errors.New("401 unauthorized"), categoryFatal, true, false},
		{"keyword 429", errors.New("429 too many requests"), categoryRateLimit, true, true},
		{"keyword rate limit", errors.New("rate limit exceeded"), categoryRateLimit, true, true},
		{"keyword timeout", errors.New("request timeout"), categoryTimeout, true, true},
		{"keyword deadline", errors.New("context deadline exceeded"), categoryTimeout, true, true},
		{"keyword connection reset", errors.New("connection reset by peer"), categoryTransient, true, true},
		{"keyword connection refused", errors.New("connection refused"), categoryTransient, true, true},
		{"keyword transient", errors.New("transient failure"), categoryTransient, true, true},
		{"keyword busy", errors.New("harness is busy"), categoryFatal, true, false},

		// Default: unknown errors are now categoryTransient (retryable).
		{"unknown default transient", errors.New("something unexpected"), categoryTransient, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyError(tc.err)
			assert.Equal(t, tc.wantCat, got)
			if tc.wantType {
				assert.Equal(t, tc.retryable, got.retryable())
			}
		})
	}
}

// TestOverflowErrorChain verifies that isOverflowError uses errors.As to
// traverse the Unwrap chain, detecting a ProviderError with ErrTypeOverflow
// even when wrapped multiple times. It also verifies the keyword fallback
// for non-ProviderError errors and that non-overflow ProviderErrors are
// correctly rejected.
func TestOverflowErrorChain(t *testing.T) {
	overflowPE := &ProviderError{
		StatusCode: 400,
		ErrorType:  ErrTypeOverflow,
		Provider:   "openai",
		Message:    "context_length_exceeded",
	}
	serverPE := &ProviderError{
		StatusCode: 500,
		ErrorType:  ErrTypeServer,
		Provider:   "openai",
		Message:    "internal error",
	}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		// Direct ProviderError with ErrTypeOverflow.
		{"direct overflow", overflowPE, true},
		// Single-wrapped ProviderError — errors.As traverses one level.
		{"wrapped overflow", fmt.Errorf("call: %w", overflowPE), true},
		// Double-wrapped ProviderError — errors.As traverses two levels.
		{"double-wrapped overflow", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", overflowPE)), true},
		// ProviderError with a non-overflow type must NOT be flagged.
		{"direct server error", serverPE, false},
		{"wrapped server error", fmt.Errorf("call: %w", serverPE), false},
		// Fallback: plain error with overflow keyword (backward compat).
		{"keyword context_length_exceeded", errors.New("context_length_exceeded: too many tokens"), true},
		{"keyword maximum context length", errors.New("maximum context length exceeded"), true},
		{"keyword token limit", errors.New("token limit exceeded"), true},
		// Plain error without overflow keyword.
		{"plain non-overflow", errors.New("internal server error"), false},
		// nil.
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isOverflowError(tc.err))
		})
	}
}
