//go:build e2e

// Package e2e_20260802 contains end-to-end integration tests.
// This file verifies Phase 20 SSE Streaming: DefaultSSEParser, and
// nativeChatModel.Stream for Claude, OpenAI, and Gemini providers using
// httptest.Server mock endpoints.
package e2e_20260802 //nolint:staticcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// =============================================================================
// Shared helpers
// =============================================================================

// phase20SSEEvent formats a single SSE event with an event type and data line.
func phase20SSEEvent(eventType, data string) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, data)
}

// phase20SSEData formats a single SSE event with only a data line (no event type).
func phase20SSEData(data string) string {
	return fmt.Sprintf("data: %s\n\n", data)
}

// phase20SSEServer creates an httptest.Server that responds with the given SSE
// body and Content-Type text/event-stream.
func phase20SSEServer(t *testing.T, sseBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, sseBody)
	}))
}

// phase20ErrorServer creates an httptest.Server that responds with a non-200
// status code and an error body.
func phase20ErrorServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
}

// phase20CollectChunks drains a MessageChunk channel into a slice.
func phase20CollectChunks(t *testing.T, ch <-chan llm.MessageChunk) []llm.MessageChunk {
	t.Helper()
	var chunks []llm.MessageChunk
	for c := range ch {
		chunks = append(chunks, c)
	}
	return chunks
}

// phase20StreamCtx returns a context with a 10-second timeout for streaming tests.
func phase20StreamCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// =============================================================================
// AC-1: DefaultSSEParser.Parse returns events with correct Type and Data
// =============================================================================

// TestET_Phase20_Streaming_AC1_SSEParser verifies that DefaultSSEParser.Parse
// correctly parses SSE events from a reader, returning events with the correct
// Type and Data fields via a channel.
func TestET_Phase20_Streaming_AC1_SSEParser(t *testing.T) {
	parser := llm.NewDefaultSSEParser()

	input := phase20SSEEvent("message_start", `{"type":"message_start"}`) +
		phase20SSEEvent("content_block_delta", `{"type":"content_block_delta"}`) +
		phase20SSEData(`{"choices":[]}`)

	ch, err := parser.Parse(strings.NewReader(input))
	require.NoError(t, err)

	var events []llm.SSEEvent
	for e := range ch {
		events = append(events, e)
	}

	require.Len(t, events, 3, "parser must emit 3 events")
	assert.Equal(t, "message_start", events[0].Type)
	assert.Contains(t, events[0].Data, "message_start")
	assert.Equal(t, "content_block_delta", events[1].Type)
	assert.Contains(t, events[1].Data, "content_block_delta")
	// Third event has no event type (OpenAI-style data-only event).
	assert.Equal(t, "", events[2].Type)
	assert.Contains(t, events[2].Data, "choices")
}

// =============================================================================
// AC-2: Claude streaming returns multiple MessageChunks
// =============================================================================

// TestET_Phase20_Streaming_AC2_ClaudeStream verifies that streaming against a
// mock Claude SSE endpoint returns multiple MessageChunks (not just one) with
// the correct content and a Final chunk.
func TestET_Phase20_Streaming_AC2_ClaudeStream(t *testing.T) {
	// Build Claude SSE events: message_start, two text deltas, message_stop.
	msgStart, _ := json.Marshal(map[string]any{
		"type":    "message_start",
		"message": map[string]any{"role": "assistant"},
	})
	delta1, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"delta": map[string]any{"type": "text_delta", "text": "Hello"},
	})
	delta2, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"delta": map[string]any{"type": "text_delta", "text": " world"},
	})
	msgStop, _ := json.Marshal(map[string]any{"type": "message_stop"})

	sseBody := phase20SSEEvent("message_start", string(msgStart)) +
		phase20SSEEvent("content_block_delta", string(delta1)) +
		phase20SSEEvent("content_block_delta", string(delta2)) +
		phase20SSEEvent("message_stop", string(msgStop))

	srv := phase20SSEServer(t, sseBody)
	defer srv.Close()

	provider := llm.NewClaudeProvider(
		llm.WithNativeBaseURL(srv.URL),
		llm.WithNativeAPIKey("test-key"),
	)
	ctx, cancel := phase20StreamCtx()
	defer cancel()
	model, cleanup, err := provider.Build(ctx, llm.ModelConfig{Model: "test-model"})
	require.NoError(t, err)
	defer cleanup()

	ch, err := model.Stream(ctx, []llm.Message{{Role: llm.RoleUser, Content: "hi"}})
	require.NoError(t, err)

	chunks := phase20CollectChunks(t, ch)
	assert.Greater(t, len(chunks), 1, "Claude stream must return multiple chunks")

	// Collect text content from non-final chunks.
	var content strings.Builder
	var hasFinal bool
	for _, c := range chunks {
		content.WriteString(c.Content)
		if c.Final {
			hasFinal = true
		}
	}
	assert.Contains(t, content.String(), "Hello")
	assert.Contains(t, content.String(), "world")
	assert.True(t, hasFinal, "stream must include a Final chunk")
}

// =============================================================================
// AC-3: OpenAI streaming returns a content chunk then a Final chunk
// =============================================================================

// TestET_Phase20_Streaming_AC3_OpenAIStream verifies that streaming against a
// mock OpenAI SSE endpoint returns a content chunk followed by a Final chunk
// when the server sends `data: [DONE]`.
func TestET_Phase20_Streaming_AC3_OpenAIStream(t *testing.T) {
	sseBody := phase20SSEData(`{"choices":[{"delta":{"content":"hello"}}]}`) +
		phase20SSEData("[DONE]")

	srv := phase20SSEServer(t, sseBody)
	defer srv.Close()

	provider := llm.NewOpenAIProvider(
		llm.WithNativeBaseURL(srv.URL),
		llm.WithNativeAPIKey("test-key"),
	)
	ctx, cancel := phase20StreamCtx()
	defer cancel()
	model, cleanup, err := provider.Build(ctx, llm.ModelConfig{Model: "test-model"})
	require.NoError(t, err)
	defer cleanup()

	ch, err := model.Stream(ctx, []llm.Message{{Role: llm.RoleUser, Content: "hi"}})
	require.NoError(t, err)

	chunks := phase20CollectChunks(t, ch)
	require.GreaterOrEqual(t, len(chunks), 2, "OpenAI stream must have at least content + final chunks")

	var foundContent, foundFinal bool
	for _, c := range chunks {
		if c.Content == "hello" {
			foundContent = true
		}
		if c.Final {
			foundFinal = true
		}
	}
	assert.True(t, foundContent, "stream must include a content chunk with 'hello'")
	assert.True(t, foundFinal, "stream must include a Final chunk")
}

// =============================================================================
// AC-4: Gemini streaming returns content chunks
// =============================================================================

// TestET_Phase20_Streaming_AC4_GeminiStream verifies that streaming against a
// mock Gemini SSE endpoint returns content chunks with the correct text.
func TestET_Phase20_Streaming_AC4_GeminiStream(t *testing.T) {
	chunk1, _ := json.Marshal(map[string]any{
		"candidates": []map[string]any{
			{"content": map[string]any{"parts": []map[string]any{{"text": "Hello"}}}},
		},
	})
	chunk2, _ := json.Marshal(map[string]any{
		"candidates": []map[string]any{
			{"content": map[string]any{"parts": []map[string]any{{"text": " world"}}}},
		},
	})

	sseBody := phase20SSEData(string(chunk1)) + phase20SSEData(string(chunk2))

	srv := phase20SSEServer(t, sseBody)
	defer srv.Close()

	provider := llm.NewGeminiProvider(
		llm.WithNativeBaseURL(srv.URL),
		llm.WithNativeAPIKey("test-key"),
	)
	ctx, cancel := phase20StreamCtx()
	defer cancel()
	model, cleanup, err := provider.Build(ctx, llm.ModelConfig{Model: "test-model"})
	require.NoError(t, err)
	defer cleanup()

	ch, err := model.Stream(ctx, []llm.Message{{Role: llm.RoleUser, Content: "hi"}})
	require.NoError(t, err)

	chunks := phase20CollectChunks(t, ch)
	assert.Greater(t, len(chunks), 1, "Gemini stream must return multiple chunks")

	var content strings.Builder
	for _, c := range chunks {
		content.WriteString(c.Content)
	}
	assert.Contains(t, content.String(), "Hello")
	assert.Contains(t, content.String(), "world")
}

// =============================================================================
// AC-5: Non-200 HTTP status returns an error
// =============================================================================

// TestET_Phase20_Streaming_AC5_Non200Error verifies that when the SSE endpoint
// returns a non-200 status, Stream() returns an error containing the status.
func TestET_Phase20_Streaming_AC5_Non200Error(t *testing.T) {
	srv := phase20ErrorServer(t, http.StatusInternalServerError, "internal server error")
	defer srv.Close()

	provider := llm.NewOpenAIProvider(
		llm.WithNativeBaseURL(srv.URL),
		llm.WithNativeAPIKey("test-key"),
	)
	ctx, cancel := phase20StreamCtx()
	defer cancel()
	model, cleanup, err := provider.Build(ctx, llm.ModelConfig{Model: "test-model"})
	require.NoError(t, err)
	defer cleanup()

	_, err = model.Stream(ctx, []llm.Message{{Role: llm.RoleUser, Content: "hi"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

// =============================================================================
// AC-6: Claude tool_use streaming - Final chunk has complete ToolCalls
// =============================================================================

// TestET_Phase20_Streaming_AC6_ClaudeToolUse verifies that Claude streaming with
// tool_use content blocks (content_block_start with tool_use + content_block_delta
// with input_json_delta) produces a Final chunk with complete, assembled ToolCalls.
func TestET_Phase20_Streaming_AC6_ClaudeToolUse(t *testing.T) {
	// message_start
	msgStart, _ := json.Marshal(map[string]any{
		"type":    "message_start",
		"message": map[string]any{"role": "assistant"},
	})
	// content_block_start with tool_use
	blockStart, _ := json.Marshal(map[string]any{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]any{
			"type": "tool_use",
			"id":   "tool-123",
			"name": "get_weather",
		},
	})
	// Two input_json_delta fragments that together form {"city":"SF"}
	delta1, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"city":`},
	})
	delta2, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": `"SF"}`},
	})
	msgStop, _ := json.Marshal(map[string]any{"type": "message_stop"})

	sseBody := phase20SSEEvent("message_start", string(msgStart)) +
		phase20SSEEvent("content_block_start", string(blockStart)) +
		phase20SSEEvent("content_block_delta", string(delta1)) +
		phase20SSEEvent("content_block_delta", string(delta2)) +
		phase20SSEEvent("message_stop", string(msgStop))

	srv := phase20SSEServer(t, sseBody)
	defer srv.Close()

	provider := llm.NewClaudeProvider(
		llm.WithNativeBaseURL(srv.URL),
		llm.WithNativeAPIKey("test-key"),
	)
	ctx, cancel := phase20StreamCtx()
	defer cancel()
	model, cleanup, err := provider.Build(ctx, llm.ModelConfig{Model: "test-model"})
	require.NoError(t, err)
	defer cleanup()

	ch, err := model.Stream(ctx, []llm.Message{{Role: llm.RoleUser, Content: "weather in SF"}})
	require.NoError(t, err)

	chunks := phase20CollectChunks(t, ch)

	// Find the Final chunk.
	var finalChunk *llm.MessageChunk
	for i := range chunks {
		if chunks[i].Final {
			finalChunk = &chunks[i]
			break
		}
	}
	require.NotNil(t, finalChunk, "stream must include a Final chunk")
	require.Len(t, finalChunk.ToolCalls, 1, "Final chunk must have exactly one ToolCall")

	tc := finalChunk.ToolCalls[0]
	assert.Equal(t, "tool-123", tc.ID)
	assert.Equal(t, "get_weather", tc.Name)

	argsMap, ok := tc.Args.(map[string]any)
	require.True(t, ok, "ToolCall Args must be decoded to map[string]any")
	assert.Equal(t, "SF", argsMap["city"])
}
