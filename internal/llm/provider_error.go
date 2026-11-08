// Package llm provider_error.go - ProviderError is a structured error type
// returned by native model providers. It lets middleware (retry, overflow,
// failover) classify errors via type assertions instead of fragile string
// matching.
package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// ErrorType classifies the kind of error returned by a model provider.
type ErrorType string

const (
	// ErrTypeRateLimit indicates an explicit provider rate-limit response,
	// including a recognized rate-limit envelope inside an HTTP 200 SSE stream.
	ErrTypeRateLimit ErrorType = "rate_limit"
	// ErrTypeAuth indicates a 401/403 authentication or authorization failure.
	ErrTypeAuth ErrorType = "auth"
	// ErrTypeOverflow indicates a 400 response whose body contains a
	// context-length-overflow indicator.
	ErrTypeOverflow ErrorType = "overflow"
	// ErrTypeInvalidRequest indicates a recognized non-retryable request error:
	// an ordinary 400 response or a 404 response.
	ErrTypeInvalidRequest ErrorType = "invalid_request"
	// ErrTypeServer indicates a 5xx server error (or any other retryable HTTP
	// error that does not fit the categories above).
	ErrTypeServer ErrorType = "server"
	// ErrTypeNetwork indicates a transport-level failure (dial, DNS, etc.).
	ErrTypeNetwork ErrorType = "network"
	// ErrTypeQuota indicates an exhausted provider credit or quota allocation.
	ErrTypeQuota ErrorType = "quota"
	// ErrTypeProtocol indicates an unrecognized or malformed provider response.
	ErrTypeProtocol ErrorType = "protocol"
)

// openAIErrorEnvelope is the structured error payload embedded in an
// OpenAI-compatible SSE data frame.
type openAIErrorEnvelope struct {
	Message string          `json:"message"`
	Type    string          `json:"type"`
	Code    json.RawMessage `json:"code"`
}

// ProviderError is a structured error returned by a model provider. It carries
// the HTTP status code, a typed error category, the provider name and a bounded
// response message so middleware can classify errors without fragile string
// matching. Error deliberately excludes Message because providers may echo
// prompts or tool input in response bodies.
type ProviderError struct {
	StatusCode int
	ErrorType  ErrorType
	Provider   string
	Message    string

	// BodyTruncated reports that the error response contained bytes beyond the
	// bounded diagnostic prefix stored in Message.
	BodyTruncated bool
}

// Error implements the error interface.
func (e *ProviderError) Error() string {
	return fmt.Sprintf("%s error from %s (HTTP %d)", e.ErrorType, e.Provider, e.StatusCode)
}

// setProviderErrorSpanAttributes retains bounded provider diagnostics without
// copying the response body's free-form error message into telemetry.
func setProviderErrorSpanAttributes(span tracing.TraceSpan, err error) {
	if span == nil || err == nil {
		return
	}
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) || providerErr == nil {
		return
	}
	span.SetAttributes(
		tracing.Attribute{Key: "error_type", Value: string(providerErr.ErrorType)},
		tracing.Attribute{Key: "http_status_code", Value: providerErr.StatusCode},
	)
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
// name and response body. The error type is derived from the status code and
// recognized quota body codes:
//   - 429 → ErrTypeRateLimit, unless the body explicitly identifies exhausted quota
//   - 401/403 → ErrTypeAuth
//   - 400 with overflow indicators in the body → ErrTypeOverflow
//   - ordinary 400 and 404 → ErrTypeInvalidRequest
//   - 5xx → ErrTypeServer
//   - other → ErrTypeServer (default, retryable)
func newProviderError(statusCode int, provider, payload string) *ProviderError {
	msg := strings.TrimSpace(payload)
	errType := ErrTypeServer //nolint:ineffassign
	switch {
	case statusCode == 429:
		if openAIQuotaErrorPayload(msg) {
			errType = ErrTypeQuota
		} else {
			errType = ErrTypeRateLimit
		}
	case statusCode == 401 || statusCode == 403:
		errType = ErrTypeAuth
	case statusCode == 400 && containsOverflowIndicator(msg):
		errType = ErrTypeOverflow
	case statusCode == 400 || statusCode == 404:
		errType = ErrTypeInvalidRequest
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

func openAIQuotaErrorPayload(payload string) bool {
	var response struct {
		Error *openAIErrorEnvelope `json:"error"`
	}
	if json.Unmarshal([]byte(payload), &response) != nil || response.Error == nil {
		return false
	}
	return classifyOpenAIStreamError(*response.Error) == ErrTypeQuota
}

func newOpenAIStreamProviderError(statusCode int, provider, payload string, envelope openAIErrorEnvelope) *ProviderError {
	message := payload
	truncated := false
	if len(message) > maxProviderErrorBodyBytes {
		message = message[:maxProviderErrorBodyBytes]
		truncated = true
	}
	return &ProviderError{
		StatusCode:    statusCode,
		ErrorType:     classifyOpenAIStreamError(envelope),
		Provider:      provider,
		Message:       message,
		BodyTruncated: truncated,
	}
}

func classifyOpenAIStreamError(envelope openAIErrorEnvelope) ErrorType {
	typeName := strings.ToLower(strings.TrimSpace(envelope.Type))
	rawCode := strings.TrimSpace(string(envelope.Code))
	code := ""
	codeIsEmpty := rawCode == "" || rawCode == "null"
	if !codeIsEmpty {
		if err := json.Unmarshal([]byte(rawCode), &code); err != nil {
			return ErrTypeProtocol
		}
		code = strings.ToLower(strings.TrimSpace(code))
		codeIsEmpty = code == ""
	}

	codeType, codeKnown := classifyOpenAIStreamErrorCode(code)
	typeType, typeKnown := classifyOpenAIStreamErrorCode(typeName)
	switch {
	case codeKnown && typeKnown:
		if codeType == typeType {
			return codeType
		}
		// Prefer a specific non-retryable classification over an explicit
		// transient one, so contradictory rate-limit metadata cannot make
		// authentication, quota, or invalid-request errors retryable.
		if isOpenAIStreamTransientErrorType(codeType) && !isOpenAIStreamTransientErrorType(typeType) {
			return typeType
		}
		if isOpenAIStreamTransientErrorType(typeType) && !isOpenAIStreamTransientErrorType(codeType) {
			return codeType
		}
		// invalid_request_error is a broad category; a more specific code or
		// type such as context_length_exceeded remains authoritative. Specific
		// request codes like model_not_found are not treated as generic here.
		if isGenericOpenAIInvalidRequest(code) && typeType != ErrTypeInvalidRequest {
			return typeType
		}
		if isGenericOpenAIInvalidRequest(typeName) && codeType != ErrTypeInvalidRequest {
			return codeType
		}
		return ErrTypeProtocol
	case codeKnown:
		return codeType
	case typeKnown:
		return typeType
	}

	message := strings.ToLower(envelope.Message)
	if typeName == "api_error" && codeIsEmpty && containsAnyProviderErrorMarker(message, "rate limit", "too many requests") {
		return ErrTypeRateLimit
	}
	return ErrTypeProtocol
}

func isOpenAIStreamTransientErrorType(errType ErrorType) bool {
	return errType == ErrTypeRateLimit || errType == ErrTypeServer || errType == ErrTypeNetwork
}

func isGenericOpenAIInvalidRequest(value string) bool {
	return value == "invalid_request_error" || value == "invalid_request"
}

func classifyOpenAIStreamErrorCode(value string) (ErrorType, bool) {
	switch value {
	case "insufficient_quota", "quota_exceeded", "billing_hard_limit_reached", "out_of_credits":
		return ErrTypeQuota, true
	case "invalid_api_key", "authentication_error", "permission_error", "unauthorized":
		return ErrTypeAuth, true
	case "context_length_exceeded", "maximum_context_length_exceeded":
		return ErrTypeOverflow, true
	case "invalid_request_error", "invalid_request", "model_not_found", "unsupported_parameter":
		return ErrTypeInvalidRequest, true
	case "rate_limit_error", "rate_limit_exceeded", "too_many_requests", "rate_limit":
		return ErrTypeRateLimit, true
	case "server_error", "internal_error", "service_unavailable":
		return ErrTypeServer, true
	default:
		return "", false
	}
}

func containsAnyProviderErrorMarker(value string, markers ...string) bool {
	for _, marker := range markers {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}
