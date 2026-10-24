//go:build !headless

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
	m.width = 160
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
		"Known tokens: 2000/8000",
		"Estimated cost: $0.0250",
		"usage=reported/u1",
		"cost=estimated,unknown/c2",
		"price=configured_tier,unknown",
		"openai/gpt-4o-mini",
	} {
		if !containsText(bar, expected) {
			t.Errorf("status bar %q does not contain %q", bar, expected)
		}
	}
	if containsText(bar, "25%") || containsText(bar, "Tokens: 2000/8000") {
		t.Fatalf("partial token usage was presented as an exact percentage: %q", bar)
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
			m.modelName = "claude-sonnet-4"
			m.turnCount = 3
			m.sessionID = "abc12345"
			m.modeLabel = "chat"
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
			if len(lines) > 3 {
				t.Fatalf("status bar wrapped into %d lines at width %d: %q", len(lines), width, bar)
			}
			for _, line := range lines {
				if got := lipgloss.Width(line); got > width {
					t.Errorf("status line width = %d, exceeds terminal width %d: %q", got, width, line)
				}
			}
			if !strings.Contains(lines[0], "claude-sonnet-4") || !strings.Contains(lines[0], "turn #3") || !strings.Contains(lines[0], "abc12345") || !strings.Contains(lines[0], "chat") {
				t.Errorf("status context was lost at width %d: %q", width, lines[0])
			}
			if !strings.Contains(lines[1], "Known tokens: 2000/8000") || !strings.Contains(lines[1], "Estimated cost: $0.0250") {
				t.Errorf("full token metric or estimated cost subtotal was lost at width %d: %q", width, lines[1])
			}
			if !strings.Contains(lines[2], "/cost") || !strings.Contains(lines[2], "usage=reported") || !strings.Contains(lines[2], "cost=estimated") {
				t.Errorf("bounded source provenance cue missing at width %d: %q", width, lines[2])
			}
			if width < 70 {
				if !strings.Contains(lines[2], "unknown:1") || !strings.Contains(lines[2], "unknown:2") {
					t.Errorf("compact unknown counts missing at width %d: %q", width, lines[2])
				}
			} else if !strings.Contains(lines[2], "unknown usage=1") || !strings.Contains(lines[2], "unknown cost=2") {
				t.Errorf("unknown counts missing at width %d: %q", width, lines[2])
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

func TestStatusBarShowsFullProvenanceAtWideWidth(t *testing.T) {
	m := NewBubbleteaApp(make(chan AgentEvent, 1)).model
	m.width = 120
	m.modelName = "claude-sonnet-4"
	m.tokenInput = 1500
	m.tokenOutput = 500
	m.tokenMax = 8000
	m.tokenCost = 0.025
	m.tokenUnknownUsageAttempts = 1
	m.tokenUnknownCostAttempts = 2
	m.tokenUsageSource = core.TokenUsageSourceReported
	m.tokenCostSources = []core.TokenUsageCostSource{core.TokenUsageCostEstimated, core.TokenUsageCostUnknown}
	m.tokenPriceSources = []core.TokenUsagePriceSource{core.TokenUsagePriceConfiguredTier, core.TokenUsagePriceUnknown}
	m.tokenProviderModels = []core.TokenUsageProviderModel{{ProviderID: "openai", ModelID: "gpt-4o-mini"}}

	m.mu.Lock()
	bar := m.renderStatusBarLocked()
	m.mu.Unlock()
	lines := strings.Split(bar, "\n")
	if len(lines) != 3 {
		t.Fatalf("status bar has %d lines, want fixed three-line layout: %q", len(lines), bar)
	}
	for _, expected := range []string{
		"usage=reported/u1",
		"cost=estimated,unknown/c2",
		"price=configured_tier,unknown",
		"actual=openai/gpt-4o-mini",
	} {
		if !strings.Contains(lines[2], expected) {
			t.Errorf("wide provenance line %q does not contain %q", lines[2], expected)
		}
	}
	if got := lipgloss.Width(lines[2]); got > 120 {
		t.Fatalf("provenance line width=%d exceeds 120 columns: %q", got, lines[2])
	}
}

func TestStatusBarLabelsEstimatedPartialUsageAndCostAtCommonWidths(t *testing.T) {
	for _, width := range []int{60, 80, 120} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			m := NewBubbleteaApp(make(chan AgentEvent, 1)).model
			m.width = width
			m.modelName = "claude-sonnet-4"
			m.turnCount = 3
			m.sessionID = "abc12345"
			m.modeLabel = "chat"
			m.handleEvent(AgentEvent{
				Type: "token_usage",
				TokenUsage: &TokenUsageData{
					InputTokens: 1500, OutputTokens: 500, MaxTokens: 8000, Cost: 0.025,
					UnknownUsageAttempts: 1, UnknownCostAttempts: 2,
					UsageSource:    core.TokenUsageSourceEstimated,
					CostSources:    []core.TokenUsageCostSource{core.TokenUsageCostEstimated, core.TokenUsageCostUnknown},
					PriceSources:   []core.TokenUsagePriceSource{core.TokenUsagePriceConfiguredTier, core.TokenUsagePriceUnknown},
					ProviderModels: []core.TokenUsageProviderModel{{ProviderID: "openai", ModelID: "gpt-4o-mini"}},
				},
			})

			m.mu.Lock()
			bar := m.renderStatusBarLocked()
			m.mu.Unlock()
			lines := strings.Split(bar, "\n")
			if len(lines) != 3 {
				t.Fatalf("status bar has %d lines at width %d, want 3: %q", len(lines), width, bar)
			}
			for _, line := range lines {
				if got := lipgloss.Width(line); got > width {
					t.Errorf("status line width=%d exceeds %d columns: %q", got, width, line)
				}
			}
			for _, expected := range []string{"Estimated tokens: 2000/8000", "Estimated cost: $0.0250"} {
				if !strings.Contains(lines[1], expected) {
					t.Errorf("partial estimated metric line %q does not contain %q", lines[1], expected)
				}
			}
			if strings.Contains(lines[1], "Known tokens") || strings.Contains(lines[1], "Known cost") || strings.Contains(lines[1], "25%") {
				t.Errorf("partial estimated totals were presented as known or exact: %q", lines[1])
			}
			if width < 70 {
				for _, expected := range []string{"usage=estimated", "unknown:1", "cost=estimated", "unknown:2", "/cost"} {
					if !strings.Contains(lines[2], expected) {
						t.Errorf("narrow provenance line %q does not contain %q", lines[2], expected)
					}
				}
			} else if width < splitWidthThreshold {
				for _, expected := range []string{"usage=estimated", "unknown usage=1", "cost=estimated", "unknown cost=2", "/cost"} {
					if !strings.Contains(lines[2], expected) {
						t.Errorf("medium provenance line %q does not contain %q", lines[2], expected)
					}
				}
			} else {
				for _, expected := range []string{"usage=estimated/u1", "cost=estimated,unknown/c2", "actual=openai/gpt-4o-mini"} {
					if !strings.Contains(lines[2], expected) {
						t.Errorf("wide provenance line %q does not contain %q", lines[2], expected)
					}
				}
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

	if !containsText(bar, "Cost: unknown") || !containsText(bar, "/cost") {
		t.Fatalf("status bar omitted unknown cost evidence: %q", bar)
	}
	if containsText(bar, "$0.0000") {
		t.Fatalf("unknown cost was shown as a zero-cost total: %q", bar)
	}
}

func containsText(value, fragment string) bool {
	return strings.Contains(value, fragment)
}
