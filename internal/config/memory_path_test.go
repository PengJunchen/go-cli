package config

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMemoryStorePathValidationAndFileOverlay(t *testing.T) {
	cfg := defaultConfig()
	cfg.Memory.StorePath = "../outside.jsonl"
	require.ErrorContains(t, NewDefaultValidator().Validate(*cfg), "memory store_path")
	overlay, err := NewLoader().WithFileBytes("project.json", []byte(`{"memory":{"store_path":"workspace/memory.jsonl"}}`)).LoadFileLayer(context.Background())
	require.NoError(t, err)
	merged := mergeConfigs(defaultConfig(), overlay)
	require.Equal(t, "workspace/memory.jsonl", merged.Memory.StorePath)
	require.NoError(t, NewDefaultValidator().Validate(*merged))
}
