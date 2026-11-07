package compaction

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSummaryEssentialBudgetClassifierRespectsOriginAndWrapping(t *testing.T) {
	for _, framing := range []bool{false, true} {
		t.Run(fmt.Sprintf("framing=%v", framing), func(t *testing.T) {
			items := coverageHistory()
			items[9].Content = strings.Repeat("required ", 900)
			var options []SummaryCompactorOption
			if framing {
				options = append(options, WithMaxSummaryPromptTokens(1))
			}
			capture := &coverageCapture{summary: "unexpected"}
			builtin := NewSummaryCompactor(capture, options...)
			_, direct := builtin.Compact(context.Background(), items, 2500, runeCountEstimator{})
			require.Error(t, direct)
			require.Empty(t, capture.prompts)
			require.True(t, IsSummaryEssentialInputBudgetError(direct))
			require.False(t, IsSummaryEssentialInputBudgetError(fmt.Errorf("foreign wrapper: %w", direct)))
			require.False(t, IsSummaryEssentialInputBudgetError(errors.New(direct.Error())))
			require.False(t, IsSummaryEssentialInputBudgetError(nil))
			if framing {
				require.ErrorIs(t, direct, ErrSummaryPromptBudgetTooSmall)
			}

			router := NewUnifiedCompactor(WithSummary(builtin))
			_, routed := router.Compact(context.Background(), items, 2500, runeCountEstimator{})
			require.True(t, IsSummaryEssentialInputBudgetError(routed))
			foreign := NewRecordingCompactor("foreign").WithError(direct)
			require.True(t, IsSummaryEssentialInputBudgetError(direct), "a trusted transparent forwarder retains the direct error classification")
			foreignRouter := NewUnifiedCompactor(WithSummary(foreign))
			_, fromForeign := foreignRouter.Compact(context.Background(), items, 2500, runeCountEstimator{})
			require.ErrorIs(t, fromForeign, direct)
			require.True(t, IsSummaryEssentialInputBudgetError(fromForeign), "a directly forwarded error retains its identity")
			provider := NewSummaryCompactor(borrowedSummaryFailure{err: direct})
			_, fromProvider := provider.Compact(context.Background(), coverageHistory(), 2500, runeCountEstimator{})
			require.ErrorIs(t, fromProvider, direct)
			require.False(t, IsSummaryEssentialInputBudgetError(fromProvider), "a wrapped provider error cannot impersonate a local failure")
		})
	}
}
