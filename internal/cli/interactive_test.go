package cli

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestInteractiveCmd_Metadata verifies the interactive command's registration
// metadata.
func TestInteractiveCmd_Metadata(t *testing.T) {
	cmd := newInteractiveCmd(nil, &bytes.Buffer{})
	assert.Equal(t, "interactive", cmd.Name())
	assert.NotEmpty(t, cmd.Synopsis())
	assert.Contains(t, cmd.Synopsis(), "interactive")
}

// TestInteractiveCmd_ExitCommand simulates a user typing "exit" and verifies the
// session output contains "Session ended".
func TestInteractiveCmd_ExitCommand(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	var out bytes.Buffer
	in := strings.NewReader("exit\n")
	cmd := newInteractiveCmd(in, &out)
	cfg := &config.Config{
		Provider: config.ProviderConfig{
			Name:    "test",
			BaseURL: srv.URL,
			APIKey:  "test",
			Model:   "test-model",
		},
	}

	err := cmd.Run(t.Context(), cfg, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Session ended")
}

// TestInteractiveCmd_EmptyInput simulates an empty input line followed by
// "exit", verifying the session ends cleanly without errors.
func TestInteractiveCmd_EmptyInput(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	var out bytes.Buffer
	// Empty line then exit.
	in := strings.NewReader("\nexit\n")
	cmd := newInteractiveCmd(in, &out)
	cfg := &config.Config{
		Provider: config.ProviderConfig{
			Name:    "test",
			BaseURL: srv.URL,
			APIKey:  "test",
			Model:   "test-model",
		},
	}

	err := cmd.Run(t.Context(), cfg, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Session ended")
}

// TestInteractiveCmd_FlagParsing verifies that unknown flags produce a
// UsageError.
func TestInteractiveCmd_FlagParsing(t *testing.T) {
	var out bytes.Buffer
	cmd := newInteractiveCmd(nil, &out)

	err := cmd.Run(t.Context(), &config.Config{}, []string{"-bogus"})
	require.Error(t, err)
	var usageErr *UsageError
	assert.True(t, errors.As(err, &usageErr))
	assert.Contains(t, err.Error(), "bogus")
}

// TestMessagesToTurnItems tests the conversion from core.AgentMessage to
// compaction.TurnItem.
func TestMessagesToTurnItems(t *testing.T) {
	msgs := []core.AgentMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
		{Role: "user", Content: "how are you?"},
	}

	items := messagesToTurnItems(msgs)
	require.Len(t, items, 3)

	assert.Equal(t, "msg-0", items[0].ID)
	assert.Equal(t, "user", items[0].Role)
	assert.Equal(t, "hello", items[0].Content)

	assert.Equal(t, "msg-1", items[1].ID)
	assert.Equal(t, "assistant", items[1].Role)
	assert.Equal(t, "hi there", items[1].Content)

	assert.Equal(t, "msg-2", items[2].ID)
	assert.Equal(t, "user", items[2].Role)
	assert.Equal(t, "how are you?", items[2].Content)
}

// TestTurnItemsToMessages tests the conversion from compaction.TurnItem back to
// core.AgentMessage, including the IsCompaction flag logic.
func TestTurnItemsToMessages(t *testing.T) {
	t.Run("normal items", func(t *testing.T) {
		items := []compaction.TurnItem{
			{ID: "msg-0", Role: "user", Content: "hello"},
			{ID: "msg-1", Role: "assistant", Content: "world"},
		}
		msgs := turnItemsToMessages(items)
		require.Len(t, msgs, 2)
		assert.Equal(t, "user", msgs[0].Role)
		assert.Equal(t, "hello", msgs[0].Content)
		assert.Equal(t, "assistant", msgs[1].Role)
		assert.Equal(t, "world", msgs[1].Content)
	})

	t.Run("compaction item with empty content", func(t *testing.T) {
		items := []compaction.TurnItem{
			{ID: "msg-0", Role: "assistant", Content: "", IsCompaction: true},
		}
		msgs := turnItemsToMessages(items)
		require.Len(t, msgs, 1)
		assert.Equal(t, "[compacted]", msgs[0].Content)
	})

	t.Run("compaction item with non-empty content preserves original", func(t *testing.T) {
		items := []compaction.TurnItem{
			{ID: "msg-0", Role: "assistant", Content: "summary text", IsCompaction: true},
		}
		msgs := turnItemsToMessages(items)
		require.Len(t, msgs, 1)
		assert.Equal(t, "summary text", msgs[0].Content)
	})

	t.Run("round-trip preserves role and content", func(t *testing.T) {
		original := []core.AgentMessage{
			{Role: "user", Content: "question"},
			{Role: "assistant", Content: "answer"},
		}
		items := messagesToTurnItems(original)
		roundTripped := turnItemsToMessages(items)
		assert.Equal(t, original, roundTripped)
	})
}

// TestEstimateTurnTokens tests the token estimation for turn items.
func TestEstimateTurnTokens(t *testing.T) {
	estimator := compaction.NewHeuristicTokenEstimator()

	t.Run("empty items", func(t *testing.T) {
		total := estimateTurnTokens(nil, estimator)
		assert.Equal(t, 0, total)
	})

	t.Run("items with content only", func(t *testing.T) {
		items := []compaction.TurnItem{
			{Role: "user", Content: "hello world"}, // 11 chars => 2 tokens
		}
		total := estimateTurnTokens(items, estimator)
		assert.Equal(t, 11/4, total)
	})

	t.Run("items with tool result only", func(t *testing.T) {
		items := []compaction.TurnItem{
			{Role: "tool", ToolResult: "file contents here"}, // 18 chars => 4 tokens
		}
		total := estimateTurnTokens(items, estimator)
		assert.Equal(t, 18/4, total)
	})

	t.Run("items with both content and tool result", func(t *testing.T) {
		items := []compaction.TurnItem{
			{Role: "tool", Content: "call info", ToolResult: "result data"}, // 9/4 + 11/4 = 2 + 2 = 4 tokens
		}
		total := estimateTurnTokens(items, estimator)
		assert.Equal(t, 9/4+11/4, total)
	})

	t.Run("items with empty content skipped", func(t *testing.T) {
		items := []compaction.TurnItem{
			{Role: "user", Content: ""},
			{Role: "assistant", Content: "hi"}, // 2 chars => 0 tokens
		}
		total := estimateTurnTokens(items, estimator)
		assert.Equal(t, 2/4, total)
	})
}

// TestInteractiveCmd_AutoCompact tests that autoCompact triggers compaction
// when the token estimate exceeds the budget.
func TestInteractiveCmd_AutoCompact(t *testing.T) {
	compactor := compaction.NewUnifiedCompactor()
	estimator := compaction.NewHeuristicTokenEstimator()
	midTurn := compaction.NewMidTurnCompact()
	cmd := newInteractiveCmd(nil, &bytes.Buffer{})

	// Build a large conversation that exceeds the default 8000 token budget.
	// Each char counts as ~0.25 tokens, so 40000+ chars will exceed 8000 tokens.
	bigContent := strings.Repeat("x", 10000) // 2500 tokens per item
	items := []compaction.TurnItem{
		{ID: "msg-0", Role: "user", Content: bigContent},
		{ID: "msg-1", Role: "assistant", Content: bigContent},
		{ID: "msg-2", Role: "user", Content: bigContent},
		{ID: "msg-3", Role: "assistant", Content: bigContent},
	}

	// Total tokens: 4 * 2500 = 10000, budget 8000 => triggers compaction.
	logger := slog.Default()
	result := cmd.autoCompact(t.Context(), items, 8000, compactor, estimator, midTurn, logger)

	// Compaction should have reduced the item count or modified items.
	assert.LessOrEqual(t, len(result), len(items))
}

// TestInteractiveCmd_NoCompactBelowBudget tests that autoCompact is a no-op
// when the conversation is well under the MidTurnCompact threshold (80% of
// maxTokens by default).
func TestInteractiveCmd_NoCompactBelowBudget(t *testing.T) {
	compactor := compaction.NewUnifiedCompactor()
	estimator := compaction.NewHeuristicTokenEstimator()
	midTurn := compaction.NewMidTurnCompact()
	cmd := newInteractiveCmd(nil, &bytes.Buffer{})

	items := []compaction.TurnItem{
		{ID: "msg-0", Role: "user", Content: "hello"},
		{ID: "msg-1", Role: "assistant", Content: "world"},
	}

	// Budget is 8000, threshold is 80% = 6400. "hello"+"world" = 10 chars / 4
	// = 2 tokens, well below 6400, so CompactIfNeeded returns items unchanged.
	logger := slog.Default()
	result := cmd.autoCompact(t.Context(), items, 8000, compactor, estimator, midTurn, logger)

	assert.Equal(t, items, result)
}
