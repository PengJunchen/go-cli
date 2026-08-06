package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// ---------------------------------------------------------------------------
// Provider contract basics
// ---------------------------------------------------------------------------

func TestNativeProvidersImplementModelProvider(t *testing.T) {
	var (
		_ ModelProvider = (*OpenAIProvider)(nil)
		_ ModelProvider = (*ClaudeProvider)(nil)
		_ ModelProvider = (*GeminiProvider)(nil)
	)
	assert.Equal(t, openaiProviderName, NewOpenAIProvider().Name())
	assert.Equal(t, claudeProviderName, NewClaudeProvider().Name())
	assert.Equal(t, geminiProviderName, NewGeminiProvider().Name())
}

func TestNativeProvidersModelsCopy(t *testing.T) {
	models := []ModelInfo{{Name: "m1", ContextWindow: 128000}}
	assert.Equal(t, "m1", NewOpenAIProvider(WithNativeModels(models)).Models()[0].Name)
	// Mutating the returned slice must not affect the provider.
	out := NewGeminiProvider(WithNativeModels(models)).Models()
	out[0].Name = "mutated"
	assert.Equal(t, "m1", NewGeminiProvider(WithNativeModels(models)).Models()[0].Name)
}

func TestNativeProvidersBuildInvalidEmptyModel(t *testing.T) {
	// A provider with no default yields an error on an empty model.
	p := &OpenAIProvider{}
	p.nativeProviderBase = nativeProviderBase{name: openaiProviderName, httpClient: http.DefaultClient}
	_, _, err := p.Build(context.Background(), ModelConfig{})
	require.Error(t, err)
}

func TestNativeProvidersBuildReturnsNilSafeCleanup(t *testing.T) {
	for _, p := range []ModelProvider{NewOpenAIProvider(), NewClaudeProvider(), NewGeminiProvider()} {
		m, cleanup, err := p.Build(context.Background(), ModelConfig{Model: "x"})
		require.NoError(t, err)
		require.NotNil(t, m)
		require.NotNil(t, cleanup)
		assert.NotPanics(t, func() { cleanup() })
	}
}

// ---------------------------------------------------------------------------
// OpenAI
// ---------------------------------------------------------------------------

func TestOpenAIProviderGenerateCannedCompletion(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer sk-test", r.Header.Get("Authorization"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		body, rerr := io.ReadAll(r.Body)
		require.NoError(t, rerr)
		var req openAIRequest
		require.NoError(t, json.Unmarshal(body, &req))
		assert.Equal(t, "gpt-4o", req.Model)
		assert.Equal(t, 0.5, req.Temperature)
		require.Len(t, req.Messages, 1)
		assert.Equal(t, "user", req.Messages[0].Role)
		assert.Equal(t, "hello", req.Messages[0].Content)

		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{
			"choices":[{"message":{"role":"assistant","content":"hi there"}}],
			"usage":{"prompt_tokens":6,"completion_tokens":3}
		}`)
	}))
	defer srv.Close()

	provider := NewOpenAIProvider(WithNativeBaseURL(srv.URL), WithNativeAPIKey("sk-test"))
	model, _, err := provider.Build(context.Background(), ModelConfig{Model: "gpt-4o", Temperature: 0.5})
	require.NoError(t, err)

	msg, err := model.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hello"}})
	require.NoError(t, err)
	require.NotNil(t, msg)
	assert.Equal(t, RoleAssistant, msg.Role)
	assert.Equal(t, "hi there", msg.Content)
	require.NotNil(t, msg.Usage)
	assert.Equal(t, 9, msg.Usage.TotalTokens)
}

// ---------------------------------------------------------------------------
// Claude
// ---------------------------------------------------------------------------

func TestClaudeProviderGenerateCannedCompletion(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/messages", r.URL.Path)
		assert.Equal(t, "sk-ant-test", r.Header.Get("x-api-key"))
		assert.Equal(t, claudeVersionHeader, r.Header.Get("anthropic-version"))

		body, rerr := io.ReadAll(r.Body)
		require.NoError(t, rerr)
		var req claudeRequest
		require.NoError(t, json.Unmarshal(body, &req))
		assert.Equal(t, "claude-3-5-sonnet-latest", req.Model)
		assert.Equal(t, "You are helpful.", req.System)
		require.Len(t, req.Messages, 1)
		assert.Equal(t, "user", req.Messages[0].Role)
		require.Len(t, req.Messages[0].Content, 1)
		assert.Equal(t, "text", req.Messages[0].Content[0].Type)
		assert.Equal(t, "hello", req.Messages[0].Content[0].Text)

		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"content":[{"type":"text","text":"bonjour"}],"usage":{"input_tokens":4,"output_tokens":2}}`)
	}))
	defer srv.Close()

	provider := NewClaudeProvider(WithNativeBaseURL(srv.URL), WithNativeAPIKey("sk-ant-test"))
	model, _, err := provider.Build(context.Background(), ModelConfig{Model: "claude-3-5-sonnet-latest"})
	require.NoError(t, err)

	msg, err := model.Generate(context.Background(), []Message{
		{Role: RoleSystem, Content: "You are helpful."},
		{Role: RoleUser, Content: "hello"},
	})
	require.NoError(t, err)
	require.NotNil(t, msg)
	assert.Equal(t, RoleAssistant, msg.Role)
	assert.Equal(t, "bonjour", msg.Content)
	require.NotNil(t, msg.Usage)
	assert.Equal(t, 4, msg.Usage.InputTokens)
	assert.Equal(t, 2, msg.Usage.OutputTokens)
	assert.Equal(t, 6, msg.Usage.TotalTokens)
}

func TestClaudeProviderGenerateSystemOnly(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, rerr := io.ReadAll(r.Body)
		require.NoError(t, rerr)
		var req claudeRequest
		require.NoError(t, json.Unmarshal(body, &req))
		assert.Equal(t, "sys A\nsys B", req.System)
		require.Len(t, req.Messages, 0)
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"content":[]}`)
	}))
	defer srv.Close()

	provider := NewClaudeProvider(WithNativeBaseURL(srv.URL))
	model, _, err := provider.Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)
	_, err = model.Generate(context.Background(), []Message{
		{Role: RoleSystem, Content: "sys A"},
		{Role: RoleSystem, Content: "sys B"},
	})
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Gemini
// ---------------------------------------------------------------------------

func TestGeminiProviderGenerateCannedCompletion(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	var gotPath, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-goog-api-key")
		assert.Equal(t, http.MethodPost, r.Method)

		body, rerr := io.ReadAll(r.Body)
		require.NoError(t, rerr)
		var req geminiRequest
		require.NoError(t, json.Unmarshal(body, &req))
		require.Len(t, req.Contents, 1)
		assert.Equal(t, "user", req.Contents[0].Role)
		require.Len(t, req.Contents[0].Parts, 1)
		assert.Equal(t, "hello", req.Contents[0].Parts[0].Text)
		require.NotNil(t, req.GenerationConfig)
		assert.Equal(t, 0.3, req.GenerationConfig.Temperature)

		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{
			"candidates":[{"content":{"parts":[{"text":"hi from gemini"}]}}],
			"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":7}
		}`)
	}))
	defer srv.Close()

	provider := NewGeminiProvider(WithNativeBaseURL(srv.URL), WithNativeAPIKey("goog-test"))
	model, _, err := provider.Build(context.Background(), ModelConfig{Model: "gemini-1.5-flash", Temperature: 0.3})
	require.NoError(t, err)

	msg, err := model.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hello"}})
	require.NoError(t, err)
	require.NotNil(t, msg)
	assert.Equal(t, RoleAssistant, msg.Role)
	assert.Equal(t, "hi from gemini", msg.Content)
	assert.Equal(t, "/v1beta/models/gemini-1.5-flash:generateContent", gotPath)
	assert.Equal(t, "goog-test", gotKey)
	require.NotNil(t, msg.Usage)
	assert.Equal(t, 12, msg.Usage.TotalTokens)
}

// ---------------------------------------------------------------------------
// Stream (shared native helper)
// ---------------------------------------------------------------------------

func TestNativeChatModelStreamSingleChunk(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeResponse(t, w, "data: {\"choices\":[{\"delta\":{\"content\":\"streamed\"}}]}\n\ndata: [DONE]\n\n")
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
	require.Len(t, chunks, 2)
	assert.Equal(t, RoleAssistant, chunks[0].Role)
	assert.Equal(t, "streamed", chunks[0].Content)
	assert.True(t, chunks[1].Final)
}

func TestNativeChatModelHTTP500(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		writeResponse(t, w, `{"error":{"message":"boom"}}`)
	}))
	defer srv.Close()

	provider := NewGeminiProvider(WithNativeBaseURL(srv.URL))
	model, _, err := provider.Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)
	_, err = model.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}
