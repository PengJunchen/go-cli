package tui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/pengjunchen/go-cli/internal/core"
)

func TestStatusBarShowsTokenSourcesUnknownCountsAndActualModels(t *testing.T) {
	m := NewBubbleteaApp(make(chan AgentEvent, 1)).model
	m.handleEvent(AgentEvent{
		Type: "token_usage",
		TokenUsage: &TokenUsageData{
			InputTokens: 1500, OutputTokens: 500, MaxTokens: 8000, Cost: 0.025,
			UnknownUsageAttempts: 1, UnknownCostAttempts: 2,
			UsageSource:    core.TokenUsageSourceReported,
			CostSources:    []core.TokenUsageCostSource{core.TokenUsageCostEstimated, core.TokenUsageCostUnknown},
			PriceSources:   []core.TokenUsagePriceSource{core.TokenUsagePriceConfiguredTier, core.TokenUsagePriceUnknown},
			ProviderModels: []core.TokenUsageProviderModel{{ProviderID: "openai", ModelID: "gpt-4o-mini"}},
		},
	})

	m.mu.Lock()
	bar := m.renderStatusBarLocked()
	m.mu.Unlock()

	for _, expected := range []string{
		"Tokens: 2000/8000 (25%)",
		"usage=reported",
		"unknown usage attempts=1",
		"Known cost: $0.0250",
		"cost=estimated,unknown",
		"price=configured_tier,unknown",
		"unknown cost attempts=2",
		"openai/gpt-4o-mini",
	} {
		if !containsText(bar, expected) {
			t.Errorf("status bar %q does not contain %q", bar, expected)
		}
	}
	if containsText(bar, "Total cost:") || containsText(bar, "Cost: $0.0250") {
		t.Fatalf("partial known cost was presented as complete: %q", bar)
	}
}

func TestStatusBarClipsLongProvenanceAtNarrowWidthsWithoutDisplacingPrompt(t *testing.T) {
	for _, width := range []int{60, 80} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			m := NewBubbleteaApp(make(chan AgentEvent, 1)).model
			m.width = width
			m.height = 24
			m.modelName = "configured-model-" + strings.Repeat("x", 60)
			m.steerInputMode = true
			m.steerInput = "continue"
			m.tokenInput = 1500
			m.tokenOutput = 500
			m.tokenMax = 8000
			m.tokenCost = 0.025
			m.tokenUnknownUsageAttempts = 1
			m.tokenUnknownCostAttempts = 2
			m.tokenUsageSource = core.TokenUsageSourceReported
			m.tokenCostSources = []core.TokenUsageCostSource{core.TokenUsageCostEstimated, core.TokenUsageCostUnknown}
			m.tokenPriceSources = []core.TokenUsagePriceSource{core.TokenUsagePriceConfiguredTier, core.TokenUsagePriceUnknown}
			m.tokenProviderModels = []core.TokenUsageProviderModel{{
				ProviderID: strings.Repeat("provider", 8),
				ModelID:    strings.Repeat("model", 16),
			}}

			m.mu.Lock()
			bar := m.renderStatusBarLocked()
			m.mu.Unlock()
			lines := strings.Split(bar, "\n")
			if len(lines) > 2 {
				t.Fatalf("status bar wrapped into %d lines at width %d: %q", len(lines), width, bar)
			}
			for _, line := range lines {
				if got := lipgloss.Width(line); got > width {
					t.Errorf("status line width = %d, exceeds terminal width %d: %q", got, width, line)
				}
			}
			if !strings.Contains(bar, "Known cost: $0.0250 (unknown cost attempts=2)") {
				t.Errorf("partial cost label or unknown count was clipped: %q", bar)
			}
			if strings.Contains(bar, "https://") || strings.Contains(bar, "sk-SECRET") {
				t.Errorf("status bar leaked endpoint or credential-like data: %q", bar)
			}
			if view := m.View(); !strings.Contains(view, "> steer>") || !strings.Contains(view, "continue") {
				t.Errorf("status bar displaced steer prompt at width %d: %q", width, view)
			}
		})
	}
}

func TestStatusBarMarksZeroCostUnknownWhenNoAmountIsKnown(t *testing.T) {
	m := NewBubbleteaApp(make(chan AgentEvent, 1)).model
	m.handleEvent(AgentEvent{
		Type: "token_usage",
		TokenUsage: &TokenUsageData{
			UnknownUsageAttempts: 1,
			UnknownCostAttempts:  2,
			UsageSource:          core.TokenUsageSourceUnknown,
			CostSources:          []core.TokenUsageCostSource{core.TokenUsageCostUnknown},
			PriceSources:         []core.TokenUsagePriceSource{core.TokenUsagePriceUnknown},
		},
	})

	m.mu.Lock()
	bar := m.renderStatusBarLocked()
	m.mu.Unlock()

	if !containsText(bar, "Cost: unknown") || !containsText(bar, "unknown cost attempts=2") || !containsText(bar, "price=unknown") {
		t.Fatalf("status bar omitted unknown cost evidence: %q", bar)
	}
	if containsText(bar, "$0.0000") {
		t.Fatalf("unknown cost was shown as a zero-cost total: %q", bar)
	}
}

func containsText(value, fragment string) bool {
	return strings.Contains(value, fragment)
}
