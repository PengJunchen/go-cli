//exempt:scan003
package production

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRedactAPIKey verifies that API-key-like patterns are masked with
// [REDACTED] while the surrounding text is preserved.
func TestRedactAPIKey(t *testing.T) {
	t.Parallel()

	g := NewRedactingOutputGuard()
	g.AddRedactPattern(`sk-[A-Za-z0-9]{20,}`)

	text := "The API key sk-abc123def456ghi789jkl012mno345 was leaked."
	result, err := g.Check(context.Background(), text)
	require.NoError(t, err)

	assert.True(t, result.Allowed, "redacting guard must allow the output")
	assert.NotContains(t, result.Sanitized, "sk-abc123def456ghi789jkl012mno345")
	assert.Contains(t, result.Sanitized, "[REDACTED]")
	assert.Contains(t, result.Sanitized, "was leaked")
}

// TestRedactPassword verifies that password patterns in connection strings are
// masked.
func TestRedactPassword(t *testing.T) {
	t.Parallel()

	g := NewRedactingOutputGuard()
	g.AddRedactPattern(`password=\S+`)

	text := "Connecting with password=s3cr3t to the database at db.example.com"
	result, err := g.Check(context.Background(), text)
	require.NoError(t, err)

	assert.True(t, result.Allowed)
	assert.NotContains(t, result.Sanitized, "s3cr3t")
	assert.Contains(t, result.Sanitized, "[REDACTED]")
	// The non-sensitive part is preserved.
	assert.Contains(t, result.Sanitized, "db.example.com")
}

// TestRedactNonSensitivePreserved verifies that text without any sensitive
// patterns passes through unchanged.
func TestRedactNonSensitivePreserved(t *testing.T) {
	t.Parallel()

	g := NewRedactingOutputGuard()
	g.AddRedactPattern(`sk-[A-Za-z0-9]{20,}`)
	g.AddRedactPattern(`password=\S+`)

	text := "This is a harmless message with no secrets."
	result, err := g.Check(context.Background(), text)
	require.NoError(t, err)

	assert.True(t, result.Allowed)
	assert.Equal(t, text, result.Sanitized, "non-sensitive text must be unchanged")
	assert.Empty(t, result.Reason, "no redaction reason when nothing was masked")
}

// TestRedactMultiplePatterns verifies that multiple patterns are all applied.
func TestRedactMultiplePatterns(t *testing.T) {
	t.Parallel()

	g := NewRedactingOutputGuard()
	g.AddRedactPattern(`sk-[A-Za-z0-9]+`)
	g.AddRedactPattern(`Bearer\s+[A-Za-z0-9._-]+`)

	text := "Keys: sk-abc123 and Bearer eyJhbGciOiJIUzI1NiJ9.token"
	result, err := g.Check(context.Background(), text)
	require.NoError(t, err)

	assert.True(t, result.Allowed)
	assert.NotContains(t, result.Sanitized, "sk-abc123")
	assert.NotContains(t, result.Sanitized, "Bearer eyJhbGciOiJIUzI1NiJ9.token")
	assert.Contains(t, result.Sanitized, "[REDACTED]")
}

// TestRedactingOutputGuardName verifies the default and override name.
func TestRedactingOutputGuardName(t *testing.T) {
	t.Parallel()

	g := NewRedactingOutputGuard()
	assert.Equal(t, "redacting-output-guard", g.Name())

	g2 := NewRedactingOutputGuard(WithName("custom-redact"))
	assert.Equal(t, "custom-redact", g2.Name())
}

// TestRedactEmptyPatternIgnored verifies that an empty pattern is silently
// ignored rather than corrupting the output by matching at every position.
func TestRedactEmptyPatternIgnored(t *testing.T) {
	t.Parallel()

	g := NewRedactingOutputGuard()
	g.AddRedactPattern("")
	g.AddRedactPattern(`sk-[A-Za-z0-9]+`)

	text := "Keys: sk-abc123 and plain text"
	result, err := g.Check(context.Background(), text)
	require.NoError(t, err)

	assert.True(t, result.Allowed)
	assert.NotContains(t, result.Sanitized, "sk-abc123")
	assert.Contains(t, result.Sanitized, "[REDACTED]")
	// The plain text must be preserved character-for-character, not
	// punctuated with [REDACTED] between every character.
	assert.Contains(t, result.Sanitized, "plain text")
}

// TestRedactInvalidPatternIgnored verifies that an invalid regex pattern is
// silently ignored without affecting other registered patterns.
func TestRedactInvalidPatternIgnored(t *testing.T) {
	t.Parallel()

	g := NewRedactingOutputGuard()
	g.AddRedactPattern(`[invalid(`) // unbalanced bracket
	g.AddRedactPattern(`sk-[A-Za-z0-9]+`)

	text := "key sk-abc123 leaked"
	result, err := g.Check(context.Background(), text)
	require.NoError(t, err)

	assert.True(t, result.Allowed)
	assert.NotContains(t, result.Sanitized, "sk-abc123")
	assert.Contains(t, result.Sanitized, "[REDACTED]")
}
