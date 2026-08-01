package compaction

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// nearBudgetItems assembles a conversation whose estimated token count is
// roughly chars/4 for a controlled maxTokens in the triggering tests.
func nearBudgetItems(charsPerItem int) []TurnItem {
	return []TurnItem{
		{Role: RoleUser, Content: strings.Repeat("u", charsPerItem)},
		{Role: RoleAssistant, Content: strings.Repeat("a", charsPerItem)},
	}
}

func TestMidTurnCompactDoesNotTriggerWithinThreshold(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()
	items := nearBudgetItems(100) // ~50 tokens total

	compactor := NewRecordingCompactor("micro")
	mtc := NewMidTurnCompact() // ratio 0.8

	// maxTokens=100 -> threshold 80; current ~50 <= 80, so no compaction.
	out, res, err := mtc.CompactIfNeeded(ctx, items, 100, est, compactor)
	require.NoError(t, err)
	assert.False(t, res.Triggered)
	assert.Equal(t, TriggerNone, res.Reason)
	assert.Equal(t, items, out)
	assert.Empty(t, compactor.Called(), "compaction must not run under threshold")
}

func TestMidTurnCompactTriggersAtThreshold(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, exp := newTracedCtx(t)
	est := tracedEstimator()
	items := nearBudgetItems(500) // ~250 tokens > threshold 80

	compactor := NewRecordingCompactor("micro").WithResult(
		[]TurnItem{{Role: RoleSystem, Content: "compacted", IsCompaction: true}},
	)
	mtc := NewMidTurnCompact()

	out, res, err := mtc.CompactIfNeeded(ctx, items, 100, est, compactor)
	require.NoError(t, err)
	assert.True(t, res.Triggered)
	assert.Equal(t, TriggerThreshold, res.Reason)
	assert.Equal(t, []string{"micro"}, compactor.Called())
	assert.Len(t, out, 1)

	// The compaction.trigger span is emitted when compaction runs.
	assertSpanEventually(t, exp, "compaction.trigger")
}

func TestMidTurnCompactCompactTriggeredAlwaysRuns(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, exp := newTracedCtx(t)
	est := tracedEstimator()
	items := nearBudgetItems(10) // well under any threshold

	compactor := NewRecordingCompactor("micro").WithResult(
		[]TurnItem{{Role: RoleSystem, Content: "compacted", IsCompaction: true}},
	)
	mtc := NewMidTurnCompact()

	// CompactTriggered ignores any threshold and always compacts (manual).
	out, res, err := mtc.CompactTriggered(ctx, items, 100, est, compactor)
	require.NoError(t, err)
	assert.True(t, res.Triggered)
	assert.Equal(t, TriggerManual, res.Reason)
	assert.Equal(t, []string{"micro"}, compactor.Called())
	assert.Len(t, out, 1)

	assertSpanEventually(t, exp, "compaction.trigger")
}

func TestMidTurnCompactCustomThresholdRatio(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()
	items := nearBudgetItems(60) // ~30 tokens

	compactor := NewRecordingCompactor("micro")
	// A tight ratio of 0.2 -> threshold 12; current ~30 > 12 triggers.
	mtc := NewMidTurnCompact(WithThresholdRatio(0.2))

	_, res, err := mtc.CompactIfNeeded(ctx, items, 60, est, compactor)
	require.NoError(t, err)
	assert.True(t, res.Triggered)
	assert.Equal(t, TriggerThreshold, res.Reason)
}

func TestMidTurnCompactRequiresCompactor(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()
	items := nearBudgetItems(500)

	mtc := NewMidTurnCompact()
	_, res, err := mtc.CompactTriggered(ctx, items, 100, est, nil)
	require.ErrorIs(t, err, ErrCompactorRequired)
	assert.False(t, res.Triggered)
}

func TestTriggerReasonString(t *testing.T) {
	assert.Equal(t, "threshold", TriggerThreshold.String())
	assert.Equal(t, "overflow", TriggerOverflow.String())
	assert.Equal(t, "manual", TriggerManual.String())
	assert.Equal(t, "none", TriggerNone.String())
}
