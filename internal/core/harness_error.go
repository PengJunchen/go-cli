package core

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
)

// AgentHarnessError is a normalized error from the agent harness. It carries a
// stable code so callers can switch on error category without parsing error
// messages.
type AgentHarnessError struct {
	// Code is the stable error category (e.g. "timeout", "busy").
	Code string
	// Message is a human-readable description of the error.
	Message string
	// Cause is the underlying error, if any.
	Cause error
}

// Compile-time assertion that AgentHarnessError satisfies the error interface.
var _ error = (*AgentHarnessError)(nil)

// Error codes returned by NormalizeError.
const (
	// ErrCodeBusy indicates the harness is already running a submission.
	ErrCodeBusy = "busy"
	// ErrCodeHookRejected indicates a hook halted the run.
	ErrCodeHookRejected = "hook_rejected"
	// ErrCodeTimeout indicates the run exceeded its time budget.
	ErrCodeTimeout = "timeout"
	// ErrCodeModelError indicates the model failed to generate a response.
	ErrCodeModelError = "model_error"
	// ErrCodeToolError indicates a tool execution failure.
	ErrCodeToolError = "tool_error"
)

// Error returns a human-readable description of the harness error.
func (e *AgentHarnessError) Error() string {
	if e.Cause != nil {
		return e.Code + ": " + e.Message + ": " + e.Cause.Error()
	}
	return e.Code + ": " + e.Message
}

// Unwrap returns the underlying cause so errors.Is and errors.As traverse it.
func (e *AgentHarnessError) Unwrap() error { return e.Cause }

// NormalizeError converts a raw error into an AgentHarnessError with an
// appropriate code. It inspects the error chain and message text to classify
// it. A nil error returns nil.
func NormalizeError(err error) *AgentHarnessError {
	if err == nil {
		return nil
	}

	// Already normalized - return as-is.
	var ahe *AgentHarnessError
	if errors.As(err, &ahe) {
		return ahe
	}

	code, msg := classifyError(err)

	slog.Debug("core.harness_error.normalize",
		"code", code,
		"message", msg,
		"cause", err.Error(),
	)

	return &AgentHarnessError{
		Code:    code,
		Message: msg,
		Cause:   err,
	}
}

// classifyError inspects an error and returns its code and a descriptive
// message.
func classifyError(err error) (code, msg string) {
	// Context deadline / timeout.
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrCodeTimeout, "operation timed out"
	}

	// Network-level timeout.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ErrCodeTimeout, "network operation timed out"
	}

	lower := strings.ToLower(err.Error())

	// Hook rejection - a hook halted the run. The hook chain surfaces these
	// via the interrupt result or the BeforeRun error.
	if strings.Contains(lower, "hook") && (strings.Contains(lower, "reject") ||
		strings.Contains(lower, "interrupt") || strings.Contains(lower, "halt")) {
		return ErrCodeHookRejected, "a hook rejected the run"
	}

	// Busy / already running - the run slot is held by another submission.
	if strings.Contains(lower, "busy") || strings.Contains(lower, "already running") {
		return ErrCodeBusy, "harness is busy"
	}

	// Tool execution errors.
	if strings.Contains(lower, "tool") && (strings.Contains(lower, "error") ||
		strings.Contains(lower, "fail") || strings.Contains(lower, "execute")) {
		return ErrCodeToolError, "tool execution failed"
	}

	// Context cancellation without deadline is treated as a model error
	// surface (the caller cancelled, often while waiting on the model).
	if errors.Is(err, context.Canceled) {
		return ErrCodeModelError, "operation cancelled"
	}

	// Default: treat as a model generation error.
	return ErrCodeModelError, "model generation failed"
}
