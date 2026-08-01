//go:build mock

package mock

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/production"
)

// Compile-time assertions that the mock satisfies the guard contract.
var (
	_ production.OutputGuard = (*MockOutputGuard)(nil)
)

func TestMockOutputGuardAllowedByDefault(t *testing.T) {
	g := NewMockOutputGuard("mock-guard")
	assert.Equal(t, "mock-guard", g.Name())

	res, err := g.Check(context.Background(), "some text")
	require.NoError(t, err)
	require.True(t, res.Allowed)
	assert.Equal(t, "some text", res.Sanitized)
	assert.Equal(t, 1, g.CheckCount())
}

func TestMockOutputGuardDenied(t *testing.T) {
	g := NewMockOutputGuard("mock-guard")
	g.WithDenied("blocked reason").WithSanitized("safe text").WithSeverity(production.GuardCritical)

	res, err := g.Check(context.Background(), "original")
	require.NoError(t, err)
	require.False(t, res.Allowed)
	assert.Equal(t, "blocked reason", res.Reason)
	assert.Equal(t, "safe text", res.Sanitized)
	assert.Equal(t, production.GuardCritical, res.Severity)

	// Re-enable allow.
	g.WithAllowed()
	res, err = g.Check(context.Background(), "x")
	require.NoError(t, err)
	assert.True(t, res.Allowed)
	assert.Equal(t, 2, g.CheckCount())
}
