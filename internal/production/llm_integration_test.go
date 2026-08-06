package production

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// flakyModel fails the first maxFails calls, then succeeds.
type flakyModel struct {
	mu        sync.Mutex
	failCount int
	maxFails  int
	usage     *llm.Usage
}

func (m *flakyModel) Generate(_ context.Context, _ []llm.Message, _ ...llm.Option) (*llm.Message, error) {
	m.mu.Lock()
	m.failCount++
	shouldFail := m.failCount <= m.maxFails
	m.mu.Unlock()

	if shouldFail {
		return nil, fmt.Errorf("transient error")
	}

	resp := &llm.Message{Role: llm.RoleAssistant, Content: "success"}
	if m.usage != nil {
		resp.Usage = m.usage
	}
	return resp, nil
}

func (m *flakyModel) Stream(_ context.Context, _ []llm.Message, _ ...llm.Option) (<-chan llm.MessageChunk, error) {
	return nil, fmt.Errorf("not implemented")
}

var _ llm.BaseChatModel = (*flakyModel)(nil)

func TestProductionModelWrapperWrapModel(t *testing.T) {
	model := &flakyModel{maxFails: 0}
	wrapper := NewProductionModelWrapper()

	wrapped := wrapper.WrapModel(model)
	require.NotNil(t, wrapped)

	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "success", resp.Content)
}

func TestProductionModelWrapperRetry(t *testing.T) {
	model := &flakyModel{maxFails: 1}

	policy := NewDefaultRetryPolicy(RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   1 * time.Millisecond,
		MaxDelay:    10 * time.Millisecond,
	})

	wrapper := NewProductionModelWrapper(
		WithWrapperRetryPolicy(policy),
	)

	wrapped := wrapper.WrapModel(model)

	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "success", resp.Content)

	// The model should have been called twice: first (fail) + retry (success).
	model.mu.Lock()
	assert.Equal(t, 2, model.failCount)
	model.mu.Unlock()
}

func TestProductionModelWrapperRetryExhausted(t *testing.T) {
	model := &flakyModel{maxFails: 100} // always fails

	policy := NewDefaultRetryPolicy(RetryConfig{
		MaxAttempts: 2,
		BaseDelay:   1 * time.Millisecond,
		MaxDelay:    5 * time.Millisecond,
	})

	wrapper := NewProductionModelWrapper(
		WithWrapperRetryPolicy(policy),
	)

	wrapped := wrapper.WrapModel(model)

	_, err := wrapped.Generate(context.Background(), nil)
	assert.Error(t, err)
}

func TestProductionModelWrapperCostTracking(t *testing.T) {
	model := &flakyModel{
		maxFails: 0,
		usage: &llm.Usage{
			InputTokens:  100,
			OutputTokens: 50,
			TotalTokens:  150,
		},
	}

	tracker := NewCostTracker(nil) // use default tiers
	wrapper := NewProductionModelWrapper(
		WithWrapperCostTracker(tracker),
		WithWrapperModelName("gpt-4o"),
	)

	wrapped := wrapper.WrapModel(model)
	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "success", resp.Content)

	assert.Equal(t, 1, tracker.Calls())
	assert.Greater(t, tracker.Total(), 0.0)
}

func TestProductionModelWrapperStatsRecording(t *testing.T) {
	model := &flakyModel{
		maxFails: 0,
		usage: &llm.Usage{
			InputTokens:  200,
			OutputTokens: 100,
			TotalTokens:  300,
		},
	}

	stats := NewStatsRegistry()
	wrapper := NewProductionModelWrapper(
		WithWrapperStatsRegistry(stats),
		WithWrapperSessionID("session-1"),
	)

	wrapped := wrapper.WrapModel(model)
	_, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)

	s, ok := stats.GetSessionStats("session-1")
	require.True(t, ok)
	assert.Equal(t, 200, s.TokensIn)
	assert.Equal(t, 100, s.TokensOut)
}

func TestProductionModelWrapperCostAndStats(t *testing.T) {
	model := &flakyModel{
		maxFails: 0,
		usage: &llm.Usage{
			InputTokens:  500,
			OutputTokens: 250,
			TotalTokens:  750,
		},
	}

	tracker := NewCostTracker(nil)
	stats := NewStatsRegistry()
	wrapper := NewProductionModelWrapper(
		WithWrapperCostTracker(tracker),
		WithWrapperStatsRegistry(stats),
		WithWrapperSessionID("combined-session"),
		WithWrapperModelName("gpt-4o-mini"),
	)

	wrapped := wrapper.WrapModel(model)
	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "success", resp.Content)

	// Cost tracked.
	assert.Equal(t, 1, tracker.Calls())
	assert.Greater(t, tracker.Total(), 0.0)

	// Stats recorded.
	s, ok := stats.GetSessionStats("combined-session")
	require.True(t, ok)
	assert.Equal(t, 500, s.TokensIn)
	assert.Equal(t, 250, s.TokensOut)
}

func TestProductionModelWrapperNoRetry(t *testing.T) {
	model := &flakyModel{maxFails: 0}

	wrapper := NewProductionModelWrapper() // no retry policy

	wrapped := wrapper.WrapModel(model)
	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "success", resp.Content)
}

func TestProductionModelWrapperNilUsage(t *testing.T) {
	model := &flakyModel{maxFails: 0, usage: nil}

	tracker := NewCostTracker(nil)
	stats := NewStatsRegistry()
	wrapper := NewProductionModelWrapper(
		WithWrapperCostTracker(tracker),
		WithWrapperStatsRegistry(stats),
		WithWrapperModelName("gpt-4o"),
		WithWrapperSessionID("nil-usage-session"),
	)

	wrapped := wrapper.WrapModel(model)
	_, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)

	// With nil Usage, no cost or stats should be recorded.
	assert.Equal(t, 0, tracker.Calls())
	_, ok := stats.GetSessionStats("nil-usage-session")
	assert.False(t, ok)
}

func TestProductionModelWrapperString(t *testing.T) {
	wrapper := NewProductionModelWrapper(
		WithWrapperSessionID("s1"),
		WithWrapperModelName("gpt-4o"),
	)
	s := wrapper.String()
	assert.Contains(t, s, "gpt-4o")
	assert.Contains(t, s, "s1")
}

func TestProductionModelWrapperStream(t *testing.T) {
	model := &flakyModel{maxFails: 0}

	tracker := NewCostTracker(nil)
	wrapper := NewProductionModelWrapper(
		WithWrapperCostTracker(tracker),
		WithWrapperModelName("gpt-4o"),
	)

	wrapped := wrapper.WrapModel(model)

	// Stream should delegate to the underlying model (which returns an error).
	_, err := wrapped.Stream(context.Background(), nil)
	assert.Error(t, err) // flakyModel.Stream always errors

	// No cost should be recorded since Stream doesn't track usage.
	assert.Equal(t, 0, tracker.Calls())
}
