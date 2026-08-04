//go:build e2e

// Package e2e_20260802 contains end-to-end integration tests.
// This file verifies Phase 20 token estimation: UnicodeTokenEstimator
// (CJK/ASCII/mixed), HeuristicTokenEstimator, CompositeTokenEstimator, and
// AgentAssembly wiring.
package e2e_20260802

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
)

// TestET_Phase20_TokenEstimation exercises the token estimators end-to-end
// using real estimator instances. No mocks are used.
func TestET_Phase20_TokenEstimation(t *testing.T) {
	// AC-1: Chinese text "你好世界" (4 CJK chars) -> ~8 tokens (4 * 2).
	t.Run("AC1_UnicodeEstimator_Chinese", func(t *testing.T) {
		est := compaction.NewUnicodeTokenEstimator()
		n, err := est.Estimate("你好世界")
		require.NoError(t, err)
		assert.Equal(t, 8, n, "4 CJK chars * 2 tokens each = 8")
	})

	// AC-2: English text "hello world" (11 chars) -> ~3 tokens.
	t.Run("AC2_UnicodeEstimator_English", func(t *testing.T) {
		est := compaction.NewUnicodeTokenEstimator()
		n, err := est.Estimate("hello world")
		require.NoError(t, err)
		// 10 letters * 0.25 + 1 space * 0.5 = 3.0 -> round to 3
		assert.Equal(t, 3, n, "10 letters * 0.25 + 1 space * 0.5 = 3")
	})

	// AC-3: Mixed text (Chinese + English) -> estimate is reasonable.
	t.Run("AC3_UnicodeEstimator_Mixed", func(t *testing.T) {
		est := compaction.NewUnicodeTokenEstimator()
		// "你好world": 2 CJK chars (2*2=4) + 5 letters (5*0.25=1.25) = 5.25 -> 5
		n, err := est.Estimate("你好world")
		require.NoError(t, err)
		assert.Equal(t, 5, n, "2 CJK * 2 + 5 letters * 0.25 = 5.25 -> round to 5")

		// The mixed estimate should fall between pure Chinese and pure English.
		chineseN, _ := est.Estimate("你好世界")   // 8
		englishN, _ := est.Estimate("hello world") // 3
		assert.Greater(t, n, englishN, "mixed estimate should exceed pure English estimate")
		assert.Less(t, n, chineseN, "mixed estimate should be below pure Chinese estimate")
	})

	// AC-4: HeuristicTokenEstimator for "你好世界" returns ~3 (len=12, /4=3),
	// showing the old estimator underestimates CJK text.
	t.Run("AC4_HeuristicEstimator_Chinese", func(t *testing.T) {
		est := compaction.NewHeuristicTokenEstimator()
		n, err := est.Estimate("你好世界")
		require.NoError(t, err)
		// len("你好世界") = 12 bytes (4 CJK * 3 bytes each), 12 / 4 = 3
		assert.Equal(t, 3, n, "len('你好世界')=12 bytes / 4 = 3")
	})

	// AC-5: CompositeTokenEstimator with primary, no precise -> uses primary.
	t.Run("AC5_Composite_NoPrecise_UsesPrimary", func(t *testing.T) {
		primary := compaction.NewUnicodeTokenEstimator()
		comp := compaction.NewCompositeTokenEstimator(primary)
		text := "你好世界"
		n, err := comp.Estimate(text)
		require.NoError(t, err)
		primaryN, _ := primary.Estimate(text)
		assert.Equal(t, primaryN, n, "composite without precise should delegate to primary")
	})

	// AC-6: CompositeTokenEstimator with precise set -> uses precise.
	t.Run("AC6_Composite_WithPrecise_UsesPrecise", func(t *testing.T) {
		primary := compaction.NewUnicodeTokenEstimator()
		precise := compaction.NewHeuristicTokenEstimator()
		comp := compaction.NewCompositeTokenEstimator(primary)
		comp.SetPrecise(precise)
		text := "你好世界"
		n, err := comp.Estimate(text)
		require.NoError(t, err)
		preciseN, _ := precise.Estimate(text)
		assert.Equal(t, preciseN, n, "composite with precise should delegate to precise")
		// Unicode gives 8, Heuristic gives 3 — they differ, proving precise was used.
		primaryN, _ := primary.Estimate(text)
		assert.NotEqual(t, primaryN, n, "precise result (3) should differ from primary (8)")
	})

	// AC-7: AgentAssembly after AssembleAgent uses UnicodeTokenEstimator.
	t.Run("AC7_AssemblyUsesUnicodeEstimator", func(t *testing.T) {
		assembly := phase19wAssemble(t, phase19wTestConfig())
		require.NotNil(t, assembly.Estimator, "Estimator must be non-nil after AssembleAgent")
		_, ok := assembly.Estimator.(*compaction.UnicodeTokenEstimator)
		assert.True(t, ok, "Estimator should be *compaction.UnicodeTokenEstimator")
	})
}
