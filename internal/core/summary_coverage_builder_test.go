package core

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestCoverageDurablePreflightCommitsOnlyProtectedSummary(t *testing.T) {
	for _, overBudget := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "fail-closed"}[overBudget], func(t *testing.T) {
			snapshot, messages := durableLargeWriteFixture()
			requirement := "negative quantities must return ErrNegative"
			failure := "test failed because negative quantity was accepted"
			if overBudget {
				requirement = strings.Repeat("必须", 4000)
			}
			snapshot.Messages[0].Content, messages[0].Content = requirement, requirement
			snapshot.Messages[2].Content, messages[2].Content = failure, failure
			snapshot.Messages[2].ReceiptOutcome = ReceiptOutcomeError
			messages[2].Runtime.ReceiptOutcome = ReceiptOutcomeError
			original := append([]llm.Message(nil), messages...)
			const runID = "run-large-write-envelope"
			owner := newRunOwner(context.Background(), nil, RunClaim{ID: runID}, nil)
			sink := &compactionCommitCountingSink{}
			consumer := &runCommitConsumer{ctx: owner.ctx, owner: owner, source: snapshot, runID: runID, sink: sink}
			summarizer := &durableCompactionSummarizer{summary: "accepted durable summary"}
			strategy := compaction.NewUnifiedCompactor(compaction.WithSummary(compaction.NewSummaryCompactor(summarizer)))
			out, index, err := preflightDurableRequestAt(context.Background(), messages, consumer, strategy, durableRuneCountEstimator{}, 6400, 3)
			require.Equal(t, original, messages)
			if overBudget {
				require.ErrorContains(t, err, "essential summary input")
				require.Zero(t, sink.commits)
				require.Empty(t, summarizer.calls)
				require.Nil(t, out)
				return
			}
			require.NoError(t, err)
			require.Len(t, summarizer.calls, 1)
			require.Contains(t, summarizer.calls[0], requirement)
			require.Contains(t, summarizer.calls[0], failure)
			require.Equal(t, 1, sink.commits)
			require.Equal(t, CompactionCursor{PriorHistoryCount: 3}, sink.projection.Through)
			require.Equal(t, "accepted durable summary", sink.projection.Summary)
			require.Equal(t, 1, index)
			require.Len(t, out, 2)
			require.Equal(t, compaction.UntrustedSummaryContext("accepted durable summary"), out[0].Content)
			require.Equal(t, messages[3], out[1])
		})
	}
}
