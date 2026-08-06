//go:build e2e

package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestET_model_cycler_rotation verifies the three core behaviors of
// ModelCycler end-to-end:
//
//  1. Round-robin rotation: consecutive calls cycle through all models in
//     order, wrapping around after the last model.
//  2. Session affinity: the same session ID always routes to the same model,
//     while different session IDs can be assigned different models.
//  3. Fallback: when the selected model returns an error, the call falls back
//     to the primary (wrapped) model.
func TestET_model_cycler_rotation(t *testing.T) {
	// --- Round-robin rotation ---
	t.Run("round_robin_rotation", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		reg := newCyclerTestRegistry(t, nil)
		c := NewModelCycler(ModelCyclerConfig{
			Models:   threeModels,
			Strategy: StrategyRoundRobin,
		}).WithRegistry(reg)

		primary := &fbMockModel{genContent: "primary-ok"}
		wrapped := c.WrapModel(primary)

		// Each provider's Build returns a model whose Generate content is the
		// model name from the config, so we can verify which model was selected
		// by checking resp.Content.
		expected := []string{"gpt-4o", "claude-3", "gemini-pro"}
		for i, want := range expected {
			resp, err := wrapped.Generate(ctx, nil)
			require.NoError(t, err)
			assert.Equal(t, want, resp.Content, "call %d should select model %q", i, want)
		}

		// Fourth call wraps around to the first model.
		resp, err := wrapped.Generate(ctx, nil)
		require.NoError(t, err)
		assert.Equal(t, "gpt-4o", resp.Content, "call 3 should wrap around to first model")

		assert.Equal(t, 0, primary.genCalls,
			"primary should never be called when all selected models succeed")
	})

	// --- Session affinity ---
	t.Run("session_affinity", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		reg := newCyclerTestRegistry(t, nil)
		c := NewModelCycler(ModelCyclerConfig{
			Models:          threeModels,
			Strategy:        StrategyRoundRobin,
			SessionAffinity: true,
		}).WithRegistry(reg)

		primary := &fbMockModel{genContent: "primary-ok"}
		wrapped := c.WrapModel(primary)

		ctxA := WithSessionID(ctx, "session-A")
		ctxB := WithSessionID(ctx, "session-B")

		// First call for session-A selects a model via round-robin (index 0).
		respA1, err := wrapped.Generate(ctxA, nil)
		require.NoError(t, err)
		firstA := respA1.Content
		assert.Contains(t, []string{"gpt-4o", "claude-3", "gemini-pro"}, firstA,
			"first call should select a valid model")

		// Subsequent calls for session-A must always return the same model.
		for i := 0; i < 4; i++ {
			resp, err := wrapped.Generate(ctxA, nil)
			require.NoError(t, err)
			assert.Equal(t, firstA, resp.Content,
				"session-A should always route to the same model (call %d)", i+1)
		}

		// Session-B gets a different model (round-robin index 1).
		respB, err := wrapped.Generate(ctxB, nil)
		require.NoError(t, err)
		assert.NotEqual(t, firstA, respB.Content,
			"session-B should get a different model than session-A")

		// Repeated calls for session-B keep the same assignment.
		respB2, err := wrapped.Generate(ctxB, nil)
		require.NoError(t, err)
		assert.Equal(t, respB.Content, respB2.Content,
			"session-B should keep its model assignment on repeated calls")

		assert.Equal(t, 0, primary.genCalls,
			"primary should never be called when selected models succeed")
	})

	// --- Fallback on error ---
	t.Run("fallback_on_error", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Every built model will fail Generate with an error.
		reg := newCyclerTestRegistry(t, errors.New("selected model down"))
		c := NewModelCycler(ModelCyclerConfig{
			Models:   threeModels,
			Strategy: StrategyRoundRobin,
		}).WithRegistry(reg)

		primary := &fbMockModel{genContent: "primary-ok"}
		wrapped := c.WrapModel(primary)

		resp, err := wrapped.Generate(ctx, nil)
		require.NoError(t, err)
		assert.Equal(t, "primary-ok", resp.Content,
			"should fall back to primary when the selected model fails")
		assert.Equal(t, 1, primary.genCalls,
			"primary should be called exactly once as fallback")

		// A second call also falls back (round-robin advances to the next
		// model, which also fails).
		resp2, err := wrapped.Generate(ctx, nil)
		require.NoError(t, err)
		assert.Equal(t, "primary-ok", resp2.Content,
			"second call should also fall back to primary")
		assert.Equal(t, 2, primary.genCalls,
			"primary should be called twice as fallback")
	})
}
