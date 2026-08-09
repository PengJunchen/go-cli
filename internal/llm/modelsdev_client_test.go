package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// sampleModelsDevAPI returns a minimal but realistic models.dev API JSON
// response covering two providers and three models.
func sampleModelsDevAPI() string {
	return `{
  "openai": {
    "name": "OpenAI",
    "npm": "@ai-sdk/openai",
    "api": "https://api.openai.com/v1",
    "env": ["OPENAI_API_KEY"],
    "doc": "https://platform.openai.com/docs/api-reference",
    "models": {
      "gpt-4o": {
        "name": "GPT-4o",
        "attachment": true,
        "reasoning": false,
        "tool_call": true,
        "structured_output": true,
        "temperature": true,
        "cost": {"input": 0.005, "output": 0.015},
        "limit": {"context": 128000, "input": 128000, "output": 16384},
        "modalities": {"input": ["text", "image"], "output": ["text"]}
      },
      "gpt-4o-mini": {
        "name": "GPT-4o mini",
        "attachment": true,
        "reasoning": false,
        "tool_call": true,
        "structured_output": true,
        "temperature": true,
        "cost": {"input": 0.00015, "output": 0.0006},
        "limit": {"context": 128000, "input": 128000, "output": 16384},
        "modalities": {"input": ["text", "image"], "output": ["text"]}
      }
    }
  },
  "anthropic": {
    "name": "Anthropic",
    "npm": "@ai-sdk/anthropic",
    "api": "https://api.anthropic.com/v1",
    "env": ["ANTHROPIC_API_KEY"],
    "doc": "https://docs.anthropic.com",
    "models": {
      "claude-3-5-sonnet": {
        "name": "Claude 3.5 Sonnet",
        "attachment": true,
        "reasoning": true,
        "tool_call": true,
        "structured_output": true,
        "temperature": true,
        "cost": {"input": 0.003, "output": 0.015},
        "limit": {"context": 200000, "input": 200000, "output": 8192},
        "modalities": {"input": ["text", "image"], "output": ["text"]}
      }
    }
  }
}`
}

// TestModelsDevClient_ParseAndLookup verifies that the registry correctly
// parses the upstream JSON and that Lookup/Providers/ModelsForProvider return
// enriched ModelInfo and ProviderMetadata.
func TestModelsDevClient_ParseAndLookup(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sampleModelsDevAPI()))
	}))
	defer server.Close()

	cachePath := filepath.Join(t.TempDir(), "cache.json")
	reg := NewModelsDevRegistry(cachePath, time.Hour)
	reg.url = server.URL

	require.NoError(t, reg.Refresh(context.Background()))

	// Lookup an existing model and verify enriched fields.
	info, ok := reg.Lookup("openai", "gpt-4o")
	require.True(t, ok)
	assert.Equal(t, "GPT-4o", info.Name)
	assert.Equal(t, 128000, info.ContextWindow)
	assert.Equal(t, 16384, info.MaxOutputTokens)
	assert.Equal(t, 0.005, info.InputPrice)
	assert.Equal(t, 0.015, info.OutputPrice)
	assert.Equal(t, "text+image\u2192text", info.Modality)

	// Lookup a model from a second provider.
	info2, ok := reg.Lookup("anthropic", "claude-3-5-sonnet")
	require.True(t, ok)
	assert.Equal(t, "Claude 3.5 Sonnet", info2.Name)
	assert.Equal(t, 200000, info2.ContextWindow)
	assert.Equal(t, 8192, info2.MaxOutputTokens)
	assert.Equal(t, 0.003, info2.InputPrice)
	assert.Equal(t, 0.015, info2.OutputPrice)

	// Unknown provider returns false.
	_, ok = reg.Lookup("nope", "gpt-4o")
	assert.False(t, ok)

	// Unknown model returns false.
	_, ok = reg.Lookup("openai", "nope")
	assert.False(t, ok)

	// Providers returns all provider metadata.
	providers := reg.Providers()
	require.Len(t, providers, 2)

	var openaiMeta *ProviderMetadata
	for i := range providers {
		if providers[i].ID == "openai" {
			openaiMeta = &providers[i]
			break
		}
	}
	require.NotNil(t, openaiMeta)
	assert.Equal(t, "OpenAI", openaiMeta.Name)
	assert.Equal(t, "https://api.openai.com/v1", openaiMeta.APIBase)
	assert.Equal(t, []string{"OPENAI_API_KEY"}, openaiMeta.Env)

	// ModelsForProvider returns all models for a known provider.
	models := reg.ModelsForProvider("openai")
	require.Len(t, models, 2)

	// ModelsForProvider returns nil for an unknown provider.
	assert.Nil(t, reg.ModelsForProvider("nope"))
}

// TestModelsDevClient_CacheHit verifies that a second registry instance with
// the same cache path uses the on-disk cache without contacting the upstream.
func TestModelsDevClient_CacheHit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	fetchCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sampleModelsDevAPI()))
	}))
	defer server.Close()

	cachePath := filepath.Join(t.TempDir(), "cache.json")

	// First instance: fetch from upstream and write the cache.
	reg1 := NewModelsDevRegistry(cachePath, time.Hour)
	reg1.url = server.URL
	require.NoError(t, reg1.Refresh(context.Background()))
	require.Equal(t, 1, fetchCount)

	// The cache file should exist on disk.
	_, err := os.Stat(cachePath)
	require.NoError(t, err)

	// Second instance: Lookup should use the fresh cache, not the network.
	reg2 := NewModelsDevRegistry(cachePath, time.Hour)
	reg2.url = server.URL

	info, ok := reg2.Lookup("openai", "gpt-4o")
	require.True(t, ok)
	assert.Equal(t, "GPT-4o", info.Name)
	assert.Equal(t, 128000, info.ContextWindow)

	// No additional HTTP fetch should have occurred.
	assert.Equal(t, 1, fetchCount, "should use cached data without fetching")
}

// TestModelsDevClient_OfflineFallback verifies that when the upstream is
// unreachable and the cache is stale, the registry falls back to the stale
// cache so callers can still look up models.
func TestModelsDevClient_OfflineFallback(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	cachePath := filepath.Join(t.TempDir(), "cache.json")

	// Phase 1: populate the cache with a healthy upstream.
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sampleModelsDevAPI()))
	}))

	reg1 := NewModelsDevRegistry(cachePath, time.Hour)
	reg1.url = healthy.URL
	require.NoError(t, reg1.Refresh(context.Background()))
	healthy.Close()

	// Phase 2: a "broken" upstream that always returns 503, combined with a
	// very short TTL so the cache is considered stale.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer broken.Close()

	reg2 := NewModelsDevRegistry(cachePath, 1*time.Millisecond)
	reg2.url = broken.URL

	// Allow the TTL to expire so ensureLoaded will try to refresh.
	time.Sleep(10 * time.Millisecond)

	// Lookup triggers ensureLoaded, which finds the stale cache, tries to
	// refresh, fails, and falls back to the stale cache.
	info, ok := reg2.Lookup("openai", "gpt-4o")
	require.True(t, ok)
	assert.Equal(t, "GPT-4o", info.Name)
	assert.Equal(t, 128000, info.ContextWindow)
	assert.Equal(t, 16384, info.MaxOutputTokens)

	info2, ok := reg2.Lookup("anthropic", "claude-3-5-sonnet")
	require.True(t, ok)
	assert.Equal(t, "Claude 3.5 Sonnet", info2.Name)
}

// TestComposerWithRegistry verifies that the DefaultProviderComposer accepts a
// ModelRegistry option and still produces a working ProviderRegistry.
func TestComposerWithRegistry(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	noop := NoopModelRegistry{}
	c := NewDefaultProviderComposer(WithModelRegistry(noop))

	reg, err := c.Compose(context.Background())
	require.NoError(t, err)
	require.NotNil(t, reg)

	// Builtin providers should still be available.
	p, err := reg.Get("eino")
	require.NoError(t, err)
	assert.Equal(t, "eino", p.Name())
}
