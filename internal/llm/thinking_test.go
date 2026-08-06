package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThinkingConfigForLevel(t *testing.T) {
	tests := []struct {
		name   string
		level  ThinkingLevel
		budget int
	}{
		{"None", ThinkingNone, 0},
		{"Minimal", ThinkingMinimal, 1024},
		{"Low", ThinkingLow, 4096},
		{"Medium", ThinkingMedium, 8192},
		{"High", ThinkingHigh, 16384},
		{"Max", ThinkingMax, 32768},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ThinkingConfigForLevel(tt.level)
			assert.Equal(t, tt.level, cfg.Level)
			assert.Equal(t, tt.budget, cfg.BudgetTokens)
		})
	}
}

func TestOpenAIThinkingAdapter(t *testing.T) {
	adapter := OpenAIThinkingAdapter{}

	tests := []struct {
		name       string
		level      ThinkingLevel
		wantKey    bool
		wantEffort string
	}{
		{"None", ThinkingNone, false, ""},
		{"Minimal", ThinkingMinimal, true, "low"},
		{"Low", ThinkingLow, true, "low"},
		{"Medium", ThinkingMedium, true, "medium"},
		{"High", ThinkingHigh, true, "high"},
		{"Max", ThinkingMax, true, "high"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := map[string]any{}
			adapter.Apply(opts, ThinkingConfigForLevel(tt.level))
			if !tt.wantKey {
				assert.NotContains(t, opts, "reasoning_effort")
				assert.Empty(t, opts)
				return
			}
			assert.Equal(t, tt.wantEffort, opts["reasoning_effort"])
		})
	}
}

func TestClaudeThinkingAdapter(t *testing.T) {
	adapter := ClaudeThinkingAdapter{}

	t.Run("None produces no parameter", func(t *testing.T) {
		opts := map[string]any{}
		adapter.Apply(opts, ThinkingConfigForLevel(ThinkingNone))
		assert.NotContains(t, opts, "thinking")
		assert.Empty(t, opts)
	})

	nonNone := []struct {
		name   string
		level  ThinkingLevel
		budget int
	}{
		{"Minimal", ThinkingMinimal, 1024},
		{"Low", ThinkingLow, 4096},
		{"Medium", ThinkingMedium, 8192},
		{"High", ThinkingHigh, 16384},
		{"Max", ThinkingMax, 32768},
	}
	for _, tt := range nonNone {
		t.Run(tt.name, func(t *testing.T) {
			opts := map[string]any{}
			adapter.Apply(opts, ThinkingConfigForLevel(tt.level))
			require.Contains(t, opts, "thinking")
			thinking, ok := opts["thinking"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, "enabled", thinking["type"])
			assert.Equal(t, tt.budget, thinking["budget_tokens"])
		})
	}
}

func TestGeminiThinkingAdapter(t *testing.T) {
	adapter := GeminiThinkingAdapter{}

	t.Run("None produces no parameter", func(t *testing.T) {
		opts := map[string]any{}
		adapter.Apply(opts, ThinkingConfigForLevel(ThinkingNone))
		assert.NotContains(t, opts, "thinkingConfig")
		assert.Empty(t, opts)
	})

	nonNone := []struct {
		name   string
		level  ThinkingLevel
		budget int
	}{
		{"Minimal", ThinkingMinimal, 1024},
		{"Low", ThinkingLow, 4096},
		{"Medium", ThinkingMedium, 8192},
		{"High", ThinkingHigh, 16384},
		{"Max", ThinkingMax, 32768},
	}
	for _, tt := range nonNone {
		t.Run(tt.name, func(t *testing.T) {
			opts := map[string]any{}
			adapter.Apply(opts, ThinkingConfigForLevel(tt.level))
			require.Contains(t, opts, "thinkingConfig")
			tc, ok := opts["thinkingConfig"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, true, tc["includeThoughts"])
			assert.Equal(t, tt.budget, tc["thinkingBudget"])
		})
	}
}

func TestThinkingNoneProducesNoParameters(t *testing.T) {
	cfg := ThinkingConfigForLevel(ThinkingNone)
	adapters := []struct {
		name    string
		adapter ThinkingAdapter
		key     string
	}{
		{"OpenAI", OpenAIThinkingAdapter{}, "reasoning_effort"},
		{"Claude", ClaudeThinkingAdapter{}, "thinking"},
		{"Gemini", GeminiThinkingAdapter{}, "thinkingConfig"},
	}
	for _, a := range adapters {
		t.Run(a.name, func(t *testing.T) {
			opts := map[string]any{}
			a.adapter.Apply(opts, cfg)
			assert.NotContains(t, opts, a.key)
			assert.Empty(t, opts)
		})
	}
}

func TestWithThinkingStoresAndRetrievesConfig(t *testing.T) {
	opts := &GenerationOptions{}
	cfg := ThinkingConfigForLevel(ThinkingHigh)
	WithThinking(cfg)(opts)

	retrieved, ok := ThinkingFromOpts(opts)
	require.True(t, ok)
	assert.Equal(t, ThinkingHigh, retrieved.Level)
	assert.Equal(t, 16384, retrieved.BudgetTokens)

	DeleteThinking(opts)
	_, ok = ThinkingFromOpts(opts)
	assert.False(t, ok)
}

func TestThinkingFromOptsNotSet(t *testing.T) {
	opts := &GenerationOptions{}
	_, ok := ThinkingFromOpts(opts)
	assert.False(t, ok)
}

func TestThinkingAdapterInterfaceCompliance(t *testing.T) {
	var _ ThinkingAdapter = OpenAIThinkingAdapter{}
	var _ ThinkingAdapter = ClaudeThinkingAdapter{}
	var _ ThinkingAdapter = GeminiThinkingAdapter{}
}
