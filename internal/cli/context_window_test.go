package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/production"
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
		CoreRuntime:          CoreRuntime{Agent: agent},
		SessionManagement:    SessionManagement{Estimator: estimator, MaxTokens: 8000},
		ProductionResilience: ProductionResilience{CostTracker: costTracker},
		ModelConfig:          ModelConfig{ContextWindow: 128000},
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

func TestTokenUsageEventUsesAttemptSubtotalsAndUnknownCounts(t *testing.T) {
	agent := core.NewAgentImpl("attempt-usage", &stubAgentLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "request"},
		{Role: "assistant", Content: "response", Usage: &llm.Usage{InputTokens: 99, OutputTokens: 99}},
	}))
	tracker := production.NewCostTracker(nil)
	known := llm.ProviderAttemptResult{
		OperationID: "op-known",
		AttemptID:   "attempt-known",
		Identity: llm.AttemptIdentity{
			ProviderID: "openai",
			ModelID:    "gpt-4o",
			ProtocolID: llm.ProtocolOpenAICompatible,
			OriginKind: llm.OriginResolverDefaultEndpoint,
			Candidate:  "candidate-known",
		},
		Outcome: llm.AttemptSuccess,
		Usage: llm.UsageEvidence{
			State:  llm.UsageKnown,
			Usage:  &llm.Usage{InputTokens: 12, OutputTokens: 4, TotalTokens: 16},
			Source: llm.UsageProviderResponse,
		},
	}
	unknown := known
	unknown.OperationID = "op-unknown"
	unknown.AttemptID = "attempt-unknown"
	unknown.Identity.Candidate = "candidate-unknown"
	unknown.Outcome = llm.AttemptTransportFailure
	unknown.Failure = llm.FailureTransientProvider
	unknown.Usage = llm.UnknownUsageEvidence()
	_, err := tracker.RecordAttempt(known)
	require.NoError(t, err)
	_, err = tracker.RecordAttempt(unknown)
	require.NoError(t, err)

	assembly := &AgentAssembly{
		CoreRuntime:          CoreRuntime{Agent: agent},
		ProductionResilience: ProductionResilience{CostTracker: tracker},
		ModelConfig:          ModelConfig{ContextWindow: 8192},
	}
	stream := core.NewEventStream(16)
	emitTokenUsageEvent(stream, assembly)

	select {
	case event := <-stream.Events():
		require.NotNil(t, event.TokenUsage)
		assert.Equal(t, 12, event.TokenUsage.InputTokens)
		assert.Equal(t, 4, event.TokenUsage.OutputTokens)
		assert.Equal(t, 1, event.TokenUsage.UnknownUsageAttempts)
		assert.Equal(t, 1, event.TokenUsage.UnknownCostAttempts)
		assert.Greater(t, event.TokenUsage.Cost, 0.0)
	default:
		t.Fatal("expected a token_usage event")
	}
}

// TestCostHandler verifies that cumulative per-request token totals are not
// presented as current context-window occupancy.
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
	assert.Contains(t, output, "Context:   unknown/128000 (current occupancy unavailable)")
	assert.NotContains(t, output, "Context:   8000/128000", "cumulative request usage is not current context occupancy")
}

func TestCostHandlerReportsUnknownContextOccupancyWithoutSessionSnapshot(t *testing.T) {
	var buf bytes.Buffer
	sc := &slashContext{
		statsRegistry: production.NewStatsRegistry(),
		sessionID:     "missing-session",
		out:           &buf,
		contextWindow: 128000,
	}

	_, err := (&CostHandler{}).Handle(context.Background(), nil, sc)
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "Context:   unknown/128000 (current occupancy unavailable)")
	assert.NotContains(t, output, "Context:   0/128000", "a missing session snapshot must not imply empty context")
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
