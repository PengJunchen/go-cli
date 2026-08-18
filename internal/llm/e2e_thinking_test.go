//go:build e2e

package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestET_thinking_level_params_correct verifies that different ThinkingLevel
// values produce correct request parameters for each provider (OpenAI, Claude,
// Gemini). It uses httptest.Server to capture the actual JSON request body sent
// by each native provider and checks that the thinking configuration is
// correctly injected by the provider-specific ThinkingAdapter.
//
// For each provider it verifies:
//   - ThinkingNone produces no thinking-related parameters in the request body.
//   - ThinkingHigh produces the correct provider-specific thinking parameters.
func TestET_thinking_level_params_correct(t *testing.T) {
	msgs := []Message{{Role: RoleUser, Content: "hello"}}

	// --- OpenAI ---
	// OpenAIThinkingAdapter sets "reasoning_effort" on the top-level request.
	t.Run("openai", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		var capturedBody []byte
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
		}))
		defer srv.Close()

		provider := NewOpenAIProvider(WithNativeBaseURL(srv.URL), WithNativeAPIKey("test-key"))
		model, cleanup, err := provider.Build(ctx, ModelConfig{Model: "gpt-4o"})
		require.NoError(t, err)
		defer cleanup()

		// ThinkingNone → no reasoning_effort key.
		_, err = model.Generate(ctx, msgs, WithThinking(ThinkingConfigForLevel(ThinkingNone)))
		require.NoError(t, err)

		var bodyMap map[string]any
		require.NoError(t, json.Unmarshal(capturedBody, &bodyMap))
		_, hasKey := bodyMap["reasoning_effort"]
		assert.False(t, hasKey, "ThinkingNone should not produce reasoning_effort for OpenAI")

		// ThinkingHigh → reasoning_effort = "high".
		_, err = model.Generate(ctx, msgs, WithThinking(ThinkingConfigForLevel(ThinkingHigh)))
		require.NoError(t, err)

		require.NoError(t, json.Unmarshal(capturedBody, &bodyMap))
		assert.Equal(t, "high", bodyMap["reasoning_effort"],
			"ThinkingHigh should set reasoning_effort=high for OpenAI")
	})

	// --- Claude ---
	// ClaudeThinkingAdapter sets "thinking": {"type":"enabled","budget_tokens":N}.
	t.Run("claude", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		var capturedBody []byte
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"stop"}`))
		}))
		defer srv.Close()

		provider := NewClaudeProvider(WithNativeBaseURL(srv.URL), WithNativeAPIKey("test-key"))
		model, cleanup, err := provider.Build(ctx, ModelConfig{Model: "claude-3"})
		require.NoError(t, err)
		defer cleanup()

		// ThinkingNone → no thinking key.
		_, err = model.Generate(ctx, msgs, WithThinking(ThinkingConfigForLevel(ThinkingNone)))
		require.NoError(t, err)

		var bodyMap map[string]any
		require.NoError(t, json.Unmarshal(capturedBody, &bodyMap))
		_, hasKey := bodyMap["thinking"]
		assert.False(t, hasKey, "ThinkingNone should not produce thinking params for Claude")

		// ThinkingHigh → thinking = {type: enabled, budget_tokens: 16384}.
		_, err = model.Generate(ctx, msgs, WithThinking(ThinkingConfigForLevel(ThinkingHigh)))
		require.NoError(t, err)

		require.NoError(t, json.Unmarshal(capturedBody, &bodyMap))
		thinking, ok := bodyMap["thinking"].(map[string]any)
		require.True(t, ok, "ThinkingHigh should set a thinking object for Claude")
		assert.Equal(t, "enabled", thinking["type"])
		assert.Equal(t, float64(16384), thinking["budget_tokens"])
	})

	// --- Gemini ---
	// GeminiThinkingAdapter sets "thinkingConfig" inside "generationConfig".
	t.Run("gemini", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		var capturedBody []byte
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`))
		}))
		defer srv.Close()

		provider := NewGeminiProvider(WithNativeBaseURL(srv.URL), WithNativeAPIKey("test-key"))
		model, cleanup, err := provider.Build(ctx, ModelConfig{Model: "gemini-pro"})
		require.NoError(t, err)
		defer cleanup()

		// ThinkingNone → no thinkingConfig inside generationConfig.
		_, err = model.Generate(ctx, msgs, WithThinking(ThinkingConfigForLevel(ThinkingNone)))
		require.NoError(t, err)

		var bodyMap map[string]any
		require.NoError(t, json.Unmarshal(capturedBody, &bodyMap))
		if gc, ok := bodyMap["generationConfig"].(map[string]any); ok {
			_, hasKey := gc["thinkingConfig"]
			assert.False(t, hasKey, "ThinkingNone should not produce thinkingConfig for Gemini")
		}

		// ThinkingHigh → generationConfig.thinkingConfig = {includeThoughts: true, thinkingBudget: 16384}.
		_, err = model.Generate(ctx, msgs, WithThinking(ThinkingConfigForLevel(ThinkingHigh)))
		require.NoError(t, err)

		require.NoError(t, json.Unmarshal(capturedBody, &bodyMap))
		gc, ok := bodyMap["generationConfig"].(map[string]any)
		require.True(t, ok, "generationConfig should exist for Gemini")
		tc, ok := gc["thinkingConfig"].(map[string]any)
		require.True(t, ok, "thinkingConfig should exist for Gemini with ThinkingHigh")
		assert.Equal(t, true, tc["includeThoughts"])
		assert.Equal(t, float64(16384), tc["thinkingBudget"])
	})
}
