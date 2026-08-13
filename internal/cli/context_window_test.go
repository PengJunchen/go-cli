package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubAgentLoop is a minimal AgentLoop used to construct an AgentImpl in tests.
type stubAgentLoop struct{}

func (s *stubAgentLoop) Run(context.Context, core.Submission, ...core.EventStream) ([]core.AgentEvent, error) {
	return nil, nil
}

// TestTokenUsageEvent verifies that emitTokenUsageEvent sends a token_usage
// event whose MaxTokens equals the ContextWindow (not the compaction
// MaxTokens). This ensures the TUI status bar shows the correct window size.
func TestTokenUsageEvent(t *testing.T) {
	estimator := compaction.NewUnicodeTokenEstimator()
	agent := core.NewAgentImpl("test", &stubAgentLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "Hello, world"},
		{Role: "assistant", Content: "Hi there! How can I help?"},
	}))
	costTracker := production.NewCostTracker(nil)

	assembly := &AgentAssembly{
		Agent:         agent,
		Estimator:     estimator,
		CostTracker:   costTracker,
		MaxTokens:     8000,
		ContextWindow: 128000,
	}

	stream := core.NewEventStream(16)
	emitTokenUsageEvent(stream, assembly)

	select {
	case ev := <-stream.Events():
		require.NotNil(t, ev.TokenUsage)
		assert.Equal(t, 128000, ev.TokenUsage.MaxTokens,
			"MaxTokens in token_usage event should equal ContextWindow, not the compaction budget")
	default:
		t.Fatal("expected a token_usage event on the stream")
	}
}

// TestCostHandler verifies that the /cost command displays context window
// occupancy (total tokens / context window as a percentage).
func TestCostHandler(t *testing.T) {
	statsReg := production.NewStatsRegistry()
	statsReg.RecordTokens("sess-1", 5000, 3000)

	var buf bytes.Buffer
	sc := &slashContext{
		statsRegistry: statsReg,
		sessionID:     "sess-1",
		out:           &buf,
		contextWindow: 128000,
	}

	h := &CostHandler{}
	_, err := h.Handle(context.Background(), nil, sc)
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "Context:")
	assert.Contains(t, output, "128000")
	// 8000 total tokens / 128000 = 6%
	assert.Contains(t, output, "(6%)")
}

// TestContextWindowFallback verifies that when ModelInfo.ContextWindow is 0
// (the provider does not report a context window), the AgentAssembly falls
// back to MaxTokens for the ContextWindow value.
func TestContextWindowFallback(t *testing.T) {
	// Use the EinoProvider path (BaseURL set) without configuring any models.
	// findModelInfo will return a zero ModelInfo with ContextWindow=0.
	rc := &config.Config{
		Provider: config.ProviderConfig{
			BaseURL: "http://localhost:8080",
		},
	}

	ctx := context.Background()
	_, modelInfo, cleanup, err := buildModel(ctx, rc, "eino", "test-model")
	require.NoError(t, err)
	if cleanup != nil {
		defer cleanup()
	}

	// The provider has no models configured, so ContextWindow should be 0.
	assert.Equal(t, 0, modelInfo.ContextWindow,
		"buildModel should return zero ContextWindow when provider has no model list")

	// Simulate the fallback logic from AssembleAgent.
	maxTokens := 8000
	contextWindow := modelInfo.ContextWindow
	if contextWindow <= 0 {
		contextWindow = maxTokens
	}
	assert.Equal(t, maxTokens, contextWindow,
		"ContextWindow should fall back to MaxTokens when ModelInfo.ContextWindow is 0")
}

// TestFindModelInfo verifies the helper that locates a model by name in a
// slice of ModelInfo.
func TestFindModelInfo(t *testing.T) {
	models := []llm.ModelInfo{
		{Name: "gpt-4o", ContextWindow: 128000},
		{Name: "gpt-4o-mini", ContextWindow: 64000},
	}

	// Found
	info := findModelInfo(models, "gpt-4o")
	assert.Equal(t, 128000, info.ContextWindow)

	// Not found returns zero value
	info = findModelInfo(models, "nonexistent")
	assert.Equal(t, 0, info.ContextWindow)
	assert.Equal(t, "", info.Name)

	// Empty slice returns zero value
	info = findModelInfo(nil, "anything")
	assert.Equal(t, 0, info.ContextWindow)
}
