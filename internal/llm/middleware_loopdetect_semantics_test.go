package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// semanticLoopModel returns equivalent response messages through each public
// model seam. Stream puts the complete, framework-merged tool calls on the
// final chunk, matching the BaseChatModel contract.
func semanticLoopModel(responses []Message) *mockModel {
	var generateIndex int
	var streamIndex int
	return &mockModel{
		generateFn: func(context.Context, []Message, ...Option) (*Message, error) {
			response := responses[generateIndex]
			generateIndex++
			return &response, nil
		},
		streamFn: func(context.Context, []Message, ...Option) (<-chan MessageChunk, error) {
			response := responses[streamIndex]
			streamIndex++
			ch := make(chan MessageChunk, 3)
			if response.Content != "" {
				mid := len(response.Content) / 2
				ch <- MessageChunk{Role: RoleAssistant, Content: response.Content[:mid]}
				ch <- MessageChunk{Role: RoleAssistant, Content: response.Content[mid:]}
			}
			ch <- MessageChunk{
				Role:      RoleAssistant,
				Final:     true,
				ToolCalls: response.ToolCalls,
			}
			close(ch)
			return ch, nil
		},
	}
}

func receiveLoopStream(t *testing.T, model BaseChatModel) error {
	t.Helper()
	ch, err := model.Stream(context.Background(), nil)
	require.NoError(t, err)
	for chunk := range ch {
		if chunk.Error != nil {
			return chunk.Error
		}
	}
	return nil
}

func TestLoopDetectGenerateDistinguishesEmptyToolResponses(t *testing.T) {
	responses := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-a", Name: "ls", Args: map[string]any{"path": "."}}}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-b", Name: "grep", Args: map[string]any{"pattern": "ALPHA"}}}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-c", Name: "read", Args: map[string]any{"path": "a.txt"}}}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-d", Name: "read", Args: map[string]any{"path": "b.txt"}}}},
	}
	model := NewLoopDetectionModelMiddleware().WrapModel(semanticLoopModel(responses))

	for range responses {
		_, err := model.Generate(context.Background(), nil)
		require.NoError(t, err, "distinct empty-content tool calls are distinct responses")
	}
}

func TestLoopDetectStreamDistinguishesEmptyToolResponses(t *testing.T) {
	responses := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-a", Name: "ls", Args: map[string]any{"path": "."}}}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-b", Name: "grep", Args: map[string]any{"pattern": "ALPHA"}}}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-c", Name: "read", Args: map[string]any{"path": "a.txt"}}}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-d", Name: "read", Args: map[string]any{"path": "b.txt"}}}},
	}
	model := NewLoopDetectionModelMiddleware().WrapModel(semanticLoopModel(responses))

	for range responses {
		require.NoError(t, receiveLoopStream(t, model), "distinct empty-content tool calls are distinct responses")
	}
}

func TestLoopDetectGenerateIgnoresToolCallIDsButComparesNameAndArgs(t *testing.T) {
	responses := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-random-1", Name: "read", Args: map[string]any{"path": "same.txt", "limit": 1}}}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-random-2", Name: "read", Args: map[string]any{"limit": 1, "path": "same.txt"}}}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-random-3", Name: "read", Args: map[string]any{"path": "same.txt", "limit": 1}}}},
	}
	model := NewLoopDetectionModelMiddleware().WrapModel(semanticLoopModel(responses))

	for range responses[:2] {
		_, err := model.Generate(context.Background(), nil)
		require.NoError(t, err)
	}
	_, err := model.Generate(context.Background(), nil)
	require.ErrorContains(t, err, "count=3")
}

func TestLoopDetectStreamIgnoresToolCallIDsButComparesNameAndArgs(t *testing.T) {
	responses := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-random-1", Name: "read", Args: map[string]any{"path": "same.txt", "limit": 1}}}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-random-2", Name: "read", Args: map[string]any{"limit": 1, "path": "same.txt"}}}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-random-3", Name: "read", Args: map[string]any{"path": "same.txt", "limit": 1}}}},
	}
	model := NewLoopDetectionModelMiddleware().WrapModel(semanticLoopModel(responses))

	for range responses[:2] {
		require.NoError(t, receiveLoopStream(t, model))
	}
	require.ErrorContains(t, receiveLoopStream(t, model), "count=3")
}

func TestLoopDetectGenerateDistinguishesToolsWithRepeatedText(t *testing.T) {
	responses := []Message{
		{Role: RoleAssistant, Content: "I am checking the workspace.", ToolCalls: []ToolCall{{ID: "call-a", Name: "ls", Args: map[string]any{"path": "."}}}},
		{Role: RoleAssistant, Content: "I am checking the workspace.", ToolCalls: []ToolCall{{ID: "call-b", Name: "grep", Args: map[string]any{"pattern": "ALPHA"}}}},
		{Role: RoleAssistant, Content: "I am checking the workspace.", ToolCalls: []ToolCall{{ID: "call-c", Name: "read", Args: map[string]any{"path": "a.txt"}}}},
	}
	model := NewLoopDetectionModelMiddleware().WrapModel(semanticLoopModel(responses))

	for range responses {
		_, err := model.Generate(context.Background(), nil)
		require.NoError(t, err, "same prose with different actions is not the same response")
	}
}

func TestLoopDetectStreamDistinguishesToolsWithRepeatedText(t *testing.T) {
	responses := []Message{
		{Role: RoleAssistant, Content: "I am checking the workspace.", ToolCalls: []ToolCall{{ID: "call-a", Name: "ls", Args: map[string]any{"path": "."}}}},
		{Role: RoleAssistant, Content: "I am checking the workspace.", ToolCalls: []ToolCall{{ID: "call-b", Name: "grep", Args: map[string]any{"pattern": "ALPHA"}}}},
		{Role: RoleAssistant, Content: "I am checking the workspace.", ToolCalls: []ToolCall{{ID: "call-c", Name: "read", Args: map[string]any{"path": "a.txt"}}}},
	}
	model := NewLoopDetectionModelMiddleware().WrapModel(semanticLoopModel(responses))

	for range responses {
		require.NoError(t, receiveLoopStream(t, model), "same prose with different actions is not the same response")
	}
}

func TestLoopDetectBoundsToolCallPayloadForGenerateAndStream(t *testing.T) {
	responses := []Message{{
		Role: RoleAssistant,
		ToolCalls: []ToolCall{{
			ID:   "call-large",
			Name: "write",
			Args: map[string]any{"content": strings.Repeat("x", GuardedStreamMaxBytes)},
		}},
	}}

	generate := NewLoopDetectionModelMiddleware().WrapModel(semanticLoopModel(responses))
	_, err := generate.Generate(context.Background(), nil)
	require.ErrorContains(t, err, "candidate limit")

	stream := NewLoopDetectionModelMiddleware().WrapModel(semanticLoopModel(responses))
	require.ErrorContains(t, receiveLoopStream(t, stream), "candidate limit")
}
