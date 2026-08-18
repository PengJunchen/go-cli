package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// UserFriendlyError wraps an error with an action description and a recovery
// hint so that end users see actionable messages instead of raw error text.
type UserFriendlyError struct {
	Err    error
	Action string // what the user was trying to do
	Hint   string // recovery suggestion
}

// Error implements the error interface.
func (e *UserFriendlyError) Error() string {
	return fmt.Sprintf("%s: %v\nHint: %s", e.Action, e.Err, e.Hint)
}

// Unwrap returns the underlying error for errors.As / errors.Is use.
func (e *UserFriendlyError) Unwrap() error { return e.Err }

// classifyError examines an error and returns a user-friendly version with a
// recovery hint. It inspects structured errors (e.g. llm.ProviderError) first,
// then falls back to string matching for transport-level and file errors.
// Returns nil for context cancellation errors (expected, not failures) and
// for nil input.
func classifyError(err error) *UserFriendlyError {
	if err == nil {
		return nil
	}

	// Context cancellation is expected behavior, not a failure that needs
	// a recovery hint.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}

	// --- Structured provider errors ---
	var pe *llm.ProviderError
	if errors.As(err, &pe) {
		switch pe.ErrorType {
		case llm.ErrTypeAuth:
			return &UserFriendlyError{
				Err:    err,
				Action: "authentication failed",
				Hint:   "Check your API key (run `go-cli doctor` to diagnose) and ensure the key has not expired or been revoked.",
			}
		case llm.ErrTypeRateLimit:
			return &UserFriendlyError{
				Err:    err,
				Action: "rate limit exceeded",
				Hint:   "Wait a moment and retry, or reduce the request frequency. Consider upgrading your plan for a higher quota.",
			}
		case llm.ErrTypeNetwork:
			return &UserFriendlyError{
				Err:    err,
				Action: "network error",
				Hint:   "Check your internet connection, DNS resolution, and firewall settings. Verify the provider endpoint URL is correct.",
			}
		case llm.ErrTypeOverflow:
			return &UserFriendlyError{
				Err:    err,
				Action: "context length exceeded",
				Hint:   "Reduce the conversation length or use /compact to summarize. Consider switching to a model with a larger context window.",
			}
		}
	}

	// --- Network / transport errors (string-based fallback) ---
	lower := strings.ToLower(err.Error())

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &UserFriendlyError{
			Err:    err,
			Action: "request timed out",
			Hint:   "The server did not respond in time. Check your network connection and retry. If the problem persists, the provider may be experiencing an outage.",
		}
	}

	if strings.Contains(lower, "connection refused") {
		return &UserFriendlyError{
			Err:    err,
			Action: "connection refused",
			Hint:   "The target service is not running or is refusing connections. Verify the address and port, and ensure the service is started.",
		}
	}

	if strings.Contains(lower, "no such host") || strings.Contains(lower, "dns") {
		return &UserFriendlyError{
			Err:    err,
			Action: "DNS resolution failed",
			Hint:   "The hostname could not be resolved. Check your DNS settings and the spelling of the endpoint URL.",
		}
	}

	// --- Config / file errors ---
	if errors.Is(err, os.ErrNotExist) || strings.Contains(lower, "no such file or directory") {
		return &UserFriendlyError{
			Err:    err,
			Action: "configuration file not found",
			Hint:   "Run `go-cli doctor` to diagnose configuration issues, or create the missing file. See the documentation for the expected location.",
		}
	}

	if strings.Contains(lower, "yaml") || strings.Contains(lower, "parse") && strings.Contains(lower, "config") {
		return &UserFriendlyError{
			Err:    err,
			Action: "configuration parse error",
			Hint:   "Run `go-cli doctor` to validate your configuration file. Check for YAML syntax errors such as incorrect indentation or missing colons.",
		}
	}

	// --- Default ---
	return &UserFriendlyError{
		Err:    err,
		Action: "operation failed",
		Hint:   "Try again. If the problem persists, run `go-cli doctor` to diagnose common issues or contact support.",
	}
}
