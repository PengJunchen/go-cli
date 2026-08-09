package compaction

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestDefaultCompactorFactory_Create(t *testing.T) {
	f := NewDefaultCompactorFactory()

	tests := []struct {
		strategy string
		wantType string
		wantErr  bool
	}{
		{"", "*compaction.UnifiedCompactor", false},
		{"unified", "*compaction.UnifiedCompactor", false},
		{"micro", "*compaction.MicroCompactor", false},
		{"summary", "*compaction.SummaryCompactor", false},
		{"truncating", "*compaction.TruncatingCompactor", false},
		{"unknown", "", true},
		{"invalid", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.strategy, func(t *testing.T) {
			c, err := f.Create(tt.strategy)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for strategy %q, got nil", tt.strategy)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for strategy %q: %v", tt.strategy, err)
			}
			if c == nil {
				t.Fatalf("expected non-nil compactor for strategy %q", tt.strategy)
			}
		})
	}
}

func TestDefaultCompactorFactory_CompileTimeCheck(t *testing.T) {
	var _ CompactorFactory = (*DefaultCompactorFactory)(nil)
}

// TestFactoryWithSummarizer verifies that a DefaultCompactorFactory configured
// with WithFactorySummarizer produces working SummaryCompactors (AC-5) and
// wires the summarizer into the UnifiedCompactor for the "unified" strategy
// (AC-3). It contrasts this with a factory that has no summarizer.
func TestFactoryWithSummarizer(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()
	sum := &fakeSummarizer{summary: "factory-injected summary"}

	f := NewDefaultCompactorFactory(WithFactorySummarizer(sum))

	// AC-5: "summary" strategy returns a SummaryCompactor backed by the
	// injected summarizer (not the no-op fallback that errors).
	c, err := f.Create("summary")
	require.NoError(t, err)
	sc, ok := c.(*SummaryCompactor)
	require.True(t, ok, "expected *SummaryCompactor for summary strategy")

	items := []TurnItem{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: strings.Repeat("u", 300)},
		{Role: RoleUser, Content: "keep"},
	}
	out, err := sc.Compact(ctx, items, 30, est)
	require.NoError(t, err, "SummaryCompactor with injected summarizer must succeed")
	require.NotEmpty(t, sum.conversations(), "injected summarizer must be invoked")
	require.NotEmpty(t, out)
	assert.Equal(t, "factory-injected summary", out[0].Content)

	// AC-3: "unified" strategy wires the same summarizer into the
	// UnifiedCompactor, so it routes to summary for large user content.
	uc, err := f.Create("unified")
	require.NoError(t, err)
	uni, ok := uc.(*UnifiedCompactor)
	require.True(t, ok, "expected *UnifiedCompactor for unified strategy")

	largeItems := []TurnItem{
		{Role: RoleUser, Content: strings.Repeat("u", 500)},
		{Role: RoleAssistant, Content: strings.Repeat("a", 500)},
		{Role: RoleUser, Content: "recent question"},
		{Role: RoleAssistant, Content: "recent answer"},
	}
	out2, err := uni.Compact(ctx, largeItems, 30, est)
	require.NoError(t, err)
	assert.Equal(t, StrategySummary, uni.LastStrategy(),
		"unified compactor should use summary when summarizer is wired via factory")
	require.NotEmpty(t, out2)

	// Contrast: a factory without a summarizer does not route to summary.
	fNone := NewDefaultCompactorFactory()
	ucNone, err := fNone.Create("unified")
	require.NoError(t, err)
	uniNone, ok := ucNone.(*UnifiedCompactor)
	require.True(t, ok)
	_, err = uniNone.Compact(ctx, largeItems, 30, est)
	require.NoError(t, err)
	assert.NotEqual(t, StrategySummary, uniNone.LastStrategy(),
		"unified compactor without summarizer must not use summary")
}

// TestFactoryNoSummarizerBackwardCompatible verifies that a no-arg
// NewDefaultCompactorFactory() still produces a SummaryCompactor that uses the
// no-op fallback (backward compatibility).
func TestFactoryNoSummarizerBackwardCompatible(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	f := NewDefaultCompactorFactory() // no options
	c, err := f.Create("summary")
	require.NoError(t, err)
	sc, ok := c.(*SummaryCompactor)
	require.True(t, ok)

	items := []TurnItem{
		{Role: RoleUser, Content: strings.Repeat("u", 200)},
		{Role: RoleUser, Content: "keep"},
	}
	_, err = sc.Compact(ctx, items, 30, est)
	assert.ErrorIs(t, err, errNoSummarizer, "no-arg factory must use the no-op fallback")
}
