package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
)

type omissionCommitSink struct {
	inertRunCommitSink
	commits int
	err     error
}

func (sink *omissionCommitSink) CommitCompaction(context.Context, CompactionProjection) error {
	sink.commits++
	return sink.err
}

func omissionEnvelopeFixture(t *testing.T) ([]llm.Message, *CompactionSourceSnapshot, *AgentMessage) {
	t.Helper()
	var imageBytes bytes.Buffer
	require.NoError(t, png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	imageURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageBytes.Bytes())
	const runID = "synthetic-envelope-omission"
	current := &AgentMessage{Role: "user", Content: "current input", InputKind: InputKindUser}
	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "system", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceEphemeral}}},
		{Role: llm.RoleUser, Content: current.Content},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "image-1", Name: "read"}, {ID: "text-1", Name: "read"}}},
		{Role: llm.RoleTool, ToolCallID: "image-1", Name: "read", ContentBlocks: []llm.ContentBlock{{Type: "image_url", ImageURL: &llm.ImageURL{URL: imageURL, Detail: "high"}}}},
		{Role: llm.RoleTool, ToolCallID: "text-1", Name: "read", Content: "same group text"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "old", Name: "read"}}},
		{Role: llm.RoleTool, ToolCallID: "old", Name: "read", Content: strings.Repeat("old diagnostic ", 500)},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "image-2", Name: "read"}}},
		{Role: llm.RoleTool, ToolCallID: "image-2", Name: "read", ContentBlocks: []llm.ContentBlock{{Type: "image_url", ImageURL: &llm.ImageURL{URL: imageURL, Detail: "low"}}}},
	}
	for i := 1; i < len(messages); i++ {
		messages[i].Runtime.Source = llm.SourceRef{Kind: llm.SourceRunTranscript, RunID: runID, TranscriptIndex: uint64(i - 1)}
		if messages[i].Role == llm.RoleTool {
			messages[i].Runtime.ReceiptOutcome = ReceiptOutcomeSuccess
		}
	}
	return messages, &CompactionSourceSnapshot{BranchID: "main"}, current
}

func TestImageOmissionAdmitsFullEnvelopeBeforeCommit(t *testing.T) {
	messages, snapshot, input := omissionEnvelopeFixture(t)
	candidate := requestEnvelopeCandidate(t, llm.ModelInfo{Name: "synthetic-image-envelope", ContextWindow: 50000, MaxOutputTokens: 200})
	proposal := llm.CandidateProposal{OperationID: "synthetic-image-operation", Candidate: candidate, Reason: llm.CandidateInitial, Antecedent: llm.CandidateAntecedent{Kind: llm.AntecedentInitial}}
	estimator := durableRuneCountEstimator{}
	for _, tc := range []struct {
		name        string
		ceiling     int
		reserve     int
		recovery    string
		commitErr   error
		reason      string
		protected   bool
		wantError   bool
		wantCommits int
	}{
		{name: "fitting omission", ceiling: 1200, reserve: 100, wantCommits: 1},
		{name: "already fits", ceiling: 20000, reserve: 100},
		{name: "protected floor cannot fit", ceiling: 400, reserve: 100, protected: true, wantError: true},
		{name: "reserves exhaust budget", ceiling: 200, reserve: 100, wantError: true},
		{name: "final envelope rejects before commit", ceiling: 1200, reserve: 100, recovery: strings.Repeat("r", 700), reason: "request still exceeds the message budget after compaction", wantError: true},
		{name: "commit failure", ceiling: 1200, reserve: 100, commitErr: errors.New("synthetic sync failure"), wantError: true, wantCommits: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &omissionCommitSink{err: tc.commitErr}
			owner := newRunOwner(context.Background(), nil, RunClaim{ID: "synthetic-envelope-omission"}, nil)
			consumer := &runCommitConsumer{ctx: owner.ctx, owner: owner, sink: sink, source: snapshot, runID: "synthetic-envelope-omission", initialInput: input}
			summary := &durableCompactionSummarizer{summary: "must not run"}
			strategy := compaction.NewUnifiedCompactor(compaction.WithSummary(compaction.NewSummaryCompactor(summary)))
			base := preparedRequestBase{multimodalTokensPerItemReserve: tc.reserve, requested: llm.GenerationOptions{MaxTokens: r3TokenPointer(150)}}
			if tc.recovery != "" {
				base.recoveryControl = &llm.Message{Role: llm.RoleUser, Content: tc.recovery}
			}
			plan, output, _, err := prepareAdmittedRequestPlan(context.Background(), proposal, messages, 1, base, consumer, nil, strategy, estimator, tc.ceiling)
			if tc.wantError {
				require.Error(t, err)
				if tc.protected {
					require.ErrorIs(t, err, compaction.ErrImageCompactionBlocked)
				}
				if tc.reason != "" {
					var envelope *RequestEnvelopeError
					require.ErrorAs(t, err, &envelope)
					require.Equal(t, tc.reason, envelope.Reason)
				}
				require.Nil(t, plan)
				require.Empty(t, consumer.projections)
			} else {
				require.NoError(t, err)
				require.NotNil(t, plan)
				require.True(t, envelopeEstimateFits(plan.Estimate()))
				require.Equal(t, 2*tc.reserve, plan.Estimate().Multimodal.Tokens)
				before := messagesToTurnItems(messages)
				after := messagesToTurnItems(output)
				for i := range after {
					for _, old := range before {
						if old.Runtime.Source == after[i].Runtime.Source {
							after[i].ID = old.ID
						}
					}
				}
				require.NoError(t, compaction.ValidateImageGroupRetention(before, after))
			}
			require.Equal(t, tc.wantCommits, sink.commits)
			require.Empty(t, summary.calls, "no summary request is permitted")
		})
	}
}

func TestImageOmissionCanceledAdmissionDoesNotCommit(t *testing.T) {
	messages, snapshot, input := omissionEnvelopeFixture(t)
	candidate := requestEnvelopeCandidate(t, llm.ModelInfo{Name: "canceled-image-envelope", ContextWindow: 50000, MaxOutputTokens: 200})
	proposal := llm.CandidateProposal{OperationID: "canceled-image-operation", Candidate: candidate, Reason: llm.CandidateInitial, Antecedent: llm.CandidateAntecedent{Kind: llm.AntecedentInitial}}
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "synthetic-envelope-omission"}, nil)
	sink := &omissionCommitSink{}
	consumer := &runCommitConsumer{ctx: owner.ctx, owner: owner, sink: sink, source: snapshot, runID: "synthetic-envelope-omission", initialInput: input}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err := prepareAdmittedRequestPlan(ctx, proposal, messages, 1, preparedRequestBase{multimodalTokensPerItemReserve: 100}, consumer, nil, compaction.NewUnifiedCompactor(), durableRuneCountEstimator{}, 1200)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, sink.commits)
	require.Empty(t, consumer.projections)
}

func TestImageOmissionCoreRevalidatesSecondCompaction(t *testing.T) {
	messages, snapshot, input := omissionEnvelopeFixture(t)
	candidate := requestEnvelopeCandidate(t, llm.ModelInfo{Name: "repeat-image-envelope", ContextWindow: 50000, MaxOutputTokens: 200})
	proposal := llm.CandidateProposal{OperationID: "repeat-image-operation", Candidate: candidate, Reason: llm.CandidateInitial, Antecedent: llm.CandidateAntecedent{Kind: llm.AntecedentInitial}}
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "synthetic-envelope-omission"}, nil)
	sink := &omissionCommitSink{}
	consumer := &runCommitConsumer{ctx: owner.ctx, owner: owner, sink: sink, source: snapshot, runID: "synthetic-envelope-omission", initialInput: input}
	base := preparedRequestBase{multimodalTokensPerItemReserve: 100, requested: llm.GenerationOptions{MaxTokens: r3TokenPointer(150)}}
	strategy := compaction.NewUnifiedCompactor()
	_, first, current, err := prepareAdmittedRequestPlan(t.Context(), proposal, messages, 1, base, consumer, nil, strategy, durableRuneCountEstimator{}, 1200)
	require.NoError(t, err)
	require.NoError(t, consumer.validateDurableRequest(messagesToTurnItems(first)))
	appendMessage := func(message llm.Message, index uint64) {
		message.Runtime.Source = llm.SourceRef{Kind: llm.SourceRunTranscript, RunID: "synthetic-envelope-omission", TranscriptIndex: index}
		if message.Role == llm.RoleTool {
			message.Runtime.ReceiptOutcome = ReceiptOutcomeSuccess
		}
		first = append(first, message)
	}
	appendMessage(llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "new-old", Name: "read"}}}, 8)
	appendMessage(llm.Message{Role: llm.RoleTool, ToolCallID: "new-old", Name: "read", Content: strings.Repeat("new old data ", 500)}, 9)
	appendMessage(llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "new-image", Name: "read"}}}, 10)
	appendMessage(llm.Message{Role: llm.RoleTool, ToolCallID: "new-image", Name: "read", ContentBlocks: messages[3].ContentBlocks}, 11)
	_, second, _, err := prepareAdmittedRequestPlan(t.Context(), proposal, first, current, base, consumer, nil, strategy, durableRuneCountEstimator{}, 1400)
	require.NoError(t, err)
	require.Equal(t, 2, sink.commits)
	require.Len(t, consumer.projections, 2)
	require.Greater(t, consumer.projections[1].Through.TranscriptCount, consumer.projections[0].Through.TranscriptCount)
	require.NoError(t, consumer.validateDurableRequest(messagesToTurnItems(second)))
}
