package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_Default(t *testing.T) {
	cfg, err := Load()
	require.NoError(t, err)
	assert.NotNil(t, cfg)
	assert.False(t, cfg.Verbose())
}

func TestLoad_VerboseEnv(t *testing.T) {
	t.Setenv("GO_CLI_VERBOSE", "1")
	cfg, err := Load()
	require.NoError(t, err)
	assert.True(t, cfg.Verbose())
}

func TestConfig_Verbose(t *testing.T) {
	cfg := &Config{verbose: true}
	assert.True(t, cfg.Verbose())

	cfg = &Config{verbose: false}
	assert.False(t, cfg.Verbose())
}
