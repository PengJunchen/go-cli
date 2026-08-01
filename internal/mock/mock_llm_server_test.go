package mock

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestMockLLMServerMultiTurnSequential(t *testing.T) {
	server := NewMockLLMServer(NewConversationTemplate("T", "multi",
		ConversationTurn{AssistantContent: "first"},
		ConversationTurn{AssistantContent: "second"},
	))

	model, cleanup, err := server.Build(context.Background(), llm.ModelConfig{Model: "mock"})
	require.NoError(t, err)
	defer cleanup()

	ctx := context.Background()
	resp1, err := model.Generate(ctx, []llm.Message{{Role: llm.RoleUser, Content: "hi"}})
	require.NoError(t, err)
	assert.Equal(t, llm.RoleAssistant, resp1.Role)
	assert.Equal(t, "first", resp1.Content)

	resp2, err := model.Generate(ctx, []llm.Message{{Role: llm.RoleUser, Content: "hi"}})
	require.NoError(t, err)
	assert.Equal(t, "second", resp2.Content)

	assert.Equal(t, 2, server.CallCount())
}

func TestMockLLMServerToolCallsInTurn(t *testing.T) {
	server := NewMockLLMServer(NewConversationTemplate("T", "tools",
		ConversationTurn{AssistantToolCalls: []ExpectedToolCall{
			{ID: "c1", Name: "read_file", Args: map[string]any{"path": "/a"}},
		}},
	))

	resp, err := server.Generate(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, resp.ToolCalls, 1)
	assert.Equal(t, "c1", resp.ToolCalls[0].ID)
	assert.Equal(t, "read_file", resp.ToolCalls[0].Name)
}

func TestMockLLMServerOutOfRangeFallback(t *testing.T) {
	server := NewMockLLMServer(NewConversationTemplate("T", "one",
		ConversationTurn{AssistantContent: "only"},
	))

	// First call returns the configured turn.
	resp, err := server.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "only", resp.Content)

	// Out-of-range calls return the default fallback content and no error.
	resp, err = server.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, fallbackDefaultContent, resp.Content)
	assert.Equal(t, llm.RoleAssistant, resp.Role)
}

func TestMockLLMServerOverridableFallback(t *testing.T) {
	server := NewMockLLMServer(NewConversationTemplate("T", "empty"))
	server.SetFallbackContent("done")

	resp, err := server.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "done", resp.Content)
}

func TestMockLLMServerCallLogRecordsMessages(t *testing.T) {
	server := NewMockLLMServer(NewConversationTemplate("T", "log",
		ConversationTurn{AssistantContent: "reply"},
	))

	msgs := []llm.Message{{Role: llm.RoleUser, Content: "hello"}}
	_, err := server.Generate(context.Background(), msgs)
	require.NoError(t, err)

	log := server.CallLog()
	require.Len(t, log, 1)
	assert.Equal(t, 0, log[0].Index)
	assert.Equal(t, msgs, log[0].Messages)
	require.NotNil(t, log[0].Response)
	assert.Equal(t, "reply", log[0].Response.Content)
	assert.Nil(t, log[0].Error)
}

func TestMockLLMServerSimulatedError(t *testing.T) {
	server := NewMockLLMServer(NewConversationTemplate("T", "err",
		ConversationTurn{AssistantError: "rate_limit"},
	))

	resp, err := server.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.Nil(t, resp)

	log := server.CallLog()
	require.Len(t, log, 1)
	assert.Error(t, log[0].Error)
}

func TestMockLLMServerStreamClosesChannel(t *testing.T) {
	server := NewMockLLMServer(NewConversationTemplate("T", "stream",
		ConversationTurn{AssistantContent: "streamed"},
	))

	ch, err := server.Stream(context.Background(), nil)
	require.NoError(t, err)

	var chunks []llm.MessageChunk
	for c := range ch {
		chunks = append(chunks, c)
	}
	require.Len(t, chunks, 1)
	assert.Equal(t, "streamed", chunks[0].Content)
}

func TestMockLLMServerReset(t *testing.T) {
	server := NewMockLLMServer(NewConversationTemplate("T", "reset",
		ConversationTurn{AssistantContent: "a"},
		ConversationTurn{AssistantContent: "b"},
	))

	_, err := server.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, server.CallCount())

	server.Reset()
	assert.Equal(t, 0, server.CallCount())

	// After reset, index restarts at 0.
	resp, err := server.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "a", resp.Content)
}

func TestMockLLMServerConcurrencySafe(t *testing.T) {
	server := NewMockLLMServer(NewConversationTemplate("T", "conc",
		ConversationTurn{AssistantContent: "x"},
		ConversationTurn{AssistantContent: "y"},
		ConversationTurn{AssistantContent: "z"},
	))

	const workers = 32
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := server.Generate(context.Background(), nil)
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("unexpected generate error: %v", err)
	}

	assert.Equal(t, workers, server.CallCount())

	// CallLog remains consistent under the record lock.
	log := server.CallLog()
	assert.Len(t, log, workers)
}

func TestMockLLMServerCancelledContext(t *testing.T) {
	server := NewMockLLMServer(NewConversationTemplate("T", "ctx"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := server.Generate(ctx, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}
