package core

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// usageStreamModel is a minimal BaseChatModel whose Stream returns a fixed set
// of chunks, including a final chunk carrying API-reported Usage.
type usageStreamModel struct {
	chunks []llm.MessageChunk
}

func (m *usageStreamModel) Generate(_ context.Context, _ []llm.Message, _ ...llm.Option) (*llm.Message, error) {
	return nil, errors.New("not implemented")
}

func (m *usageStreamModel) Stream(_ context.Context, _ []llm.Message, _ ...llm.Option) (<-chan llm.MessageChunk, error) {
	ch := make(chan llm.MessageChunk, len(m.chunks))
	for _, c := range m.chunks {
		ch <- c
	}
	close(ch)
	return ch, nil
}

var _ llm.BaseChatModel = (*usageStreamModel)(nil)

// TestStreamGenerate_PropagatesUsageToMessage verifies that streamGenerate
// captures Usage from the stream's final chunk and sets it on the returned
// Message.
func TestStreamGenerate_PropagatesUsageToMessage(t *testing.T) {
	model := &usageStreamModel{
		chunks: []llm.MessageChunk{
			{Role: llm.RoleAssistant, Content: "hello"},
			{
				Role:         llm.RoleAssistant,
				Final:        true,
				FinishReason: "stop",
				Usage: &llm.Usage{
					InputTokens:  42,
					OutputTokens: 13,
					TotalTokens:  55,
				},
			},
		},
	}

	loop := NewLoopAgent(WithLLM(model))

	msg, err := loop.streamGenerate(context.Background(), model, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, msg)

	assert.Equal(t, "hello", msg.Content)
	assert.Equal(t, "stop", msg.FinishReason)
	require.NotNil(t, msg.Usage, "Message.Usage should be set from chunk.Usage")
	assert.Equal(t, 42, msg.Usage.InputTokens)
	assert.Equal(t, 13, msg.Usage.OutputTokens)
	assert.Equal(t, 55, msg.Usage.TotalTokens)
}

// TestStreamGenerate_NoUsageWhenNotProvided verifies that when the stream does
// not include Usage, the returned Message.Usage remains nil.
func TestStreamGenerate_NoUsageWhenNotProvided(t *testing.T) {
	model := &usageStreamModel{
		chunks: []llm.MessageChunk{
			{Role: llm.RoleAssistant, Content: "hello"},
			{Role: llm.RoleAssistant, Final: true, FinishReason: "stop"},
		},
	}

	loop := NewLoopAgent(WithLLM(model))

	msg, err := loop.streamGenerate(context.Background(), model, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, msg)
	assert.Nil(t, msg.Usage, "Usage should be nil when stream has no usage")
}
