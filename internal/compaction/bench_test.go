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
// compactor must replace all tool results to fit. The optimised implementation
// uses incremental decrement instead of re-estimating on every iteration.
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

// BenchmarkMicroCompactorFullScan demonstrates the pre-fix O(n²) behaviour
// where estimateTokens is called on every loop iteration. It serves as a
// baseline for comparing against BenchmarkMicroCompactor.
func BenchmarkMicroCompactorFullScan(b *testing.B) {
	est := NewHeuristicTokenEstimator()
	ctx := context.Background()

	for _, n := range []int{10, 50, 100, 500} {
		b.Run(fmt.Sprintf("items_%d", n), func(b *testing.B) {
			items := makeBenchItems(n)
			maxTokens := 10 * n
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _ = microCompactorFullScan(ctx, items, maxTokens, est)
			}
		})
	}
}

// microCompactorFullScan replicates the pre-fix MicroCompactor.Compact
// algorithm that calls estimateTokens on every loop iteration, producing
// O(n²) behaviour.
func microCompactorFullScan(_ context.Context, items []TurnItem, maxTokens int, estimator TokenEstimator) ([]TurnItem, error) {
	result := make([]TurnItem, len(items))
	copy(result, items)
	for estimateTokens(result, estimator) > maxTokens {
		replaced := false
		for i := range result {
			if result[i].ToolResult != "" && result[i].ToolResult != compactedToolResult {
				result[i].ToolResult = compactedToolResult
				replaced = true
				break
			}
		}
		if !replaced {
			break
		}
	}
	return result, nil
}

// BenchmarkUnifiedCompactor measures the routing compactor (micro → truncating)
// across different conversation sizes. The optimised implementation pre-computes
// token counts and caches them for sub-compactors.
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

// BenchmarkFindCutPoint measures the suffix-sum-optimised findCutPoint across
// different conversation sizes. The suffix sum array allows O(1) range queries
// instead of re-estimating the tail on every iteration.
func BenchmarkFindCutPoint(b *testing.B) {
	est := NewHeuristicTokenEstimator()
	compactor := NewSummaryCompactor(nil)

	for _, n := range []int{10, 50, 100, 500} {
		b.Run(fmt.Sprintf("items_%d", n), func(b *testing.B) {
			items := makeBenchItems(n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = compactor.findCutPoint(items, 10*n, est)
			}
		})
	}
}

// BenchmarkFindCutPointFullScan demonstrates the pre-fix O(n²) behaviour
// where estimateTokens is called for every tail slice. It serves as a baseline
// for comparing against BenchmarkFindCutPoint.
func BenchmarkFindCutPointFullScan(b *testing.B) {
	est := NewHeuristicTokenEstimator()

	for _, n := range []int{10, 50, 100, 500} {
		b.Run(fmt.Sprintf("items_%d", n), func(b *testing.B) {
			items := makeBenchItems(n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = findCutPointFullScan(items, 10*n, est)
			}
		})
	}
}

// findCutPointFullScan replicates the pre-fix findCutPoint algorithm that
// calls estimateTokens on items[cut:] for every cut, producing O(n²) behaviour.
func findCutPointFullScan(items []TurnItem, maxTokens int, estimator TokenEstimator) int {
	placeholder := estimateTokens([]TurnItem{{Content: summaryPlaceholder}}, estimator)
	for cut := 0; cut <= len(items); cut++ {
		if cut < len(items) && items[cut].Role == RoleTool {
			continue
		}
		if placeholder+estimateTokens(items[cut:], estimator) <= maxTokens {
			return cut
		}
	}
	return len(items)
}

// BenchmarkMidTurnEstimate verifies that the incremental midturn estimation
// avoids the O(n²) full-scan behaviour. Each iteration simulates the agent
// loop adding one more item and calling CompactIfNeeded. With incremental
// estimation, each call estimates only the single new item rather than
// re-scanning the entire conversation, yielding O(n) total work.
func BenchmarkMidTurnEstimate(b *testing.B) {
	est := NewCompositeTokenEstimator(0)
	compactor := NewRecordingCompactor("micro")
	ctx := context.Background()

	for _, n := range []int{50, 100, 200} {
		b.Run(fmt.Sprintf("items_%d", n), func(b *testing.B) {
			base := makeBenchItems(n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				mtc := NewMidTurnCompact()
				// Simulate the loop: each iteration adds one item
				// and calls CompactIfNeeded. A fresh slice is created
				// each iteration (mirroring messagesToTurnItems).
				for j := 1; j <= n; j++ {
					items := make([]TurnItem, j)
					copy(items, base[:j])
					_, _, _ = mtc.CompactIfNeeded(ctx, items, 1000000, est, compactor)
				}
			}
		})
	}
}

// BenchmarkMidTurnEstimateFullScan demonstrates the pre-fix O(n²) behaviour
// where every item is re-estimated on every iteration. It serves as a
// baseline for comparing against BenchmarkMidTurnEstimate.
func BenchmarkMidTurnEstimateFullScan(b *testing.B) {
	est := NewCompositeTokenEstimator(0)

	for _, n := range []int{50, 100, 200} {
		b.Run(fmt.Sprintf("items_%d", n), func(b *testing.B) {
			base := makeBenchItems(n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for j := 1; j <= n; j++ {
					items := make([]TurnItem, j)
					copy(items, base[:j])
					_ = estimateTokens(items, est)
				}
			}
		})
	}
}
