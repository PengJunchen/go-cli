package memory

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
)

func TestExtract_EmptyMessages(t *testing.T) {
	server := mock.NewMockLLMServer(nil)
	store, _ := newTestStore(t)
	extractor := NewLLMMemoryExtractor(server, store)

	// nil slice.
	mems, err := extractor.Extract(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, mems)

	// Empty slice.
	mems, err = extractor.Extract(context.Background(), []llm.Message{})
	require.NoError(t, err)
	assert.Empty(t, mems)

	// The model should not have been called.
	assert.Equal(t, 0, server.CallCount())
}

func TestExtract_SingleMessage(t *testing.T) {
	server := mock.NewMockLLMServer(nil)
	store, _ := newTestStore(t)
	extractor := NewLLMMemoryExtractor(server, store)

	mems, err := extractor.Extract(context.Background(), []llm.Message{
		{Role: llm.RoleUser, Content: "hello"},
	})
	require.NoError(t, err)
	assert.Empty(t, mems)

	// The model should not have been called.
	assert.Equal(t, 0, server.CallCount())
}

func TestExtract_NormalConversation(t *testing.T) {
	jsonResponse := `[{"content":"User prefers dark mode","category":"preference"},{"content":"Project uses Go 1.24","category":"convention"}]`
	server := mock.NewMockLLMServer(mock.NewConversationTemplate("T", "extract",
		mock.ConversationTurn{AssistantContent: jsonResponse},
	))
	store, _ := newTestStore(t)
	extractor := NewLLMMemoryExtractor(server, store)

	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "I like dark mode for my editor."},
		{Role: llm.RoleAssistant, Content: "Got it, I'll use dark mode."},
	}

	mems, err := extractor.Extract(context.Background(), msgs)
	require.NoError(t, err)
	require.Len(t, mems, 2)

	assert.Equal(t, "User prefers dark mode", mems[0].Content)
	assert.Equal(t, "preference", mems[0].Category)
	assert.Equal(t, "auto", mems[0].Source)

	assert.Equal(t, "Project uses Go 1.24", mems[1].Content)
	assert.Equal(t, "convention", mems[1].Category)
	assert.Equal(t, "auto", mems[1].Source)

	// The model should have been called exactly once.
	assert.Equal(t, 1, server.CallCount())
}

func TestExtract_NormalConversation_DoesNotWriteToStore(t *testing.T) {
	jsonResponse := `[{"content":"User prefers dark mode","category":"preference"}]`
	server := mock.NewMockLLMServer(mock.NewConversationTemplate("T", "no-write",
		mock.ConversationTurn{AssistantContent: jsonResponse},
	))
	store, _ := newTestStore(t)
	ctx := context.Background()
	extractor := NewLLMMemoryExtractor(server, store)

	before, err := store.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, before)

	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "hi"},
		{Role: llm.RoleAssistant, Content: "hello"},
	}

	mems, err := extractor.Extract(ctx, msgs)
	require.NoError(t, err)
	require.Len(t, mems, 1)

	// Store should remain empty — Extract does not write.
	after, err := store.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, after)
}

func TestExtract_JSONParseFailure(t *testing.T) {
	server := mock.NewMockLLMServer(mock.NewConversationTemplate("T", "bad-json",
		mock.ConversationTurn{AssistantContent: "This is not JSON at all"},
	))
	store, _ := newTestStore(t)
	extractor := NewLLMMemoryExtractor(server, store)

	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "hi"},
		{Role: llm.RoleAssistant, Content: "hello"},
	}

	mems, err := extractor.Extract(context.Background(), msgs)
	require.NoError(t, err)
	assert.Empty(t, mems)
}

func TestExtract_EmptyResponse(t *testing.T) {
	server := mock.NewMockLLMServer(mock.NewConversationTemplate("T", "empty",
		mock.ConversationTurn{AssistantContent: ""},
	))
	store, _ := newTestStore(t)
	extractor := NewLLMMemoryExtractor(server, store)

	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "hi"},
		{Role: llm.RoleAssistant, Content: "hello"},
	}

	mems, err := extractor.Extract(context.Background(), msgs)
	require.NoError(t, err)
	assert.Empty(t, mems)
}

func TestExtract_EmptyJSONArray(t *testing.T) {
	server := mock.NewMockLLMServer(mock.NewConversationTemplate("T", "empty-array",
		mock.ConversationTurn{AssistantContent: "[]"},
	))
	store, _ := newTestStore(t)
	extractor := NewLLMMemoryExtractor(server, store)

	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "hi"},
		{Role: llm.RoleAssistant, Content: "hello"},
	}

	mems, err := extractor.Extract(context.Background(), msgs)
	require.NoError(t, err)
	assert.Empty(t, mems)
}

func TestExtract_Deduplication(t *testing.T) {
	jsonResponse := `[{"content":"User prefers dark mode","category":"preference"},{"content":"Project uses Go 1.24","category":"convention"}]`
	server := mock.NewMockLLMServer(mock.NewConversationTemplate("T", "dedup",
		mock.ConversationTurn{AssistantContent: jsonResponse},
	))
	store, _ := newTestStore(t)
	ctx := context.Background()

	// Pre-populate the store with a memory whose content exactly matches one
	// of the extracted facts.
	require.NoError(t, store.Add(ctx, Memory{
		Content:  "User prefers dark mode",
		Category: "preference",
		Source:   "manual",
	}))

	extractor := NewLLMMemoryExtractor(server, store)
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "I like dark mode."},
		{Role: llm.RoleAssistant, Content: "Noted."},
	}

	mems, err := extractor.Extract(ctx, msgs)
	require.NoError(t, err)
	require.Len(t, mems, 1)

	// The duplicate should be filtered out; only the new fact remains.
	assert.Equal(t, "Project uses Go 1.24", mems[0].Content)
	assert.Equal(t, "convention", mems[0].Category)
}

func TestExtract_Deduplication_WithinExtractedFacts(t *testing.T) {
	// The model returns the same fact twice; only one should survive.
	jsonResponse := `[{"content":"User prefers tabs","category":"preference"},{"content":"User prefers tabs","category":"preference"}]`
	server := mock.NewMockLLMServer(mock.NewConversationTemplate("T", "self-dedup",
		mock.ConversationTurn{AssistantContent: jsonResponse},
	))
	store, _ := newTestStore(t)
	extractor := NewLLMMemoryExtractor(server, store)

	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "hi"},
		{Role: llm.RoleAssistant, Content: "hello"},
	}

	mems, err := extractor.Extract(context.Background(), msgs)
	require.NoError(t, err)
	require.Len(t, mems, 1)
	assert.Equal(t, "User prefers tabs", mems[0].Content)
}

func TestExtract_SkipsEmptyContent(t *testing.T) {
	jsonResponse := `[{"content":"","category":"fact"},{"content":"Real fact","category":"fact"}]`
	server := mock.NewMockLLMServer(mock.NewConversationTemplate("T", "skip-empty",
		mock.ConversationTurn{AssistantContent: jsonResponse},
	))
	store, _ := newTestStore(t)
	extractor := NewLLMMemoryExtractor(server, store)

	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "hi"},
		{Role: llm.RoleAssistant, Content: "hello"},
	}

	mems, err := extractor.Extract(context.Background(), msgs)
	require.NoError(t, err)
	require.Len(t, mems, 1)
	assert.Equal(t, "Real fact", mems[0].Content)
}

func TestExtract_ModelError(t *testing.T) {
	server := mock.NewMockLLMServer(mock.NewConversationTemplate("T", "err",
		mock.ConversationTurn{AssistantError: "model unavailable"},
	))
	store, _ := newTestStore(t)
	extractor := NewLLMMemoryExtractor(server, store)

	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "hi"},
		{Role: llm.RoleAssistant, Content: "hello"},
	}

	mems, err := extractor.Extract(context.Background(), msgs)
	require.Error(t, err)
	assert.Empty(t, mems)
}

func TestExtract_PromptContainsConversation(t *testing.T) {
	jsonResponse := `[]`
	server := mock.NewMockLLMServer(mock.NewConversationTemplate("T", "prompt-check",
		mock.ConversationTurn{AssistantContent: jsonResponse},
	))
	store, _ := newTestStore(t)
	extractor := NewLLMMemoryExtractor(server, store)

	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "always use tabs not spaces"},
		{Role: llm.RoleAssistant, Content: "understood, tabs it is"},
	}

	_, err := extractor.Extract(context.Background(), msgs)
	require.NoError(t, err)

	// Verify the model received a prompt containing the conversation text.
	log := server.CallLog()
	require.Len(t, log, 1)
	require.Len(t, log[0].Messages, 1)
	assert.Contains(t, log[0].Messages[0].Content, "always use tabs not spaces")
	assert.Contains(t, log[0].Messages[0].Content, "understood, tabs it is")
}
