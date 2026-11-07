package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
)

type currentBudgetFallback struct{ calls int }

func (f *currentBudgetFallback) Compact(context.Context, []compaction.TurnItem, int, compaction.TokenEstimator) (compaction.CompactionResult, error) {
	f.calls++
	return compaction.CompactionResult{}, errors.New("unexpected fallback")
}

func runCurrentBudgetPreflight(ctx context.Context, at bool, messages []llm.Message, consumer *runCommitConsumer, strategy compaction.Compactor) ([]llm.Message, int, error) {
	if at {
		return preflightDurableRequestAt(ctx, messages, consumer, strategy, durableRuneCountEstimator{}, 6400, 3)
	}
	return preflightDurableRequest(ctx, messages, consumer, strategy, durableRuneCountEstimator{}, 6400, 3)
}

func currentBudgetConsumer(snapshot *CompactionSourceSnapshot) (*runCommitConsumer, *compactionCommitCountingSink) {
	const runID = "run-large-write-envelope"
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: runID}, nil)
	sink := &compactionCommitCountingSink{}
	return &runCommitConsumer{ctx: owner.ctx, owner: owner, source: snapshot, runID: runID, sink: sink}, sink
}

func TestSummaryCoverageCurrentBudgetFailsBeforeSourceOrCommit(t *testing.T) {
	for _, at := range []bool{false, true} {
		for _, unified := range []bool{false, true} {
			for _, blocks := range []bool{false, true} {
				t.Run(fmt.Sprintf("at=%v/unified=%v/blocks=%v", at, unified, blocks), func(t *testing.T) {
					snapshot, messages := durableLargeWriteFixture()
					if blocks {
						messages[3].Content = "shadow"
						messages[3].ContentBlocks = []llm.ContentBlock{{Type: "text", Text: strings.Repeat("大", 6314)}}
					} else {
						messages[3].Content = strings.Repeat("current-input-", 451)
					}
					original := cloneLLMMessage(messages[3])
					consumer, sink := currentBudgetConsumer(snapshot)
					source := &durableCompactionSummarizer{summary: "unexpected"}
					fallback := &currentBudgetFallback{}
					var strategy compaction.Compactor = compaction.NewSummaryCompactor(source)
					if unified {
						strategy = compaction.NewUnifiedCompactor(compaction.WithSummary(strategy), compaction.WithTruncating(fallback))
					}
					out, _, err := runCurrentBudgetPreflight(context.Background(), at, messages, consumer, strategy)
					require.ErrorIs(t, err, ErrDurableCompactionUnsupported)
					require.Zero(t, sink.commits)
					require.Empty(t, source.calls)
					require.Zero(t, fallback.calls)
					require.Nil(t, out)
					require.Equal(t, original, messages[3])
				})
			}
		}
	}
}

func TestSummaryCoverageProviderErrorsAreNeverRelabeledAsCurrentBudget(t *testing.T) {
	for _, at := range []bool{false, true} {
		for _, sourceErr := range []error{context.Canceled, compaction.ErrRequiresTruncating, compaction.ErrSummaryPromptBudgetTooSmall, borrowedEssentialBudgetError(t), errors.New("ordinary provider failure")} {
			t.Run(fmt.Sprintf("at=%v/%v", at, sourceErr), func(t *testing.T) {
				snapshot, messages := durableLargeWriteFixture()
				consumer, sink := currentBudgetConsumer(snapshot)
				source := &durableCompactionFailingSummarizer{err: sourceErr}
				fallback := &currentBudgetFallback{}
				strategy := compaction.NewUnifiedCompactor(compaction.WithSummary(compaction.NewSummaryCompactor(source)), compaction.WithTruncating(fallback))
				_, _, err := runCurrentBudgetPreflight(context.Background(), at, messages, consumer, strategy)
				require.ErrorIs(t, err, sourceErr)
				require.NotErrorIs(t, err, ErrDurableCompactionUnsupported)
				require.Contains(t, err.Error(), "summary source failed")
				require.Equal(t, 1, source.calls)
				require.Zero(t, fallback.calls)
				require.Zero(t, sink.commits)
			})
		}
	}
}

func TestSummaryCoverageOversizedEarlierFailureDoesNotFallback(t *testing.T) {
	for _, at := range []bool{false, true} {
		t.Run(fmt.Sprintf("at=%v", at), func(t *testing.T) {
			snapshot, messages := durableLargeWriteFixture()
			messages[2].Content = strings.Repeat("旧错误", 3000)
			messages[2].Runtime.ReceiptOutcome = ReceiptOutcomeError
			snapshot.Messages[2].Content = messages[2].Content
			snapshot.Messages[2].ReceiptOutcome = ReceiptOutcomeError
			original := cloneLLMMessage(messages[3])
			consumer, sink := currentBudgetConsumer(snapshot)
			source := &durableCompactionSummarizer{summary: "unexpected"}
			fallback := &currentBudgetFallback{}
			strategy := compaction.NewUnifiedCompactor(compaction.WithSummary(compaction.NewSummaryCompactor(source)), compaction.WithTruncating(fallback))
			_, _, err := runCurrentBudgetPreflight(context.Background(), at, messages, consumer, strategy)
			require.ErrorContains(t, err, "essential summary input")
			require.ErrorIs(t, err, ErrDurableCompactionUnsupported)
			require.ErrorIs(t, err, borrowedEssentialBudgetError(t))
			require.Empty(t, source.calls)
			require.Zero(t, fallback.calls)
			require.Zero(t, sink.commits)
			require.Equal(t, original, messages[3])
		})
	}
}

type currentBudgetForeign struct {
	err   error
	calls int
}

func (f *currentBudgetForeign) Compact(context.Context, []compaction.TurnItem, int, compaction.TokenEstimator) (compaction.CompactionResult, error) {
	f.calls++
	return compaction.CompactionResult{}, f.err
}

func borrowedEssentialBudgetError(t *testing.T) error {
	t.Helper()
	_, messages := durableLargeWriteFixture()
	messages[0].Content = strings.Repeat("earlier ", 1200)
	source := &durableCompactionSummarizer{summary: "unexpected"}
	_, err := compaction.NewSummaryCompactor(source).Compact(context.Background(), messagesToTurnItems(messages), 6400, durableRuneCountEstimator{})
	require.ErrorContains(t, err, "essential summary input")
	require.Empty(t, source.calls)
	return err
}

func TestSummaryCoverageDirectBudgetClassificationPreservesErrorChain(t *testing.T) {
	borrowed := borrowedEssentialBudgetError(t)
	for _, test := range []struct {
		name        string
		err         error
		unsupported bool
	}{
		{name: "borrowed-direct", err: borrowed, unsupported: true},
		{name: "foreign-same-message", err: errors.New(borrowed.Error())},
		{name: "borrowed-wrapped", err: fmt.Errorf("foreign wrapped: %w", borrowed)},
		{name: "cancel", err: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, at := range []bool{false, true} {
				snapshot, messages := durableLargeWriteFixture()
				consumer, sink := currentBudgetConsumer(snapshot)
				foreign := &currentBudgetForeign{err: test.err}
				_, _, err := runCurrentBudgetPreflight(context.Background(), at, messages, consumer, foreign)
				require.ErrorIs(t, err, test.err)
				if test.unsupported {
					require.ErrorIs(t, err, ErrDurableCompactionUnsupported)
				} else {
					require.NotErrorIs(t, err, ErrDurableCompactionUnsupported)
					require.Equal(t, test.err, err)
				}
				require.Equal(t, 1, foreign.calls)
				require.Zero(t, sink.commits)
			}
		})
	}
}

func TestSummaryCoverageTinyPromptPreservesBothErrorClasses(t *testing.T) {
	for _, at := range []bool{false, true} {
		t.Run(fmt.Sprintf("at=%v", at), func(t *testing.T) {
			snapshot, messages := durableLargeWriteFixture()
			original := cloneLLMMessage(messages[3])
			consumer, sink := currentBudgetConsumer(snapshot)
			source := &durableCompactionSummarizer{summary: "unexpected"}
			fallback := &currentBudgetFallback{}
			strategy := compaction.NewUnifiedCompactor(compaction.WithSummary(compaction.NewSummaryCompactor(source, compaction.WithMaxSummaryPromptTokens(1))), compaction.WithTruncating(fallback))
			_, _, err := runCurrentBudgetPreflight(context.Background(), at, messages, consumer, strategy)
			require.ErrorIs(t, err, ErrDurableCompactionUnsupported)
			require.ErrorIs(t, err, compaction.ErrSummaryPromptBudgetTooSmall)
			require.Empty(t, source.calls)
			require.Zero(t, fallback.calls)
			require.Zero(t, sink.commits)
			require.Equal(t, original, messages[3])
		})
	}
}
