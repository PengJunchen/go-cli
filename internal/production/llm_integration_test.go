package production

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
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

func TestLoopRetriesLiveHTTPClientTimeoutWithProductionRetryPolicy(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			time.Sleep(150 * time.Millisecond)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"recovered\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := &http.Client{Transport: server.Client().Transport, Timeout: 50 * time.Millisecond}
	registry := llm.NewProviderRegistry()
	require.NoError(t, registry.Register(llm.NewEinoProvider(
		llm.WithProviderName("timeout-retry"),
		llm.WithHTTPClient(client),
	)))
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID: "timeout-retry",
		ModelID:    "timeout-retry-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	defer cleanup()

	policy := NewDefaultRetryPolicy(RetryConfig{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond})
	retried := llm.NewRetryModelMiddleware(llm.WithRetryPolicy(policy)).WrapModel(model)
	loop := core.NewLoopAgent(
		core.WithLLM(retried),
		core.WithMaxIterations(1),
		core.WithProviderRetryPolicy(policy),
	)
	result, err := loop.RunCanonical(context.Background(), core.Submission{Content: "retry the transport timeout"})
	require.NoError(t, err)
	require.NotEmpty(t, result.Transcript)
	assert.Equal(t, "recovered", result.Transcript[len(result.Transcript)-1].Message.Content)
	assert.EqualValues(t, 2, requests.Load(), "one timed-out Do and one successful retry should be dispatched")
}

func TestProductionModelWrapperMaxAttemptsBoundsTotalDispatches(t *testing.T) {
	for _, maxAttempts := range []int{1, 3} {
		t.Run(fmt.Sprintf("max_attempts_%d", maxAttempts), func(t *testing.T) {
			model := &flakyModel{maxFails: 100}
			policy := NewDefaultRetryPolicy(RetryConfig{
				MaxAttempts: maxAttempts,
				BaseDelay:   time.Nanosecond,
			})
			wrapped := NewProductionModelWrapper(WithWrapperRetryPolicy(policy)).WrapModel(model)

			_, err := wrapped.Generate(context.Background(), nil)
			require.Error(t, err)
			model.mu.Lock()
			assert.Equal(t, maxAttempts, model.failCount, "MaxAttempts is the total provider dispatch count")
			model.mu.Unlock()
		})
	}
}

type cancelDuringBackoffPolicy struct {
	cancel context.CancelFunc
}

func (p *cancelDuringBackoffPolicy) ShouldRetry(context.Context, error, int) bool { return true }

func (p *cancelDuringBackoffPolicy) NextBackoff(context.Context, int) time.Duration {
	p.cancel()
	return -time.Nanosecond
}

func (p *cancelDuringBackoffPolicy) Name() string { return "cancel-during-backoff" }

type retryBoundaryModel struct {
	generateCalls atomic.Int32
	streamCalls   atomic.Int32
}

func (m *retryBoundaryModel) Generate(context.Context, []llm.Message, ...llm.Option) (*llm.Message, error) {
	m.generateCalls.Add(1)
	return nil, errors.New("provider generate failure")
}

func (m *retryBoundaryModel) Stream(context.Context, []llm.Message, ...llm.Option) (<-chan llm.MessageChunk, error) {
	m.streamCalls.Add(1)
	return nil, errors.New("provider stream initialization failure")
}

var _ llm.BaseChatModel = (*retryBoundaryModel)(nil)

func TestProductionModelWrapperDoesNotRedispatchAfterCanceledGenerateBackoff(t *testing.T) {
	for i := 0; i < 32; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		model := &retryBoundaryModel{}
		policy := &cancelDuringBackoffPolicy{cancel: cancel}
		wrapped := NewProductionModelWrapper(WithWrapperRetryPolicy(policy)).WrapModel(model)

		_, err := wrapped.Generate(ctx, nil)
		require.Error(t, err)
		assert.Equal(t, int32(1), model.generateCalls.Load(), "cancellation must fence the next provider dispatch")
		cancel()
	}
}

func TestProductionModelWrapperDoesNotRedispatchAfterCanceledStreamInitBackoff(t *testing.T) {
	for i := 0; i < 32; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		model := &retryBoundaryModel{}
		policy := &cancelDuringBackoffPolicy{cancel: cancel}
		wrapped := NewProductionModelWrapper(WithWrapperRetryPolicy(policy)).WrapModel(model)

		_, err := wrapped.Stream(ctx, nil)
		require.Error(t, err)
		assert.Equal(t, int32(1), model.streamCalls.Load(), "cancellation must fence the next stream dispatch")
		cancel()
	}
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
