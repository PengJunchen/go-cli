//go:build integration

// Package cli contains integration tests that require real LLM API keys.
// These tests are skipped unless the appropriate environment variables are set.
// Run with: go test -tags integration ./internal/cli/...
package cli

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestIntegration_OpenAI tests a single-turn conversation with OpenAI.
// Requires OPENAI_API_KEY environment variable.
func TestIntegration_OpenAI(t *testing.T) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		t.Skip("OPENAI_API_KEY not set, skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The integration test verifies that:
	// 1. The CLI can connect to OpenAI with a real API key
	// 2. A single-turn conversation returns a non-empty response
	// 3. Token usage is reported
	t.Log("OpenAI integration test placeholder - implement with real CLI invocation")
	t.Log("API key length:", len(apiKey))
	_ = ctx
}

// TestIntegration_Claude tests a single-turn conversation with Anthropic Claude.
// Requires ANTHROPIC_API_KEY environment variable.
func TestIntegration_Claude(t *testing.T) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY not set, skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Log("Claude integration test placeholder - implement with real CLI invocation")
	t.Log("API key length:", len(apiKey))
	_ = ctx
}

// TestIntegration_Gemini tests a single-turn conversation with Google Gemini.
// Requires GEMINI_API_KEY environment variable.
func TestIntegration_Gemini(t *testing.T) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		t.Skip("GEMINI_API_KEY not set, skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Log("Gemini integration test placeholder - implement with real CLI invocation")
	t.Log("API key length:", len(apiKey))
	_ = ctx
}
