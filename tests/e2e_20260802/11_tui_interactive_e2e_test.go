// Package e2e_20260802 contains end-to-end integration tests for the TUI
// interactive mode. It exercises the full pipeline from MockLLMServer through
// LoopAgent → AgentImpl → HarnessImpl → BridgeEvents → BubbleteaApp,
// plus MCP tool execution, skill execution, automatic session compaction,
// and content-type mapping through the bridge.
package e2e_20260802 //nolint:staticcheck

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tui"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// =============================================================================
// 4. TestE2EInteractive_AutoCompaction
// =============================================================================

// TestE2EInteractive_AutoCompaction creates a conversation with many TurnItems
// that exceed the token budget, calls CompactIfNeeded, and verifies that
// compaction was triggered and the result has fewer tokens.
func TestE2EInteractive_AutoCompaction(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	est := compaction.NewHeuristicTokenEstimator()

	// Build a conversation that exceeds the budget.
	maxTokens := 100
	var items []compaction.TurnItem

	// Add system prompt.
	items = append(items, compaction.TurnItem{
		ID:      "sys-0",
		Role:    compaction.RoleSystem,
		Content: "You are a helpful assistant.",
	})

	// Add many user/assistant turns with large content.
	for i := 0; i < 20; i++ {
		items = append(items, compaction.TurnItem{
			ID:      fmt.Sprintf("user-%d", i),
			Role:    compaction.RoleUser,
			Content: "Please read the file " + strings.Repeat("x", 80),
		})
		items = append(items, compaction.TurnItem{
			ID:      fmt.Sprintf("asst-%d", i),
			Role:    compaction.RoleAssistant,
			Content: "Here is the content of the file: " + strings.Repeat("y", 80),
			ToolCalls: []llm.ToolCall{{
				ID:   fmt.Sprintf("read-%d", i),
				Name: "read",
			}},
		})
		items = append(items, compaction.TurnItem{
			ID:         fmt.Sprintf("tool-%d", i),
			Role:       compaction.RoleTool,
			ToolCallID: fmt.Sprintf("read-%d", i),
			ToolName:   "read",
			ToolResult: strings.Repeat("z", 120),
		})
	}

	// Verify the conversation exceeds the budget before compaction.
	beforeTokens := tokenSum(items, est)
	assert.Greater(t, beforeTokens, maxTokens, "conversation should exceed the budget before compaction")

	// Set up compaction infrastructure.
	compactor := compaction.NewUnifiedCompactor()
	midTurn := compaction.NewMidTurnCompact()

	// Trigger compaction.
	result, compactResult, err := midTurn.CompactIfNeeded(ctx, items, maxTokens, est, compactor)
	require.NoError(t, err)

	// Verify compaction was triggered.
	assert.True(t, compactResult.Triggered, "compaction should be triggered")
	assert.Equal(t, compaction.TriggerThreshold, compactResult.Reason)

	// Verify the result has fewer tokens.
	afterTokens := tokenSum(result.Items, est)
	assert.LessOrEqual(t, afterTokens, maxTokens, "compacted items should fit within the budget")
	assert.Less(t, afterTokens, beforeTokens, "compacted items should have fewer tokens than before")
}

// =============================================================================
// 6. TestE2EInteractive_BridgeContentTypeMapping
// =============================================================================

// TestE2EInteractive_BridgeContentTypeMapping tests all core.AgentEvent.Kind
// values map to the correct tui.ContentType through the bridge.
func TestE2EInteractive_BridgeContentTypeMapping(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tests := []struct {
		name   string
		kind   string
		wantCT string
	}{
		{
			name:   "message maps to assistant",
			kind:   "message",
			wantCT: tui.ContentTypeAssistant,
		},
		{
			name:   "tool_call maps to tool_call",
			kind:   "tool_call",
			wantCT: tui.ContentTypeToolCall,
		},
		{
			name:   "tool_result maps to tool_result",
			kind:   "tool_result",
			wantCT: tui.ContentTypeToolResult,
		},
		{
			name:   "error maps to error",
			kind:   "error",
			wantCT: tui.ContentTypeError,
		},
		{
			name:   "done maps to status",
			kind:   "done",
			wantCT: tui.ContentTypeStatus,
		},
		{
			name:   "unknown maps to status fallback",
			kind:   "unknown_kind",
			wantCT: tui.ContentTypeStatus,
		},
		{
			name:   "empty kind maps to status fallback",
			kind:   "",
			wantCT: tui.ContentTypeStatus,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use KindToContentType directly for unit-level mapping check.
			gotCT := tui.KindToContentType(tt.kind)
			assert.Equal(t, tt.wantCT, gotCT, "KindToContentType(%q) = %q, want %q", tt.kind, gotCT, tt.wantCT)

			// Also verify via CoreEventToAgentEvent which is the actual bridge function.
			coreEv := core.AgentEvent{Kind: tt.kind, Content: "test-content"}
			tuiEv := tui.CoreEventToAgentEvent(coreEv)
			assert.Equal(t, tt.wantCT, tuiEv.ContentType, "CoreEventToAgentEvent ContentType mismatch")
			assert.Equal(t, tt.kind, tuiEv.Type, "Type should preserve the original Kind")
			assert.Equal(t, "test-content", tuiEv.Content, "Content should be preserved")
		})
	}

	// Integration: verify that a real EventStream → BridgeEvents → App pipeline
	// preserves content type mapping correctly.
	t.Run("end-to-end bridge mapping", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		// Create an EventStream manually and push events of each kind.
		stream := core.NewEventStream(16)

		kinds := []struct {
			kind   string
			wantCT string
		}{
			{"message", tui.ContentTypeAssistant},
			{"tool_call", tui.ContentTypeToolCall},
			{"tool_result", tui.ContentTypeToolResult},
			{"error", tui.ContentTypeError},
			{"done", tui.ContentTypeStatus},
		}

		for _, k := range kinds {
			_ = stream.Send(core.AgentEvent{Kind: k.kind, Content: "payload:" + k.kind}) //nolint:errcheck,gosec
		}
		stream.Close()

		// Bridge and collect TUI events.
		tuiCh := tui.BridgeEvents(ctx, stream)

		var received []tui.AgentEvent
		for ev := range tuiCh {
			received = append(received, ev)
		}

		require.Len(t, received, len(kinds), "should receive exactly one TUI event per core event")

		for i, ev := range received {
			assert.Equal(t, kinds[i].wantCT, ev.ContentType,
				"event %d: ContentType mismatch for kind=%q", i, kinds[i].kind)
			assert.Equal(t, kinds[i].kind, ev.Type,
				"event %d: Type should preserve the original Kind", i)
			assert.Equal(t, "payload:"+kinds[i].kind, ev.Content,
				"event %d: Content should be preserved", i)
		}
	})
}
