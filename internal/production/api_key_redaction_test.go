//exempt:scan003
package production

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDefaultAPIKeyPatterns verifies that all expected API key patterns are
// returned by DefaultAPIKeyPatterns.
func TestDefaultAPIKeyPatterns(t *testing.T) {
	t.Parallel()

	patterns := DefaultAPIKeyPatterns()

	expected := []string{
		`sk-ant-[a-zA-Z0-9_-]{20,}`,
		`sk-proj-[a-zA-Z0-9_-]{20,}`,
		`sk-[a-zA-Z0-9]{20,}`,
		`AIza[a-zA-Z0-9_-]{35}`,
		`Bearer\s+[a-zA-Z0-9_.-]{20,}`,
		`AKIA[0-9A-Z]{16}`,
		`(?i)aws_secret_access_key\s*[=:]\s*[A-Za-z0-9/+=]{40}`,
		`gh[pousr]_[a-zA-Z0-9]{36,}`,
		`glpat-[a-zA-Z0-9_-]{20,}`,
		`xox[baprs]-[a-zA-Z0-9-]{10,}`,
	}

	assert.Len(t, patterns, len(expected), "pattern count mismatch")
	for i, want := range expected {
		assert.Contains(t, patterns, want, "missing expected pattern #%d: %s", i, want)
	}
}

// TestRegisterAPIKeyRedaction verifies that after registration the guard
// redacts known API key formats while leaving normal text unchanged.
func TestRegisterAPIKeyRedaction(t *testing.T) {
	t.Parallel()

	g := NewRedactingOutputGuard()
	RegisterAPIKeyRedaction(g)

	tests := []struct {
		name      string
		input     string
		redacted  bool
		unchanged bool
	}{
		{
			name:     "openai key",
			input:    "sk-abc123def456ghi789jkl012mno345pqr678",
			redacted: true,
		},
		{
			name:     "anthropic key",
			input:    "sk-ant-api03-xxxxxxxxxxxxxxxxxxxxxxxx",
			redacted: true,
		},
		{
			name:     "google gemini key",
			input:    "AIzaSyA1234567890abcdefghijklmnopqrstuvwxyz1234",
			redacted: true,
		},
		{
			name:     "bearer token",
			input:    "Bearer abc123def456ghi789jkl012",
			redacted: true,
		},
		{
			name:     "aws access key",
			input:    "AKIAIOSFODNN7EXAMPLE",
			redacted: true,
		},
		{
			name:     "aws secret access key",
			input:    "aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			redacted: true,
		},
		{
			name:     "github pat",
			input:    "ghp_1234567890abcdefghijklmnopqrstuvwxyz1234",
			redacted: true,
		},
		{
			name:     "gitlab pat",
			input:    "glpat-1234567890abcdefghij",
			redacted: true,
		},
		{
			name:     "slack token",
			input:    "xoxb-1234567890-abcdefghij",
			redacted: true,
		},
		{
			name:      "ghp substring not redacted",
			input:     "the word ghp appears in this text",
			unchanged: true,
		},
		{
			name:      "normal text",
			input:     "hello world",
			unchanged: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result, err := g.Check(context.Background(), tc.input)
			require.NoError(t, err)
			assert.True(t, result.Allowed, "redacting guard must always allow")

			if tc.unchanged {
				assert.Equal(t, tc.input, result.Sanitized,
					"normal text must be unchanged")
				assert.NotContains(t, result.Sanitized, "[REDACTED]")
			}
			if tc.redacted {
				assert.Contains(t, result.Sanitized, "[REDACTED]",
					"expected [REDACTED] in output for %s", tc.name)
				assert.NotEqual(t, tc.input, result.Sanitized,
					"input must have been modified by redaction")
			}
		})
	}
}

// TestRegisterAPIKeyRedactionNilGuard verifies that calling
// RegisterAPIKeyRedaction with a nil guard is a safe no-op.
func TestRegisterAPIKeyRedactionNilGuard(t *testing.T) {
	t.Parallel()

	assert.NotPanics(t, func() {
		RegisterAPIKeyRedaction(nil)
	})
}
