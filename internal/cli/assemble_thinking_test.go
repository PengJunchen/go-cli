package cli

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// TestWithThinkingLevel_SetsConfig verifies that the WithThinkingLevel option
// stores the level in the assembleConfig.
func TestWithThinkingLevel_SetsConfig(t *testing.T) {
	var ac assembleConfig
	WithThinkingLevel(llm.ThinkingHigh)(&ac)
	require.NotNil(t, ac.thinkingLevel)
	assert.Equal(t, llm.ThinkingHigh, *ac.thinkingLevel)
}

// TestAssembleAgent_ThinkingLevel_ViaOption verifies that AssembleAgent with
// WithThinkingLevel(ThinkingHigh) sets the ThinkingLevel field on the returned
// AgentAssembly.
func TestAssembleAgent_ThinkingLevel_ViaOption(t *testing.T) {
	ctx := context.Background()
	assembly, err := AssembleAgent(ctx, nil, "eino", "gpt-4o", io.Discard,
		WithThinkingLevel(llm.ThinkingHigh),
	)
	require.NoError(t, err)
	defer assembly.Cleanup()
	assert.Equal(t, llm.ThinkingHigh, assembly.ThinkingLevel)
}

// TestAssembleAgent_ThinkingLevel_Default verifies that without an explicit
// option, the thinking level defaults to ThinkingMedium.
func TestAssembleAgent_ThinkingLevel_Default(t *testing.T) {
	ctx := context.Background()
	assembly, err := AssembleAgent(ctx, nil, "eino", "gpt-4o", io.Discard)
	require.NoError(t, err)
	defer assembly.Cleanup()
	assert.Equal(t, llm.ThinkingMedium, assembly.ThinkingLevel)
}

// TestAssembleAgent_ThinkingLevel_None verifies that ThinkingNone is correctly
// propagated (not confused with the zero value / "not set" sentinel).
func TestAssembleAgent_ThinkingLevel_None(t *testing.T) {
	ctx := context.Background()
	assembly, err := AssembleAgent(ctx, nil, "eino", "gpt-4o", io.Discard,
		WithThinkingLevel(llm.ThinkingNone),
	)
	require.NoError(t, err)
	defer assembly.Cleanup()
	assert.Equal(t, llm.ThinkingNone, assembly.ThinkingLevel)
}

// TestAssembleAgent_ModelCycler_NotConfigured verifies that ModelCycler is nil
// when model cycling is not configured.
func TestAssembleAgent_ModelCycler_NotConfigured(t *testing.T) {
	ctx := context.Background()
	assembly, err := AssembleAgent(ctx, nil, "eino", "gpt-4o", io.Discard)
	require.NoError(t, err)
	defer assembly.Cleanup()
	assert.Nil(t, assembly.ModelCycler)
}
