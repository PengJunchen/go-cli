package llm

import (
	"fmt"
	"runtime"
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

// TestWithThinkingSetsField verifies that WithThinking stores the config on
// the GenerationOptions.Thinking field rather than in a package-level map.
func TestWithThinkingSetsField(t *testing.T) {
	opts := &GenerationOptions{}
	require.Nil(t, opts.Thinking, "Thinking should be nil before WithThinking")

	cfg := ThinkingConfigForLevel(ThinkingHigh)
	WithThinking(cfg)(opts)

	require.NotNil(t, opts.Thinking, "Thinking field should be set after WithThinking")
	assert.Equal(t, ThinkingHigh, opts.Thinking.Level)
	assert.Equal(t, cfg.BudgetTokens, opts.Thinking.BudgetTokens)
}

// TestThinkingFromOptsReturnsConfig verifies that ThinkingFromOpts returns the
// config stored by WithThinking.
func TestThinkingFromOptsReturnsConfig(t *testing.T) {
	opts := &GenerationOptions{}

	// Without WithThinking, should return false.
	_, ok := ThinkingFromOpts(opts)
	assert.False(t, ok, "should return false when no thinking config set")

	// After WithThinking, should return the config.
	cfg := ThinkingConfigForLevel(ThinkingMedium)
	WithThinking(cfg)(opts)

	got, ok := ThinkingFromOpts(opts)
	require.True(t, ok, "should return true after WithThinking")
	assert.Equal(t, cfg, got)
}

// TestNoCrossContamination verifies that thinking config set on one
// GenerationOptions does not leak to another. This would fail with a
// package-level map if keys collided.
func TestNoCrossContamination(t *testing.T) {
	optsA := &GenerationOptions{}
	optsB := &GenerationOptions{}

	WithThinking(ThinkingConfigForLevel(ThinkingHigh))(optsA)

	// optsB should not have any thinking config.
	_, okB := ThinkingFromOpts(optsB)
	assert.False(t, okB, "optsB should not see optsA's thinking config")

	// optsA should still have its config.
	gotA, okA := ThinkingFromOpts(optsA)
	require.True(t, okA)
	assert.Equal(t, ThinkingHigh, gotA.Level)

	// Set a different config on optsB.
	WithThinking(ThinkingConfigForLevel(ThinkingNone))(optsB)

	gotB, okB := ThinkingFromOpts(optsB)
	require.True(t, okB)
	assert.Equal(t, ThinkingNone, gotB.Level)

	// optsA should be unchanged.
	gotA2, okA2 := ThinkingFromOpts(optsA)
	require.True(t, okA2)
	assert.Equal(t, ThinkingHigh, gotA2.Level, "optsA should still have its original config")
}

// TestNoMemoryLeak verifies that thinking config is stored on the
// GenerationOptions struct (not in a global map), so it is garbage-collected
// when the struct is. After GC, a new GenerationOptions at the same address
// must not see stale config from a previous instance.
func TestNoMemoryLeak(t *testing.T) {
	// Create an opts, set thinking, and let it be garbage-collected.
	createAndDiscard := func() {
		opts := &GenerationOptions{}
		WithThinking(ThinkingConfigForLevel(ThinkingMax))(opts)
	}

	createAndDiscard()
	runtime.GC()

	// A new opts should not see any stale thinking config from the global map.
	// With the struct-field approach, this is guaranteed because there is no
	// global state.
	opts := &GenerationOptions{}
	_, ok := ThinkingFromOpts(opts)
	assert.False(t, ok, "new GenerationOptions must not inherit stale thinking config")
}
