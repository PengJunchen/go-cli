// Package llm: thinking configuration and provider adaptation.
//
// This file defines the ThinkingLevel type, ThinkingConfig, the
// ThinkingAdapter interface and provider-specific implementations, plus a
// WithThinking Option that attaches thinking configuration to a generation
// call. Because GenerationOptions is declared in llm.go and cannot be extended
// from another file, WithThinking stores the config in a package-level
// sync.Map keyed by the *GenerationOptions pointer; ThinkingFromOpts retrieves
// it during request encoding.
package llm

import (
	"fmt"
	"strings"
	"sync"
)

// ThinkingLevel controls LLM reasoning depth.
type ThinkingLevel int

const (
	// ThinkingNone disables thinking. AdaptThinking strips [thinking:] tags.
	ThinkingNone ThinkingLevel = 0
	// ThinkingMinimal enables minimal thinking.
	ThinkingMinimal ThinkingLevel = 1
	// ThinkingLow enables low-effort thinking.
	ThinkingLow ThinkingLevel = 2
	// ThinkingMedium enables medium-effort thinking. This is the default.
	ThinkingMedium ThinkingLevel = 3
	// ThinkingHigh enables high-effort thinking.
	ThinkingHigh ThinkingLevel = 4
	// ThinkingMax enables maximum thinking budget.
	ThinkingMax ThinkingLevel = 5
)

// thinkingBudgets maps each ThinkingLevel to its token budget.
var thinkingBudgets = map[ThinkingLevel]int{
	ThinkingNone:    0,
	ThinkingMinimal: 1024,
	ThinkingLow:     4096,
	ThinkingMedium:  8192,
	ThinkingHigh:    16384,
	ThinkingMax:     32768,
}

// ThinkingConfig carries thinking configuration to providers.
type ThinkingConfig struct {
	// Level selects the reasoning depth.
	Level ThinkingLevel
	// BudgetTokens is the token budget corresponding to the level.
	BudgetTokens int
}

// ThinkingConfigForLevel returns a ThinkingConfig with the budget tokens for
// the given level.
func ThinkingConfigForLevel(level ThinkingLevel) ThinkingConfig {
	return ThinkingConfig{Level: level, BudgetTokens: thinkingBudgets[level]}
}

// ThinkingAdapter adapts ThinkingConfig for each provider. Apply mutates the
// provider-specific options map to enable thinking according to cfg.
type ThinkingAdapter interface {
	Apply(opts map[string]any, cfg ThinkingConfig)
}

// Compile-time assertions that the concrete adapters satisfy the interface.
var (
	_ ThinkingAdapter = OpenAIThinkingAdapter{}
	_ ThinkingAdapter = ClaudeThinkingAdapter{}
	_ ThinkingAdapter = GeminiThinkingAdapter{}
)

// OpenAIThinkingAdapter applies thinking via the reasoning_effort parameter.
// Minimal/Low map to "low", Medium to "medium", High/Max to "high".
// ThinkingNone produces no parameter.
type OpenAIThinkingAdapter struct{}

// Apply sets the reasoning_effort key in opts according to cfg.Level.
func (OpenAIThinkingAdapter) Apply(opts map[string]any, cfg ThinkingConfig) {
	if cfg.Level == ThinkingNone {
		return
	}
	var effort string
	switch cfg.Level {
	case ThinkingMinimal, ThinkingLow:
		effort = "low"
	case ThinkingMedium:
		effort = "medium"
	case ThinkingHigh, ThinkingMax:
		effort = "high"
	default:
		return
	}
	opts["reasoning_effort"] = effort
}

// ClaudeThinkingAdapter applies thinking via the thinking.budget_tokens field.
// ThinkingNone produces no parameter; all other levels set
// {"type": "enabled", "budget_tokens": cfg.BudgetTokens}.
type ClaudeThinkingAdapter struct{}

// Apply sets the thinking key in opts with type "enabled" and the budget.
func (ClaudeThinkingAdapter) Apply(opts map[string]any, cfg ThinkingConfig) {
	if cfg.Level == ThinkingNone {
		return
	}
	opts["thinking"] = map[string]any{
		"type":          "enabled",
		"budget_tokens": cfg.BudgetTokens,
	}
}

// GeminiThinkingAdapter applies thinking via the thinkingConfig field.
// ThinkingNone produces no parameter; all other levels set
// {"includeThoughts": true, "thinkingBudget": cfg.BudgetTokens}.
type GeminiThinkingAdapter struct{}

// Apply sets the thinkingConfig key in opts with includeThoughts and budget.
func (GeminiThinkingAdapter) Apply(opts map[string]any, cfg ThinkingConfig) {
	if cfg.Level == ThinkingNone {
		return
	}
	opts["thinkingConfig"] = map[string]any{
		"includeThoughts": true,
		"thinkingBudget":  cfg.BudgetTokens,
	}
}

// thinkingConfigs stores ThinkingConfig values keyed by *GenerationOptions
// pointer. This allows WithThinking to attach thinking configuration without
// modifying the GenerationOptions struct (which is declared in llm.go).
var thinkingConfigs sync.Map

// WithThinking returns an Option that attaches a ThinkingConfig to the
// generation call. The config is stored in a package-level map keyed by the
// *GenerationOptions pointer and can be retrieved with ThinkingFromOpts during
// request encoding.
func WithThinking(cfg ThinkingConfig) Option {
	return func(o *GenerationOptions) {
		thinkingConfigs.Store(o, cfg)
	}
}

// ThinkingFromOpts retrieves the ThinkingConfig stored by WithThinking for the
// given GenerationOptions. It returns the config and true if a thinking config
// was set, or a zero-value config and false otherwise.
func ThinkingFromOpts(o *GenerationOptions) (ThinkingConfig, bool) {
	v, ok := thinkingConfigs.Load(o)
	if !ok {
		return ThinkingConfig{}, false
	}
	return v.(ThinkingConfig), true
}

// DeleteThinking removes the ThinkingConfig stored for the given
// GenerationOptions. Callers should invoke this after retrieving the config to
// avoid leaking memory.
func DeleteThinking(o *GenerationOptions) {
	thinkingConfigs.Delete(o)
}

// ParseThinkingLevel converts a string to a ThinkingLevel. An empty string
// defaults to ThinkingMedium. Valid values (case-insensitive):
// none, minimal, low, medium, high, max.
func ParseThinkingLevel(s string) (ThinkingLevel, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "medium":
		return ThinkingMedium, nil
	case "none":
		return ThinkingNone, nil
	case "minimal":
		return ThinkingMinimal, nil
	case "low":
		return ThinkingLow, nil
	case "high":
		return ThinkingHigh, nil
	case "max":
		return ThinkingMax, nil
	default:
		return ThinkingMedium, fmt.Errorf("llm: invalid thinking level %q (want none|minimal|low|medium|high|max)", s)
	}
}
