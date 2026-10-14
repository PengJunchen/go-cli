package core

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestNonDurableContextCompactsBeforeFirstProviderCall(t *testing.T) {
	model := &budgetPreflightRecordingModel{}
	loop := NewLoopAgent(WithLLM(model), WithSystemPrompt("s"))
	loop.SetMidTurnCompaction(
		compaction.NewMidTurnCompact(compaction.WithThresholdRatio(1)),
		compaction.NewUnifiedCompactor(),
		budgetPreflightCharacterEstimator{},
		56,
	)

	events, err := loop.Run(context.Background(), Submission{
		Content: "now",
		History: []AgentMessage{
			{Role: "user", Content: "old"},
			{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read", Args: map[string]any{}}}},
			{Role: "tool", Content: "receipt", ToolCallID: "call-1", ToolName: "read"},
		},
	})
	require.NoError(t, err)
	require.Empty(t, findEvents(events, "tool_call"), "the final model response must not dispatch a tool")

	requests := model.requestsSnapshot()
	require.Len(t, requests, 1)
	request := requests[0]
	require.Len(t, request, 4, "the old user context should be compacted before the first request")
	require.Equal(t, "s", request[0].Content)
	require.Equal(t, llm.RoleAssistant, request[1].Role)
	require.Len(t, request[1].ToolCalls, 1)
	require.Equal(t, "call-1", request[1].ToolCalls[0].ID)
	require.Equal(t, llm.RoleTool, request[2].Role)
	require.Equal(t, "call-1", request[2].ToolCallID)
	require.Equal(t, "receipt", request[2].Content)
	require.Equal(t, llm.RoleUser, request[3].Role)
	require.Equal(t, "now", request[3].Content)
	require.NotContains(t, requestText(request), "old")
}

func TestNonDurableContextPreflightStopsBeforeProviderWhenUncompactionable(t *testing.T) {
	tests := []struct {
		name         string
		systemPrompt string
		submission   Submission
		maxTokens    int
		wantErr      error
	}{
		{
			name:         "mandatory system prompt exceeds budget",
			systemPrompt: "mandatory system prompt",
			submission:   Submission{Content: "input"},
			maxTokens:    10,
			wantErr:      compaction.ErrMandatorySystemPromptOverBudget,
		},
		{
			name:         "tool call group is missing a receipt",
			systemPrompt: "s",
			submission: Submission{
				Content: strings.Repeat("q", 20),
				History: []AgentMessage{{
					Role:      "assistant",
					ToolCalls: []llm.ToolCall{{ID: "unfinished", Name: "read", Args: map[string]any{}}},
				}},
			},
			maxTokens: 10,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := &budgetPreflightRecordingModel{}
			loop := NewLoopAgent(WithLLM(model), WithSystemPrompt(test.systemPrompt))
			loop.SetMidTurnCompaction(
				compaction.NewMidTurnCompact(compaction.WithThresholdRatio(1)),
				compaction.NewUnifiedCompactor(),
				budgetPreflightCharacterEstimator{},
				test.maxTokens,
			)

			_, err := loop.Run(context.Background(), test.submission)
			require.Error(t, err)
			if test.wantErr != nil {
				require.ErrorIs(t, err, test.wantErr)
			}
			require.Empty(t, model.requestsSnapshot(), "an uncompactionable request must fail before provider dispatch")
		})
	}
}

func TestNonDurableContextPreflightDoesNotDropOversizedCurrentInput(t *testing.T) {
	model := &budgetPreflightRecordingModel{}
	loop := NewLoopAgent(WithLLM(model), WithSystemPrompt("s"))
	loop.SetMidTurnCompaction(
		compaction.NewMidTurnCompact(compaction.WithThresholdRatio(1)),
		compaction.NewUnifiedCompactor(),
		budgetPreflightCharacterEstimator{},
		10,
	)

	_, err := loop.Run(context.Background(), Submission{
		Content: strings.Repeat("q", 20),
		History: []AgentMessage{{Role: "user", Content: "prior"}},
	})
	require.ErrorIs(t, err, errCurrentSubmissionNotPreserved)
	require.Empty(t, model.requestsSnapshot(), "the provider must not receive a request after compaction drops its current input")
}

func TestNonDurableContextPreflightUsesFreshEstimateForEachRun(t *testing.T) {
	model := &budgetPreflightRecordingModel{}
	loop := NewLoopAgent(WithLLM(model), WithSystemPrompt("s"))
	loop.SetMidTurnCompaction(
		compaction.NewMidTurnCompact(compaction.WithThresholdRatio(1)),
		compaction.NewUnifiedCompactor(),
		budgetPreflightCharacterEstimator{},
		13,
	)

	_, err := loop.Run(context.Background(), Submission{Content: "ok"})
	require.NoError(t, err)
	require.Len(t, model.requestsSnapshot(), 1)

	_, err = loop.Run(context.Background(), Submission{Content: strings.Repeat("q", 20)})
	require.ErrorIs(t, err, errCurrentSubmissionNotPreserved)
	require.Len(t, model.requestsSnapshot(), 1, "a reused loop must re-estimate the new input before dispatch")
}

func TestNonDurableContextPreflightRejectsCompactorChangesToCurrentInput(t *testing.T) {
	tests := []struct {
		name    string
		rewrite func([]compaction.TurnItem) []compaction.TurnItem
	}{
		{
			name: "content changed",
			rewrite: func(items []compaction.TurnItem) []compaction.TurnItem {
				items = append([]compaction.TurnItem(nil), items...)
				items[len(items)-1].Content = "rewritten prompt"
				return items
			},
		},
		{
			name: "role changed",
			rewrite: func(items []compaction.TurnItem) []compaction.TurnItem {
				items = append([]compaction.TurnItem(nil), items...)
				items[len(items)-1].Role = compaction.RoleAssistant
				return items
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := &budgetPreflightRecordingModel{}
			loop := NewLoopAgent(WithLLM(model), WithSystemPrompt("s"))
			loop.SetMidTurnCompaction(
				compaction.NewMidTurnCompact(compaction.WithThresholdRatio(1)),
				budgetPreflightRewriteCompactor{rewrite: test.rewrite},
				budgetPreflightCharacterEstimator{},
				10,
			)

			_, err := loop.Run(context.Background(), Submission{Content: strings.Repeat("q", 20)})
			require.ErrorIs(t, err, errCurrentSubmissionNotPreserved)
			require.Empty(t, model.requestsSnapshot(), "the provider must not receive a rewritten current input")
		})
	}
}

func TestNonDurableContextPreflightRejectsInPlaceCurrentContentBlockMutation(t *testing.T) {
	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "s"},
		{
			Role: llm.RoleUser, Content: "current input",
			ContentBlocks: []llm.ContentBlock{{Type: "text", Text: "original current block"}},
		},
	}
	compactor := &budgetPreflightInPlaceBlockCompactor{}
	_, _, _, err := preflightNonDurableRequest(
		context.Background(), messages, 1,
		compaction.NewMidTurnCompact(compaction.WithThresholdRatio(1)), compactor,
		budgetPreflightCharacterEstimator{}, 1,
	)
	require.ErrorIs(t, err, errCurrentSubmissionNotPreserved)
	require.Equal(t, 1, compactor.calls)
	require.Equal(t, "original current block", messages[1].ContentBlocks[0].Text,
		"a compactor must not mutate the caller's current-input graph while its rewrite is rejected")
}

func TestNonDurableContextPreflightRejectsBrokenCompactorToolGroup(t *testing.T) {
	model := &budgetPreflightRecordingModel{}
	loop := NewLoopAgent(WithLLM(model), WithSystemPrompt("s"))
	loop.SetMidTurnCompaction(
		compaction.NewMidTurnCompact(compaction.WithThresholdRatio(1)),
		budgetPreflightRewriteCompactor{rewrite: func(items []compaction.TurnItem) []compaction.TurnItem {
			result := make([]compaction.TurnItem, 0, len(items)-1)
			for _, item := range items {
				if item.Role == compaction.RoleTool {
					continue
				}
				result = append(result, item)
			}
			return result
		}},
		budgetPreflightCharacterEstimator{},
		11,
	)

	_, err := loop.Run(context.Background(), Submission{
		Content: "now",
		History: []AgentMessage{
			{Role: "user", Content: "old"},
			{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read", Args: map[string]any{}}}},
			{Role: "tool", Content: "receipt", ToolCallID: "call-1", ToolName: "read"},
		},
	})
	require.ErrorContains(t, err, "malformed tool-call group")
	require.Empty(t, model.requestsSnapshot(), "the provider must not receive a broken tool-call group")
}

type budgetPreflightCharacterEstimator struct{}

func (budgetPreflightCharacterEstimator) Estimate(text string) (int, error) {
	return len([]rune(text)), nil
}

type budgetPreflightRecordingModel struct {
	mu       sync.Mutex
	requests [][]llm.Message
}

func (model *budgetPreflightRecordingModel) Generate(_ context.Context, messages []llm.Message, _ ...llm.Option) (*llm.Message, error) {
	model.mu.Lock()
	model.requests = append(model.requests, append([]llm.Message(nil), messages...))
	model.mu.Unlock()
	return &llm.Message{Role: llm.RoleAssistant, Content: "done"}, nil
}

func (model *budgetPreflightRecordingModel) Stream(ctx context.Context, messages []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	response, err := model.Generate(ctx, messages, opts...)
	if err != nil {
		return nil, err
	}
	chunks := make(chan llm.MessageChunk, 2)
	if response.Content != "" {
		chunks <- llm.MessageChunk{Role: response.Role, Content: response.Content}
	}
	chunks <- llm.MessageChunk{Role: response.Role, Final: true}
	close(chunks)
	return chunks, nil
}

func (model *budgetPreflightRecordingModel) requestsSnapshot() [][]llm.Message {
	model.mu.Lock()
	defer model.mu.Unlock()
	requests := make([][]llm.Message, len(model.requests))
	for i := range model.requests {
		requests[i] = append([]llm.Message(nil), model.requests[i]...)
	}
	return requests
}

type budgetPreflightRewriteCompactor struct {
	rewrite func([]compaction.TurnItem) []compaction.TurnItem
}

type budgetPreflightInPlaceBlockCompactor struct {
	calls int
}

func (compactor *budgetPreflightInPlaceBlockCompactor) Compact(_ context.Context, items []compaction.TurnItem, _ int, _ compaction.TokenEstimator) (compaction.CompactionResult, error) {
	compactor.calls++
	items[len(items)-1].ContentBlocks[0].Text = "rewritten current block"
	return compaction.CompactionResult{Items: items, Strategy: compaction.StrategyTruncating}, nil
}

func (compactor budgetPreflightRewriteCompactor) Compact(_ context.Context, items []compaction.TurnItem, _ int, _ compaction.TokenEstimator) (compaction.CompactionResult, error) {
	return compaction.CompactionResult{
		Items:    compactor.rewrite(items),
		Strategy: compaction.StrategyTruncating,
	}, nil
}

func requestText(messages []llm.Message) string {
	var text strings.Builder
	for _, message := range messages {
		text.WriteString(message.Content)
	}
	return text.String()
}
