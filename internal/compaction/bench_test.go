package compaction

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// makeBenchItems builds a realistic conversation with n entries: a system
// message followed by user/assistant pairs, with every third entry being a
// tool call whose result is a 200-byte payload. This gives the compactor
// real work to do when the token budget is tight.
func makeBenchItems(n int) []TurnItem {
	items := make([]TurnItem, 0, n)
	items = append(items, TurnItem{Role: RoleSystem, Content: "You are a helpful assistant."})
	for i := 1; i < n; i++ {
		switch i % 3 {
		case 0:
			items = append(items, TurnItem{
				Role:       RoleTool,
				ToolName:   "read",
				ToolResult: strings.Repeat("x", 200),
			})
		case 1:
			items = append(items, TurnItem{
				Role:    RoleUser,
				Content: fmt.Sprintf("user message number %d", i),
			})
		case 2:
			items = append(items, TurnItem{
				Role:    RoleAssistant,
				Content: fmt.Sprintf("assistant response number %d", i),
			})
		}
	}
	return items
}

// BenchmarkMicroCompactor measures the zero-LLM compaction strategy across
// different conversation sizes. The token budget is set to 10*n so the
// compactor must replace all tool results to fit.
func BenchmarkMicroCompactor(b *testing.B) {
	est := NewHeuristicTokenEstimator()
	ctx := context.Background()

	for _, n := range []int{10, 50, 100, 500} {
		b.Run(fmt.Sprintf("items_%d", n), func(b *testing.B) {
			items := makeBenchItems(n)
			maxTokens := 10 * n
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _ = NewMicroCompactor().Compact(ctx, items, maxTokens, est)
			}
		})
	}
}

// BenchmarkUnifiedCompactor measures the routing compactor (micro → truncating)
// across different conversation sizes.
func BenchmarkUnifiedCompactor(b *testing.B) {
	est := NewHeuristicTokenEstimator()
	ctx := context.Background()

	for _, n := range []int{10, 50, 100, 500} {
		b.Run(fmt.Sprintf("items_%d", n), func(b *testing.B) {
			items := makeBenchItems(n)
			maxTokens := 10 * n
			compactor := NewUnifiedCompactor()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _ = compactor.Compact(ctx, items, maxTokens, est)
			}
		})
	}
}
