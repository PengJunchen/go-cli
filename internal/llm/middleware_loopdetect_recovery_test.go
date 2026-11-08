package llm

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRepeatedResponseErrorRequiresCompleteSingleToolCall(t *testing.T) {
	for _, tc := range []struct {
		name         string
		finish       string
		invalidArgs  bool
		multiple     bool
		wantEligible bool
	}{
		{name: "complete", finish: "tool_calls", wantEligible: true},
		{name: "truncated", finish: "length"},
		{name: "multiple calls", finish: "tool_calls", multiple: true},
		{name: "invalid arguments", finish: "tool_calls", invalidArgs: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			inner := &mockModel{generateFn: func(context.Context, []Message, ...Option) (*Message, error) {
				calls++
				toolCall := ToolCall{ID: fmt.Sprintf("call-%d", calls), Name: "edit", Args: map[string]any{"file_path": "sample.txt", "old_string": "missing", "new_string": "new"}}
				if tc.invalidArgs {
					toolCall.Args = "invalid"
				}
				toolCalls := []ToolCall{toolCall}
				if tc.multiple {
					toolCalls = append(toolCalls, ToolCall{ID: "read", Name: "read", Args: map[string]any{"path": "sample.txt"}})
				}
				return &Message{Role: RoleAssistant, FinishReason: tc.finish, ToolCalls: toolCalls}, nil
			}}
			model := NewLoopDetectionModelMiddleware().WrapModel(inner)
			var repeated *RepeatedResponseError
			for i := 0; i < 3; i++ {
				response, err := model.Generate(context.Background(), nil)
				if i < 2 {
					require.NoError(t, err)
					require.NotNil(t, response)
					continue
				}
				require.ErrorAs(t, err, &repeated)
				require.Nil(t, response)
			}
			prior := []ToolCall{{ID: "previous-call", Name: "edit", Args: map[string]any{"file_path": "sample.txt", "old_string": "missing", "new_string": "new"}}}
			if tc.invalidArgs {
				prior[0].Args = "invalid"
			}
			if tc.multiple {
				prior = append(prior, ToolCall{ID: "read", Name: "read", Args: map[string]any{"path": "sample.txt"}})
			}
			require.Equal(t, tc.wantEligible, repeated.MatchesResponse("", prior))
			if tc.wantEligible {
				prior[0].Args = map[string]any{"file_path": "sample.txt", "old_string": "different", "new_string": "new"}
				require.False(t, repeated.MatchesResponse("", prior))
			}
		})
	}
}

func TestRepeatedResponseErrorSurvivesStreamFinalChunk(t *testing.T) {
	var calls int
	inner := &mockModel{streamFn: func(context.Context, []Message, ...Option) (<-chan MessageChunk, error) {
		calls++
		ch := make(chan MessageChunk, 1)
		ch <- MessageChunk{Role: RoleAssistant, Final: true, FinishReason: "tool_calls", ToolCalls: []ToolCall{{ID: fmt.Sprintf("call-%d", calls), Name: "edit", Args: map[string]any{"file_path": "sample.txt", "old_string": "missing", "new_string": "new"}}}}
		close(ch)
		return ch, nil
	}}
	model := NewLoopDetectionModelMiddleware().WrapModel(inner)
	var finalErr *RepeatedResponseError
	for i := 0; i < 3; i++ {
		stream, err := model.Stream(context.Background(), nil)
		require.NoError(t, err)
		for chunk := range stream {
			if chunk.Error != nil {
				require.ErrorAs(t, chunk.Error, &finalErr)
			}
		}
	}
	require.NotNil(t, finalErr)
	require.True(t, finalErr.MatchesResponse("", []ToolCall{{ID: "different-call-id", Name: "edit", Args: map[string]any{"file_path": "sample.txt", "old_string": "missing", "new_string": "new"}}}))
}
