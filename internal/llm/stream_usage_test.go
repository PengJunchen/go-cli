package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestStreamOpenAI_CapturesUsageFromFinalChunk verifies that OpenAI streaming
// captures usage from the final chunk (empty choices array with usage object)
// when stream_options.include_usage is true.
func TestStreamOpenAI_CapturesUsageFromFinalChunk(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sseBody := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hello"}}]}`,
		"",
		`data: {"choices":[{"delta":{"content":" world"}}]}`,
		"",
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeResponse(t, w, sseBody)
	}))
	defer srv.Close()

	provider := NewOpenAIProvider(WithNativeBaseURL(srv.URL))
	model, _, err := provider.Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)

	ch, err := model.Stream(context.Background(), nil)
	require.NoError(t, err)

	var chunks []MessageChunk
	for c := range ch {
		chunks = append(chunks, c)
	}

	// Find the final chunk.
	var final *MessageChunk
	for i := range chunks {
		if chunks[i].Final {
			final = &chunks[i]
		}
	}
	require.NotNil(t, final, "should have a final chunk")

	// Verify usage was captured from the empty-choices chunk.
	require.NotNil(t, final.Usage, "final chunk should carry API usage")
	assert.Equal(t, 10, final.Usage.InputTokens)
	assert.Equal(t, 5, final.Usage.OutputTokens)
	assert.Equal(t, 15, final.Usage.TotalTokens)
}

// TestStreamEino_CapturesUsageFromFinalChunk verifies that the EinoProvider
// (HTTPChatModel) streaming path also captures usage from the final chunk.
func TestStreamEino_CapturesUsageFromFinalChunk(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sseBody := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		"",
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":8,"completion_tokens":3}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeResponse(t, w, sseBody)
	}))
	defer srv.Close()

	model := &HTTPChatModel{
		provider: NewEinoProvider(WithBaseURL(srv.URL)),
		cfg:      ModelConfig{Model: "m"},
		client:   http.DefaultClient,
	}
	ch, err := model.Stream(context.Background(), nil)
	require.NoError(t, err)

	var chunks []MessageChunk
	for c := range ch {
		chunks = append(chunks, c)
	}

	var final *MessageChunk
	for i := range chunks {
		if chunks[i].Final {
			final = &chunks[i]
		}
	}
	require.NotNil(t, final)
	require.NotNil(t, final.Usage)
	assert.Equal(t, 8, final.Usage.InputTokens)
	assert.Equal(t, 3, final.Usage.OutputTokens)
}

// TestStreamClaude_CapturesUsageFromMessageDelta verifies that Claude streaming
// captures usage from the message_delta event's output_tokens.
func TestStreamClaude_CapturesUsageFromMessageDelta(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sseBody := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"role":"assistant","usage":{"input_tokens":12}}}`,
		"",
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		"",
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"type":"message_delta","stop_reason":"end_turn","usage":{"output_tokens":7}}}`,
		"",
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeResponse(t, w, sseBody)
	}))
	defer srv.Close()

	provider := NewClaudeProvider(WithNativeBaseURL(srv.URL), WithNativeAPIKey("test"))
	model, _, err := provider.Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)

	ch, err := model.Stream(context.Background(), nil)
	require.NoError(t, err)

	var chunks []MessageChunk
	for c := range ch {
		chunks = append(chunks, c)
	}

	var final *MessageChunk
	for i := range chunks {
		if chunks[i].Final {
			final = &chunks[i]
		}
	}
	require.NotNil(t, final)

	// Input from message_start, output from message_delta.
	require.NotNil(t, final.Usage)
	assert.Equal(t, 12, final.Usage.InputTokens)
	assert.Equal(t, 7, final.Usage.OutputTokens)
	assert.Equal(t, 19, final.Usage.TotalTokens)
}

// TestStreamClaude_UsageSplitAcrossEvents verifies that when input_tokens
// arrive in message_start and output_tokens arrive in message_delta, the
// final chunk combines both correctly.
func TestStreamClaude_UsageSplitAcrossEvents(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sseBody := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"role":"assistant","usage":{"input_tokens":25}}}`,
		"",
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"response"}}`,
		"",
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"type":"message_delta","stop_reason":"end_turn","usage":{"output_tokens":15}}}`,
		"",
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeResponse(t, w, sseBody)
	}))
	defer srv.Close()

	provider := NewClaudeProvider(WithNativeBaseURL(srv.URL), WithNativeAPIKey("test"))
	model, _, err := provider.Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)

	ch, err := model.Stream(context.Background(), nil)
	require.NoError(t, err)

	var chunks []MessageChunk
	for c := range ch {
		chunks = append(chunks, c)
	}

	var final *MessageChunk
	for i := range chunks {
		if chunks[i].Final {
			final = &chunks[i]
		}
	}
	require.NotNil(t, final)
	require.NotNil(t, final.Usage)
	// Input from message_start, output from message_delta.
	assert.Equal(t, 25, final.Usage.InputTokens)
	assert.Equal(t, 15, final.Usage.OutputTokens)
	assert.Equal(t, 40, final.Usage.TotalTokens)
}

// TestStreamGemini_CapturesUsageMetadata verifies that Gemini streaming
// captures usageMetadata from the response and sets it on the Final chunk.
func TestStreamGemini_CapturesUsageMetadata(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sseBody := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"Hello"}]}}]}`,
		"",
		`data: {"candidates":[{"content":{"parts":[{"text":" world"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":8}}`,
		"",
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeResponse(t, w, sseBody)
	}))
	defer srv.Close()

	provider := NewGeminiProvider(WithNativeBaseURL(srv.URL), WithNativeAPIKey("test"))
	model, _, err := provider.Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)

	ch, err := model.Stream(context.Background(), nil)
	require.NoError(t, err)

	var chunks []MessageChunk
	for c := range ch {
		chunks = append(chunks, c)
	}

	var final *MessageChunk
	for i := range chunks {
		if chunks[i].Final {
			final = &chunks[i]
		}
	}
	require.NotNil(t, final)
	require.NotNil(t, final.Usage)
	assert.Equal(t, 20, final.Usage.InputTokens)
	assert.Equal(t, 8, final.Usage.OutputTokens)
	assert.Equal(t, 28, final.Usage.TotalTokens)
}

// TestStreamOpenAI_NoUsageWhenNotProvided verifies that when the API does not
// send usage in the stream, the final chunk's Usage remains nil (backward
// compatible).
func TestStreamOpenAI_NoUsageWhenNotProvided(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sseBody := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hello"}}]}`,
		"",
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeResponse(t, w, sseBody)
	}))
	defer srv.Close()

	provider := NewOpenAIProvider(WithNativeBaseURL(srv.URL))
	model, _, err := provider.Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)

	ch, err := model.Stream(context.Background(), nil)
	require.NoError(t, err)

	var chunks []MessageChunk
	for c := range ch {
		chunks = append(chunks, c)
	}

	var final *MessageChunk
	for i := range chunks {
		if chunks[i].Final {
			final = &chunks[i]
		}
	}
	require.NotNil(t, final)
	assert.Nil(t, final.Usage, "Usage should be nil when API does not report usage")
}
