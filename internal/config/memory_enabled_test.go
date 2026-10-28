package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMemoryAutomaticDefaultAndExplicitDisableMerge(t *testing.T) {
	for _, testCase := range []struct {
		data string
		want bool
	}{{`{}`, true}, {`{"memory":{"enabled":false}}`, false}, {`{"memory":{"enabled":true}}`, true}} {
		cfg, err := NewLoader().WithEnvPrefix("CATALOG_MEMORY_FIXTURE").WithFileBytes("memory.json", []byte(testCase.data)).Load(t.Context())
		require.NoError(t, err)
		require.Equal(t, testCase.want, cfg.Memory.AutomaticEnabled())
	}
	t.Setenv("MEMORY_FIXTURE_MEMORY_ENABLED", "false")
	cfg, err := NewLoader().WithEnvPrefix("MEMORY_FIXTURE").Load(t.Context())
	require.NoError(t, err)
	require.False(t, cfg.Memory.AutomaticEnabled())
}
