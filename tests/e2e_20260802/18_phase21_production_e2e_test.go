// Package e2e_20260802 contains end-to-end integration tests.
// This file verifies Phase 21 production resilience wiring: retry,
// cost tracking, and output guards.
package e2e_20260802

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

// =============================================================================
// Phase 21 E2E: Production Resilience Wiring
// =============================================================================

// flakyModel fails for the first N calls, then succeeds.
type flakyModel struct {
	calls        int32
	successAfter int32
	resp         *llm.Message
}

func (m *flakyModel) Generate(_ context.Context, _ []llm.Message, _ ...llm.Option) (*llm.Message, error) {
	n := atomic.AddInt32(&m.calls, 1)
	if n <= m.successAfter {
		return nil, errors.New("connection refused")
	}
	return m.resp, nil
}

func (m *flakyModel) Stream(_ context.Context, _ []llm.Message, _ ...llm.Option) (<-chan llm.MessageChunk, error) {
	return nil, nil
}

// TestE2E_Phase21_RetryOnTransientError verifies that ProductionModelWrapper
// retries failed LLM calls up to MaxAttempts.
func TestE2E_Phase21_RetryOnTransientError(t *testing.T) {
	inner := &flakyModel{
		successAfter: 2, // First 2 calls fail, 3rd succeeds
		resp:         &llm.Message{Role: llm.RoleAssistant, Content: "recovered"},
	}

	retryPolicy := production.NewDefaultRetryPolicy(production.RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   10 * time.Millisecond,
		MaxDelay:    100 * time.Millisecond,
	})
	pw := production.NewProductionModelWrapper(
		production.WithWrapperRetryPolicy(retryPolicy),
	)

	wrapped := pw.WrapModel(inner)
	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "recovered", resp.Content)
	assert.Equal(t, int32(3), atomic.LoadInt32(&inner.calls))
}

// TestE2E_Phase21_RetryExhaustedReturnsError verifies that after exhausting
// retries, the error is returned to the caller.
func TestE2E_Phase21_RetryExhaustedReturnsError(t *testing.T) {
	inner := &flakyModel{
		successAfter: 99, // Always fails
		resp:         &llm.Message{Role: llm.RoleAssistant, Content: "never"},
	}

	retryPolicy := production.NewDefaultRetryPolicy(production.RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   10 * time.Millisecond,
		MaxDelay:    100 * time.Millisecond,
	})
	pw := production.NewProductionModelWrapper(
		production.WithWrapperRetryPolicy(retryPolicy),
	)

	wrapped := pw.WrapModel(inner)
	_, err := wrapped.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.Equal(t, int32(4), atomic.LoadInt32(&inner.calls))
}

// TestE2E_Phase21_CostTrackerRecordsUsage verifies that CostTracker records
// token usage from successful LLM calls.
func TestE2E_Phase21_CostTrackerRecordsUsage(t *testing.T) {
	inner := &flakyModel{
		successAfter: 0, // Always succeeds
		resp: &llm.Message{
			Role:    llm.RoleAssistant,
			Content: "hello",
			Usage:   &llm.Usage{InputTokens: 100, OutputTokens: 50},
		},
	}

	costTracker := production.NewCostTracker(nil)
	pw := production.NewProductionModelWrapper(
		production.WithWrapperCostTracker(costTracker),
		production.WithWrapperModelName("gpt-4o-mini"),
	)

	wrapped := pw.WrapModel(inner)
	_, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)

	assert.Equal(t, 1, costTracker.Calls())
	assert.Greater(t, costTracker.Total(), 0.0)
}

// TestE2E_Phase21_OutputGuardBlocksPII verifies that PIIOutputGuard detects
// email addresses in model output.
func TestE2E_Phase21_OutputGuardBlocksPII(t *testing.T) {
	chain := production.NewOutputGuardChain([]production.OutputGuard{
		production.NewPIIOutputGuard(),
	})

	result, err := chain.Check(context.Background(), "Contact john@example.com")
	require.NoError(t, err)
	assert.False(t, result.Allowed)
}

// TestE2E_Phase21_OutputGuardBlocksCodeInjection verifies that
// CodeInjectionGuard detects SQL injection patterns.
func TestE2E_Phase21_OutputGuardBlocksCodeInjection(t *testing.T) {
	chain := production.NewOutputGuardChain([]production.OutputGuard{
		production.NewCodeInjectionGuard(),
	})

	result, err := chain.Check(context.Background(), "Run this: DROP TABLE users;")
	require.NoError(t, err)
	assert.False(t, result.Allowed)
}

// TestE2E_Phase21_OutputGuardTruncatesLong verifies that LengthGuard
// truncates overly long output.
func TestE2E_Phase21_OutputGuardTruncatesLong(t *testing.T) {
	chain := production.NewOutputGuardChain([]production.OutputGuard{
		production.NewLengthGuard(100),
	})

	long := string(make([]byte, 200))
	result, err := chain.Check(context.Background(), long)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(result.Sanitized), 100)
}

// TestE2E_Phase21_OutputGuardAllowsSafeText verifies that normal output
// passes through the guard chain unchanged.
func TestE2E_Phase21_OutputGuardAllowsSafeText(t *testing.T) {
	chain := production.NewOutputGuardChain([]production.OutputGuard{
		production.NewPIIOutputGuard(),
		production.NewCodeInjectionGuard(),
		production.NewLengthGuard(100000),
	})

	result, err := chain.Check(context.Background(), "Hello, world!")
	require.NoError(t, err)
	assert.True(t, result.Allowed)
	assert.Equal(t, "Hello, world!", result.Sanitized)
}

// TestE2E_Phase21_CombinedProductionAndGuard verifies that retry, cost
// tracking, and output guards work together in the full wrapper chain.
func TestE2E_Phase21_CombinedProductionAndGuard(t *testing.T) {
	inner := &flakyModel{
		successAfter: 1, // First call fails, second succeeds
		resp: &llm.Message{
			Role:    llm.RoleAssistant,
			Content: "Contact john@example.com",
			Usage:   &llm.Usage{InputTokens: 50, OutputTokens: 25},
		},
	}

	costTracker := production.NewCostTracker(nil)
	retryPolicy := production.NewDefaultRetryPolicy(production.RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   10 * time.Millisecond,
		MaxDelay:    100 * time.Millisecond,
	})
	pw := production.NewProductionModelWrapper(
		production.WithWrapperRetryPolicy(retryPolicy),
		production.WithWrapperCostTracker(costTracker),
		production.WithWrapperModelName("gpt-4o-mini"),
	)

	wrapped := pw.WrapModel(inner)
	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)

	// Retry happened (2 calls total: 1 fail + 1 success)
	assert.Equal(t, int32(2), atomic.LoadInt32(&inner.calls))

	// Cost tracked from successful call
	assert.Equal(t, 1, costTracker.Calls())
	assert.Greater(t, costTracker.Total(), 0.0)

	// Output guard should detect PII in the response
	guard := production.NewPIIOutputGuard()
	result, gErr := guard.Check(context.Background(), resp.Content)
	require.NoError(t, gErr)
	assert.False(t, result.Allowed)
}
