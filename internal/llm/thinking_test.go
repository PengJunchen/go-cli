package llm

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseThinkingLevel_ValidValues(t *testing.T) {
	tests := []struct {
		input string
		want  ThinkingLevel
	}{
		{"none", ThinkingNone},
		{"minimal", ThinkingMinimal},
		{"low", ThinkingLow},
		{"medium", ThinkingMedium},
		{"high", ThinkingHigh},
		{"max", ThinkingMax},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseThinkingLevel(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseThinkingLevel_EmptyDefaultsToMedium(t *testing.T) {
	got, err := ParseThinkingLevel("")
	require.NoError(t, err)
	assert.Equal(t, ThinkingMedium, got)
}

func TestParseThinkingLevel_CaseInsensitive(t *testing.T) {
	got, err := ParseThinkingLevel("HIGH")
	require.NoError(t, err)
	assert.Equal(t, ThinkingHigh, got)
}

func TestParseThinkingLevel_TrimsWhitespace(t *testing.T) {
	got, err := ParseThinkingLevel("  high  ")
	require.NoError(t, err)
	assert.Equal(t, ThinkingHigh, got)
}

func TestParseThinkingLevel_InvalidReturnsError(t *testing.T) {
	got, err := ParseThinkingLevel("ultra")
	require.Error(t, err)
	assert.Equal(t, ThinkingMedium, got, "invalid input should still return the default")
}

func TestThinkingConfigForLevel_BudgetMapping(t *testing.T) {
	tests := []struct {
		level  ThinkingLevel
		budget int
	}{
		{ThinkingNone, 0},
		{ThinkingMinimal, 1024},
		{ThinkingLow, 4096},
		{ThinkingMedium, 8192},
		{ThinkingHigh, 16384},
		{ThinkingMax, 32768},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("level_%d", tt.level), func(t *testing.T) {
			cfg := ThinkingConfigForLevel(tt.level)
			assert.Equal(t, tt.level, cfg.Level)
			assert.Equal(t, tt.budget, cfg.BudgetTokens)
		})
	}
}
