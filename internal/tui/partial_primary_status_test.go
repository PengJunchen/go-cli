//go:build !headless

package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
)

func TestStatusBarPrimaryCoverageAndScope(t *testing.T) {
	for _, test := range []struct {
		name, coverage, scope, want string
		percent                     bool
	}{
		{"message-complete", "complete", "latest_assistant_message", "Message tokens: in=100 out=50", false},
		{"message-partial", "partial", "latest_assistant_message", "Message subtotal: in=100 out=50", false},
		{"message-legacy", "unknown", "latest_assistant_message", "Message tokens (coverage unknown): in=100 out=50", false},
		{"attempts-complete", "complete", "observed_attempts", "Observed tokens: in=100 out=50", false},
		{"attempts-partial", "partial", "observed_attempts", "Observed subtotal: in=100 out=50", false},
		{"attempts-legacy", "unknown", "observed_attempts", "Observed tokens (coverage unknown): in=100 out=50", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var usage core.TokenUsage
			wire := `{"input_tokens":100,"output_tokens":50,"max_tokens":1000,"usage_source":"reported","primary_coverage":"` + test.coverage + `","usage_scope":"` + test.scope + `"}`
			require.NoError(t, json.Unmarshal([]byte(wire), &usage))
			m := NewBubbleteaApp(make(chan AgentEvent, 1)).model
			m.width = 160
			m.handleEvent(CoreEventToAgentEvent(core.AgentEvent{Kind: "token_usage", TokenUsage: &usage}))
			m.mu.Lock()
			bar := m.renderStatusBarLocked()
			m.mu.Unlock()
			assert.Contains(t, bar, test.want)
			assert.Equal(t, test.percent, strings.Contains(bar, "15%"))
			assert.NotContains(t, bar, "/1000", "a message or attempt subtotal is not context occupancy")
		})
	}
}

func TestStatusBarUnknownAndEstimatedPrimaryUsage(t *testing.T) {
	for _, test := range []struct {
		name, source, scope, want string
	}{
		{"unknown-has-no-count", "unknown", "unknown", "Tokens: unknown"},
		{"estimated-stays-estimated", "estimated", "message_history_estimate", "Estimated tokens: 150/1000"},
		{"invalid-coverage-is-unknown", "reported", "latest_assistant_message", "Message tokens (coverage unknown): in=100 out=50"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var usage core.TokenUsage
			wire := `{"input_tokens":100,"output_tokens":50,"max_tokens":1000,"usage_source":"` + test.source + `","primary_coverage":"invented","usage_scope":"` + test.scope + `"}`
			require.NoError(t, json.Unmarshal([]byte(wire), &usage))
			m := NewBubbleteaApp(make(chan AgentEvent, 1)).model
			m.width = 160
			m.handleEvent(CoreEventToAgentEvent(core.AgentEvent{Kind: "token_usage", TokenUsage: &usage}))
			m.mu.Lock()
			bar := m.renderStatusBarLocked()
			m.mu.Unlock()
			assert.Contains(t, bar, test.want)
			assert.NotContains(t, bar, "15%")
			if test.source == "unknown" {
				assert.NotContains(t, bar, "150/1000")
			}
		})
	}
}

func TestStatusBarScopedCoverageIgnoresLegacyAttemptCount(t *testing.T) {
	for _, test := range []struct{ scope, label string }{
		{"latest_assistant_message", "Message tokens: in=100 out=50"},
		{"observed_attempts", "Observed tokens: in=100 out=50"},
	} {
		t.Run(test.scope, func(t *testing.T) {
			var usage core.TokenUsage
			wire := `{"input_tokens":100,"output_tokens":50,"max_tokens":1000,"unknown_usage_attempts":1,"usage_source":"reported","primary_coverage":"complete","usage_scope":"` + test.scope + `"}`
			require.NoError(t, json.Unmarshal([]byte(wire), &usage))
			m := NewBubbleteaApp(make(chan AgentEvent, 1)).model
			m.width = 160
			m.handleEvent(CoreEventToAgentEvent(core.AgentEvent{Kind: "token_usage", TokenUsage: &usage}))
			m.mu.Lock()
			bar := m.renderStatusBarLocked()
			m.mu.Unlock()
			assert.Contains(t, bar, test.label)
		})
	}
}

func TestScopedUsageDoesNotWarnOnContextOccupancy(t *testing.T) {
	for _, scope := range []string{"latest_assistant_message", "observed_attempts"} {
		t.Run(scope, func(t *testing.T) {
			var usage core.TokenUsage
			wire := `{"input_tokens":8500,"output_tokens":0,"max_tokens":10000,"usage_source":"reported","primary_coverage":"complete","usage_scope":"` + scope + `"}`
			require.NoError(t, json.Unmarshal([]byte(wire), &usage))
			m := NewBubbleteaApp(make(chan AgentEvent, 1)).model
			m.width = 160
			m.handleEvent(CoreEventToAgentEvent(core.AgentEvent{Kind: "token_usage", TokenUsage: &usage}))
			m.mu.Lock()
			bar := m.renderStatusBarLocked()
			m.mu.Unlock()
			assert.Contains(t, bar, "8500")
			assert.NotContains(t, bar, "/10000")
			assert.NotContains(t, bar, "85%")
			assert.NotContains(t, bar, "\x1b[38;", "usage totals are not context occupancy")
		})
	}
}
