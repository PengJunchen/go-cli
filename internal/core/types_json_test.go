package core

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestAgentEventJSON(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	now := time.Now().UTC().Truncate(time.Millisecond) // truncate to avoid JSON precision loss

	original := AgentEvent{
		Kind:        "message",
		Content:     "hello world",
		Timestamp:   now,
		Incremental: true,
		TokenUsage: &TokenUsage{
			InputTokens:  100,
			OutputTokens: 50,
			MaxTokens:    4096,
			Cost:         0.015,
		},
		ToolCallID: "call-123",
		Stream:     "stdout",
		Usage: &llm.Usage{
			InputTokens:  100,
			OutputTokens: 50,
			TotalTokens:  150,
		},
		ToolCalls: []llm.ToolCall{
			{ID: "tc-1", Name: "read_file", Args: map[string]any{"path": "/tmp/test"}},
		},
		IsError: false,
	}

	data, err := json.Marshal(original)
	require.NoError(t, err)

	var decoded AgentEvent
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, original.Kind, decoded.Kind)
	assert.Equal(t, original.Content, decoded.Content)
	assert.True(t, original.Timestamp.Equal(decoded.Timestamp))
	assert.Equal(t, original.Incremental, decoded.Incremental)
	assert.Equal(t, original.ToolCallID, decoded.ToolCallID)
	assert.Equal(t, original.Stream, decoded.Stream)
	assert.Equal(t, original.IsError, decoded.IsError)

	require.NotNil(t, decoded.TokenUsage)
	assert.Equal(t, *original.TokenUsage, *decoded.TokenUsage)

	require.NotNil(t, decoded.Usage)
	assert.Equal(t, *original.Usage, *decoded.Usage)

	require.Len(t, decoded.ToolCalls, 1)
	assert.Equal(t, original.ToolCalls[0].ID, decoded.ToolCalls[0].ID)
	assert.Equal(t, original.ToolCalls[0].Name, decoded.ToolCalls[0].Name)
}

func TestTokenUsageJSON(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	original := TokenUsage{
		InputTokens:  500,
		OutputTokens: 200,
		MaxTokens:    8192,
		Cost:         0.05,
	}

	data, err := json.Marshal(original)
	require.NoError(t, err)

	var decoded TokenUsage
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, original, decoded)
}

func TestAgentEventJSONOmitEmpty(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// An event with only required fields should omit optional fields.
	evt := AgentEvent{Kind: "done", Content: "finished", Timestamp: time.Now()}
	data, err := json.Marshal(evt)
	require.NoError(t, err)

	var decoded AgentEvent
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, "done", decoded.Kind)
	assert.Equal(t, "finished", decoded.Content)
	assert.Nil(t, decoded.TokenUsage)
	assert.Nil(t, decoded.Usage)
	assert.Nil(t, decoded.ToolCalls)
	assert.False(t, decoded.Incremental)
	assert.False(t, decoded.IsError)
}
