package cli

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/production"
)

// stubModel is a test double for llm.BaseChatModel.
type stubModel struct {
	resp *llm.Message
	err  error
}

func (m *stubModel) Generate(_ context.Context, _ []llm.Message, _ ...llm.Option) (*llm.Message, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.resp, nil
}

func (m *stubModel) Stream(_ context.Context, _ []llm.Message, _ ...llm.Option) (<-chan llm.MessageChunk, error) {
	return nil, nil
}

func TestOutputGuardModel_SanitizesPII(t *testing.T) {
	inner := &stubModel{
		resp: &llm.Message{Role: llm.RoleAssistant, Content: "Contact me at john@example.com"},
	}
	m := &outputGuardModel{inner: inner, guard: production.NewPIIOutputGuard()}

	resp, err := m.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.NotContains(t, resp.Content, "john@example.com")
}

func TestOutputGuardModel_PassthroughSafe(t *testing.T) {
	inner := &stubModel{
		resp: &llm.Message{Role: llm.RoleAssistant, Content: "Hello, world!"},
	}
	m := &outputGuardModel{inner: inner, guard: production.NewPIIOutputGuard()}

	resp, err := m.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "Hello, world!", resp.Content)
}

func TestOutputGuardModel_LengthTruncation(t *testing.T) {
	long := string(make([]byte, 200))
	inner := &stubModel{
		resp: &llm.Message{Role: llm.RoleAssistant, Content: long},
	}
	m := &outputGuardModel{inner: inner, guard: production.NewLengthGuard(100)}

	resp, err := m.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(resp.Content), 100)
}

func TestOutputGuardModel_NilGuard(t *testing.T) {
	inner := &stubModel{
		resp: &llm.Message{Role: llm.RoleAssistant, Content: "untouched"},
	}
	m := &outputGuardModel{inner: inner, guard: nil}

	resp, err := m.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "untouched", resp.Content)
}

func TestNewModelWrapper_AppliesBothWrappers(t *testing.T) {
	inner := &stubModel{
		resp: &llm.Message{Role: llm.RoleAssistant, Content: "Contact john@example.com"},
	}
	pw := production.NewProductionModelWrapper()
	guard := production.NewPIIOutputGuard()
	wrapper := newModelWrapper(pw, nil, guard)

	wrapped := wrapper(inner)
	require.NotNil(t, wrapped)

	baseModel, ok := wrapped.(llm.BaseChatModel)
	require.True(t, ok)

	resp, err := baseModel.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.NotContains(t, resp.Content, "john@example.com")
}

func TestNewModelWrapper_NilGuardReturnsProductionWrapped(t *testing.T) {
	inner := &stubModel{
		resp: &llm.Message{Role: llm.RoleAssistant, Content: "safe content"},
	}
	pw := production.NewProductionModelWrapper()
	wrapper := newModelWrapper(pw, nil, nil)

	wrapped := wrapper(inner)
	require.NotNil(t, wrapped)

	_, ok := wrapped.(llm.BaseChatModel)
	require.True(t, ok)
}

func TestNewModelWrapper_NonBaseChatModelReturnsUnchanged(t *testing.T) {
	pw := production.NewProductionModelWrapper()
	wrapper := newModelWrapper(pw, nil, nil)

	result := wrapper("not a model")
	assert.Equal(t, "not a model", result)
}

// countingModel is a test double that counts Generate calls and returns err
// for the first failCount calls, then returns resp.
type countingModel struct {
	calls     int32
	failCount int32
	resp      *llm.Message
	err       error
}

func (m *countingModel) Generate(_ context.Context, _ []llm.Message, _ ...llm.Option) (*llm.Message, error) {
	n := atomic.AddInt32(&m.calls, 1)
	if n <= m.failCount {
		return nil, m.err
	}
	return m.resp, nil
}

func (m *countingModel) Stream(_ context.Context, _ []llm.Message, _ ...llm.Option) (<-chan llm.MessageChunk, error) {
	return nil, nil
}

// TestNewModelWrapperWithChain_AppliesGuard verifies that the chain-based
// wrapper still applies the output guard as the outermost layer.
func TestNewModelWrapperWithChain_AppliesGuard(t *testing.T) {
	inner := &stubModel{
		resp: &llm.Message{Role: llm.RoleAssistant, Content: "Contact john@example.com"},
	}
	pw := production.NewProductionModelWrapper()
	chain := llm.NewStandardMiddlewareChain(
		llm.NewFailoverModelMiddleware(),
		llm.NewRetryModelMiddleware(),
		llm.NewTimeoutModelMiddleware(),
		llm.NewSanitizeModelMiddleware(),
		llm.NewLoopDetectionModelMiddleware(),
		llm.NewValidateModelMiddleware(),
		llm.NewOverflowRecoveryMiddleware(),
	)
	guard := production.NewPIIOutputGuard()
	wrapper := newModelWrapperWithChain(pw, chain, nil, guard)

	wrapped := wrapper(inner)
	require.NotNil(t, wrapped)

	baseModel, ok := wrapped.(llm.BaseChatModel)
	require.True(t, ok)

	resp, err := baseModel.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.NotContains(t, resp.Content, "john@example.com")
}

// TestNewModelWrapperWithChain_NilGuardReturnsChainWrapped verifies that with a
// nil guard the wrapper still returns a usable BaseChatModel.
func TestNewModelWrapperWithChain_NilGuardReturnsChainWrapped(t *testing.T) {
	inner := &stubModel{
		resp: &llm.Message{Role: llm.RoleAssistant, Content: "safe content"},
	}
	pw := production.NewProductionModelWrapper()
	chain := llm.NewStandardMiddlewareChain(
		llm.NewFailoverModelMiddleware(),
		llm.NewRetryModelMiddleware(),
		llm.NewTimeoutModelMiddleware(),
		llm.NewSanitizeModelMiddleware(),
		llm.NewLoopDetectionModelMiddleware(),
		llm.NewValidateModelMiddleware(),
		llm.NewOverflowRecoveryMiddleware(),
	)
	wrapper := newModelWrapperWithChain(pw, chain, nil, nil)

	wrapped := wrapper(inner)
	require.NotNil(t, wrapped)

	_, ok := wrapped.(llm.BaseChatModel)
	require.True(t, ok)
}

// TestNewModelWrapperWithChain_NonBaseChatModelReturnsUnchanged verifies the
// wrapper passes through non-model values unchanged.
func TestNewModelWrapperWithChain_NonBaseChatModelReturnsUnchanged(t *testing.T) {
	pw := production.NewProductionModelWrapper()
	chain := llm.NewStandardMiddlewareChain()
	wrapper := newModelWrapperWithChain(pw, chain, nil, nil)

	result := wrapper("not a model")
	assert.Equal(t, "not a model", result)
}

// TestNoDoubleRetry verifies that when the production wrapper is constructed
// WITHOUT a retry policy and the 7-layer chain provides the retry middleware,
// the underlying model is retried exactly the number of times dictated by the
// chain's retry policy — not doubled by a second retry layer.
func TestNoDoubleRetry(t *testing.T) {
	inner := &countingModel{
		failCount: 2, // fail twice, succeed on 3rd call
		resp:      &llm.Message{Role: llm.RoleAssistant, Content: "recovered"},
		err:       errors.New("transient"),
	}

	// Production wrapper WITHOUT retry policy — cost tracking only.
	pw := production.NewProductionModelWrapper()

	// Chain with a retry policy that allows up to 3 attempts (0, 1, 2).
	chain := llm.NewStandardMiddlewareChain(
		llm.NewRetryModelMiddleware(llm.WithRetryPolicy(&countingRetryPolicy{maxAttempts: 3})),
	)

	wrapper := newModelWrapperWithChain(pw, chain, nil, nil)
	wrapped := wrapper(inner)
	baseModel, ok := wrapped.(llm.BaseChatModel)
	require.True(t, ok)

	resp, err := baseModel.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "recovered", resp.Content)

	// The model should have been called exactly 3 times: initial + 2 retries.
	// If the production wrapper also applied retry, we would see 9 calls
	// (3 × 3) instead of 3.
	assert.Equal(t, int32(3), atomic.LoadInt32(&inner.calls))
}

// countingRetryPolicy is a simple RetryPolicy for tests: retries any non-nil
// error up to maxAttempts times (0-based attempt index).
type countingRetryPolicy struct {
	maxAttempts int
}

func (p *countingRetryPolicy) ShouldRetry(_ context.Context, err error, attempt int) bool {
	if err == nil {
		return false
	}
	return attempt < p.maxAttempts
}

func (p *countingRetryPolicy) NextBackoff(_ context.Context, _ int) time.Duration {
	return 0
}

func (p *countingRetryPolicy) Name() string { return "counting-retry" }
