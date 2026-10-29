package config

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBashToolExecutionTimeoutPresenceAndValidation(t *testing.T) {
	duration, configured, err := (BashToolConfig{}).ExecutionTimeout()
	require.NoError(t, err)
	require.False(t, configured)
	require.Zero(t, duration)
	for _, value := range []string{"300s", "5m", "0.5s", "25h"} {
		t.Run(value, func(t *testing.T) {
			duration, configured, err := (BashToolConfig{Timeout: &value}).ExecutionTimeout()
			require.NoError(t, err)
			require.True(t, configured)
			expected, err := time.ParseDuration(value)
			require.NoError(t, err)
			require.Equal(t, expected, duration)
		})
	}
	for _, value := range []string{"", " ", "0", "0s", "-1s", "300", "bad", "999999999999999999999s"} {
		t.Run("invalid:"+value, func(t *testing.T) {
			_, _, err := (BashToolConfig{Timeout: &value}).ExecutionTimeout()
			require.ErrorContains(t, err, "tools.bash.timeout")
			cfg := defaultConfig()
			cfg.Tools.Bash.Timeout = &value
			require.ErrorContains(t, NewDefaultValidator().Validate(*cfg), "tools.bash.timeout")
		})
	}
}

func TestBashToolExecutionTimeoutMergedSources(t *testing.T) {
	for _, input := range []struct{ path, data string }{
		{"config.json", `{"tools":{"bash":{"timeout":"300s"}}}`},
		{"config.yaml", "tools:\n  bash:\n    timeout: 5m\n"},
	} {
		t.Run(input.path, func(t *testing.T) {
			cfg, err := NewLoader().WithEnvPrefix("GO_CLI_TEST_BASH_CONFIG").WithFileBytes(input.path, []byte(input.data)).Load(context.Background())
			require.NoError(t, err)
			duration, configured, err := cfg.Tools.Bash.ExecutionTimeout()
			require.NoError(t, err)
			require.True(t, configured)
			require.Equal(t, 5*time.Minute, duration)
		})
	}
	for _, input := range []struct{ path, data string }{
		{"config.json", `{"tools":{"bash":{"timeout":300}}}`},
		{"config.yaml", "tools:\n  bash:\n    timeout: 300\n"},
	} {
		_, err := NewLoader().WithEnvPrefix("GO_CLI_TEST_BASH_CONFIG").WithFileBytes(input.path, []byte(input.data)).Load(context.Background())
		require.Error(t, err, "numeric values cannot silently select a default")
	}
	key := "GO_CLI_TEST_BASH_ENV_TOOLS_BASH_TIMEOUT"
	t.Setenv(key, "5m")
	cfg, err := NewLoader().WithEnvPrefix("GO_CLI_TEST_BASH_ENV").WithFileBytes("config.json", []byte(`{"tools":{"bash":{"timeout":"bad"}}}`)).Load(context.Background())
	require.NoError(t, err, "semantic validation belongs to the final merged value")
	duration, configured, err := cfg.Tools.Bash.ExecutionTimeout()
	require.NoError(t, err)
	require.True(t, configured)
	require.Equal(t, 5*time.Minute, duration)
	t.Setenv(key, "")
	_, err = NewLoader().WithEnvPrefix("GO_CLI_TEST_BASH_ENV").Load(context.Background())
	require.ErrorContains(t, err, "tools.bash.timeout", "present empty environment input must not disappear")
	require.NoError(t, os.Unsetenv(key))
	cfg, err = NewLoader().WithEnvPrefix("GO_CLI_TEST_BASH_ENV").Load(context.Background())
	require.NoError(t, err)
	require.Nil(t, cfg.Tools.Bash.Timeout, "absent environment input leaves the native default untouched")
}
