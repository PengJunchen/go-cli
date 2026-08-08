package tools

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// otherError is a custom error type that is NOT a *ToolError, used to verify
// that IsToolError returns false for non-ToolError types.
type otherError struct{ msg string }

func (e *otherError) Error() string { return e.msg }

// TestIsToolErrorByType verifies that IsToolError returns true when the error
// is or wraps a *ToolError, using errors.As for type assertion.
func TestIsToolErrorByType(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// Direct *ToolError.
	te := NewToolError("file_not_found", "path: /tmp/missing.txt")
	assert.True(t, IsToolError(te))

	// Wrapped *ToolError.
	wrapped := fmt.Errorf("execute failed: %w", te)
	assert.True(t, IsToolError(wrapped))

	// Double-wrapped *ToolError.
	doubleWrapped := fmt.Errorf("outer: %w", wrapped)
	assert.True(t, IsToolError(doubleWrapped))
}

// TestIsToolErrorRegularErrorFalse verifies that IsToolError returns false for
// errors that are not *ToolError, including standard library errors.
func TestIsToolErrorRegularErrorFalse(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// Standard errors.New — not a ToolError.
	assert.False(t, IsToolError(errors.New("something went wrong")))

	// fmt.Errorf without wrapping a ToolError.
	assert.False(t, IsToolError(fmt.Errorf("timeout after %ds", 30)))

	// Nil is not a tool error.
	assert.False(t, IsToolError(nil))

	// A custom error type that is not *ToolError.
	oe := &otherError{msg: "custom"}
	assert.False(t, IsToolError(oe))
}

// TestToolErrorCodeAccessible verifies that the Code field of a *ToolError can
// be accessed via errors.As, enabling callers to switch on structured codes.
func TestToolErrorCodeAccessible(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	original := NewToolError("permission_denied", "cannot write to /root")
	wrapped := fmt.Errorf("tool execution: %w", original)

	var te *ToolError
	require.True(t, errors.As(wrapped, &te))
	assert.Equal(t, "permission_denied", te.Code)
	assert.Equal(t, "cannot write to /root", te.Message)

	// The Code can be used in a switch for structured error handling.
	switch te.Code {
	case "permission_denied":
		// expected
	default:
		t.Fatalf("unexpected code: %s", te.Code)
	}
}

// TestIsToolErrorNoStringMatching verifies that IsToolError detects tool errors
// purely via type assertion, not by inspecting the error message text. An error
// whose message contains no "error"-like keywords is still detected when it is
// a *ToolError; conversely, a plain error whose message says "error" is not.
func TestIsToolErrorNoStringMatching(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// A ToolError whose message contains no error-like keywords — detected by
	// type, not by string matching.
	te := NewToolError("ENOENT", "file does not exist")
	assert.True(t, IsToolError(te))

	// A plain error whose message contains "error:" — NOT detected, because
	// IsToolError uses type assertion, not string matching.
	plain := errors.New("Error: something failed")
	assert.False(t, IsToolError(plain))

	// A ToolError with an empty message is still detected by type.
	emptyMsg := NewToolError("timeout", "")
	assert.True(t, IsToolError(emptyMsg))
}
