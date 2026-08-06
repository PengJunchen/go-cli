package llm

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessageFinishReasonSerialization(t *testing.T) {
	// FinishReason is included when non-empty.
	msg := Message{
		Role:         RoleAssistant,
		Content:      "hello",
		FinishReason: "length",
	}
	data, err := json.Marshal(msg)
	require.NoError(t, err)

	var decoded Message
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, "length", decoded.FinishReason)
	assert.Equal(t, "hello", decoded.Content)
	assert.Equal(t, RoleAssistant, decoded.Role)
}

func TestMessageFinishReasonOmitempty(t *testing.T) {
	// FinishReason is omitted when empty (omitempty).
	msg := Message{Role: RoleAssistant, Content: "hello"}
	data, err := json.Marshal(msg)
	require.NoError(t, err)

	// The JSON should not contain the finish_reason field.
	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	_, exists := raw["finish_reason"]
	assert.False(t, exists)

	// Decoding back leaves FinishReason empty.
	var decoded Message
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Empty(t, decoded.FinishReason)
}

func TestMessageFinishReasonRoundTrip(t *testing.T) {
	cases := []string{"stop", "length", "tool_calls", "content_filter"}
	for _, reason := range cases {
		t.Run(reason, func(t *testing.T) {
			msg := Message{
				Role:         RoleAssistant,
				Content:      "test",
				FinishReason: reason,
			}
			data, err := json.Marshal(msg)
			require.NoError(t, err)

			var decoded Message
			require.NoError(t, json.Unmarshal(data, &decoded))
			assert.Equal(t, reason, decoded.FinishReason)
		})
	}
}

func TestMessageChunkFinishReasonSerialization(t *testing.T) {
	// The Final chunk carries FinishReason.
	chunk := MessageChunk{
		Role:         RoleAssistant,
		Final:        true,
		FinishReason: "length",
	}
	data, err := json.Marshal(chunk)
	require.NoError(t, err)

	var decoded MessageChunk
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.True(t, decoded.Final)
	assert.Equal(t, "length", decoded.FinishReason)
}

func TestMessageChunkFinishReasonOmitempty(t *testing.T) {
	chunk := MessageChunk{Role: RoleAssistant, Content: "text"}
	data, err := json.Marshal(chunk)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	_, exists := raw["finish_reason"]
	assert.False(t, exists)
}
