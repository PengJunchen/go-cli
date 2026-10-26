package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/memory"
)

func TestConfiguredMemoryStoreUsesOnlySelectedWorkspaceFile(t *testing.T) {
	cfg := newAssembleTestConfig(t)
	cfg.Memory.StorePath = filepath.Join(t.TempDir(), "nested", "memory.jsonl")
	assembly, err := AssembleAgent(context.Background(), cfg, "openai", "test-model", io.Discard)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	require.NotNil(t, assembly.MemoryStore)
	require.NoError(t, assembly.MemoryStore.Add(context.Background(), memory.Memory{Content: "owned workspace memory", Source: "manual"}))
	require.NoError(t, assembly.Close(context.Background()))
	data, err := os.ReadFile(cfg.Memory.StorePath)
	require.NoError(t, err)
	require.Contains(t, string(data), "owned workspace memory")
}

func TestMemoryStorePathDefaultCompatibilityWithoutOpeningStore(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, ".go-cli", "memories.jsonl"), memoryStorePath(&config.Config{}))
	explicit := filepath.Join(t.TempDir(), "owned.jsonl")
	require.Equal(t, explicit, memoryStorePath(&config.Config{Memory: config.MemoryConfig{StorePath: explicit}}))
}

func TestConfiguredMemoryStoreOpenFailureDoesNotFallBack(t *testing.T) {
	cfg := newAssembleTestConfig(t)
	cfg.Memory.StorePath = t.TempDir() // A directory cannot become a JSONL file.
	assembly, err := AssembleAgent(context.Background(), cfg, "openai", "test-model", io.Discard)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	require.Nil(t, assembly.MemoryStore)
}

func TestMemoryStorePathChangesRequireRestart(t *testing.T) {
	before := &config.Config{Memory: config.MemoryConfig{StorePath: "first.jsonl"}}
	after := *before
	after.Memory.StorePath = "second.jsonl"
	require.True(t, coldConfigurationChanged(before, &after))
}
