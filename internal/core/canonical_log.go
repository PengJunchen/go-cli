package core

import "fmt"

// canonicalErrorLogAttrs returns only low-risk metadata for errors that may
// contain caller-controlled text. Keep canonical middleware logs useful for
// diagnosis without recording the error message itself.
func canonicalErrorLogAttrs(err error) []any {
	if err == nil {
		return []any{"error_type", "", "error_len", 0}
	}
	message := err.Error()
	return []any{"error_type", fmt.Sprintf("%T", err), "error_len", len(message)}
}
