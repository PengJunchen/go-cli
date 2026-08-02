package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoad_ValidationErrorPath verifies that when merged configuration fails
// validation, the top-level Load helper surfaces a wrapped error.
func TestLoad_ValidationErrorPath(t *testing.T) {
	t.Setenv("GO_CLI_PROVIDER_TEMPERATURE", "50")
	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validation")
}
