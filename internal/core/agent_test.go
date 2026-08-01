package core

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/mock"
)

func testAgent(t *testing.T) *AgentImpl {
	t.Helper()
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"A-01", "agent",
		mock.ConversationTurn{AssistantContent: "hi there"},
	))
	loop := NewLoopAgent(WithLLM(model))
	return NewAgentImpl("test-agent", loop)
}

func TestAgentName(t *testing.T) {
	a := testAgent(t)
	assert.Equal(t, "test-agent", a.Name())
}

func TestAgentDefaultName(t *testing.T) {
	loop := NewLoopAgent()
	a := NewAgentImpl("", loop)
	assert.Equal(t, "default", a.Name())
}

func TestAgentRunSuccess(t *testing.T) {
	a := testAgent(t)
	res, err := a.Run(context.Background(), Submission{Type: SubmissionUserMessage, Content: "hello"})
	require.NoError(t, err)
	assert.True(t, res.Success)
	assert.Equal(t, "hi there", res.Message)

	// History now holds the user message and the assistant reply.
	msgs := a.Messages()
	require.Len(t, msgs, 2)
	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "hello", msgs[0].Content)
	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Equal(t, "hi there", msgs[1].Content)
}

func TestAgentRunRecordsEvents(t *testing.T) {
	a := testAgent(t)
	_, err := a.Run(context.Background(), Submission{Content: "hi"})
	require.NoError(t, err)

	events := a.Events()
	msgs := findEvents(events, "message")
	require.Len(t, msgs, 1)
	assert.Equal(t, "hi there", msgs[0])
}

func TestAgentWithHistory(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"A-02", "history",
		mock.ConversationTurn{AssistantContent: "ok"},
	))
	loop := NewLoopAgent(WithLLM(model))
	a := NewAgentImpl("hist", loop, WithHistory([]AgentMessage{
		{Role: "system", Content: "be terse"},
	}))
	assert.Len(t, a.Messages(), 1)

	_, err := a.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)
	// system + user + assistant
	assert.Len(t, a.Messages(), 3)
}

func TestAgentNaNilLoopPanics(t *testing.T) {
	assert.Panics(t, func() {
		NewAgentImpl("a", nil)
	})
}

func TestAgentRunConcurrencySafe(t *testing.T) {
	a := testAgent(t)

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Run(context.Background(), Submission{Content: "q"}); err != nil {
				t.Errorf("unexpected run error: %v", err)
			}
		}()
	}
	wg.Wait()

	// History grew; no race detected by the -race detector.
	assert.GreaterOrEqual(t, len(a.Messages()), 2)
}
