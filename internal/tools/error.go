package tools

import "errors"

// ToolError is a structured error type returned by tool implementations. It
// carries a machine-readable Code so callers can detect tool errors via type
// assertion (errors.As) instead of fragile string matching on the error
// message.
type ToolError struct {
	// Code is a stable, machine-readable error code (e.g. "file_not_found",
	// "timeout", "permission_denied").
	Code string
	// Message is the human-readable error detail.
	Message string
}

// Error implements the error interface.
func (e *ToolError) Error() string {
	if e.Message != "" {
		return e.Code + ": " + e.Message
	}
	return e.Code
}

// NewToolError constructs a ToolError with the given code and message.
func NewToolError(code, message string) *ToolError {
	return &ToolError{Code: code, Message: message}
}

// IsToolError reports whether err is or wraps a *ToolError, using a structured
// type assertion (errors.As) rather than string matching. This allows callers
// to reliably detect tool errors even when the error has been wrapped by
// middleware or other layers.
func IsToolError(err error) bool {
	var te *ToolError
	return errors.As(err, &te)
}
