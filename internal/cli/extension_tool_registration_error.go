package cli

import (
	"errors"
	"fmt"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// ExtensionToolRegistrationError reports a failed extension tool projection
// without exposing extension-controlled labels or the underlying error text.
type ExtensionToolRegistrationError struct {
	Owner string
	Tool  string
	Class string
	cause error
}

func newExtensionToolRegistrationError(owner, tool string, cause error) *ExtensionToolRegistrationError {
	class := "registration_rejected"
	switch {
	case errors.Is(cause, tools.ErrPreparedValueNotJSON):
		class = "non_json_schema"
	case errors.Is(cause, tools.ErrMCPNameReserved):
		class = "name_reserved"
	}
	return &ExtensionToolRegistrationError{
		Owner: safeExtensionRegistrationLabel(owner),
		Tool:  safeExtensionRegistrationLabel(tool),
		Class: class,
		cause: cause,
	}
}

// Error returns safe owner/tool labels and a stable failure class. It never
// prints the raw cause.
func (e *ExtensionToolRegistrationError) Error() string {
	if e == nil {
		return "cli: extension tool registration failed"
	}
	return fmt.Sprintf("cli: extension tool registration failed (owner=%s tool=%s class=%s)",
		safeExtensionRegistrationLabel(e.Owner), safeExtensionRegistrationLabel(e.Tool), safeExtensionRegistrationClass(e.Class))
}

// Unwrap preserves errors.Is and errors.As access to the registration cause.
func (e *ExtensionToolRegistrationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func safeExtensionRegistrationLabel(value string) string {
	if len(value) == 0 || len(value) > 64 {
		return "<redacted>"
	}
	for index, char := range value {
		if index == 0 {
			if !isRegistrationLabelAlphanumeric(char) {
				return "<redacted>"
			}
			continue
		}
		if !isRegistrationLabelAlphanumeric(char) && char != '.' && char != '_' && char != '-' {
			return "<redacted>"
		}
	}
	return value
}

func isRegistrationLabelAlphanumeric(char rune) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9'
}

func safeExtensionRegistrationClass(value string) string {
	switch value {
	case "non_json_schema", "name_reserved", "registration_rejected":
		return value
	default:
		return "registration_rejected"
	}
}
