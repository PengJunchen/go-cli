package cli

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestTurnRunnerOverflowUsagePreservesPartialSubtotal(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := calls.Add(1)
		input, finish := 1, "stop"
		if call == 1 {
			input, finish = math.MaxInt, "length"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w,
			`{"choices":[{"message":{"role":"assistant","content":"part"},"finish_reason":%q}],"usage":{"prompt_tokens":%d,"completion_tokens":0,"total_tokens":%d}}`,
			finish, input, input)
	}))
	defer server.Close()

	cfg := newE2ETestConfig(t, server.URL)
	assembly, err := AssembleAgent(t.Context(), cfg, cfg.Provider.Name, cfg.Provider.Model, io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	defer assembly.Cleanup()

	result, err := assembly.TurnRunner.RunTurn(t.Context(), core.Submission{Content: "fixture"})
	require.NoError(t, err)
	t.Logf("turn result: %+v", result)
	require.Equal(t, int32(2), calls.Load(), "the max-token response must continue into a second provider response")

	rows, summary := assembly.CostTracker.AttemptCostSnapshot()
	require.Len(t, rows, 2)
	require.Equal(t, math.MaxInt, summary.KnownInputTokens)
	require.True(t, summary.PrimarySubtotalIncomplete, "overflowed known usage must remain an incomplete subtotal")
	require.Zero(t, summary.UnknownUsageAttempts)
	for _, row := range rows {
		require.Equal(t, llm.UsageKnown, row.Result.Usage.State, "overflow must not rewrite raw provider attempts")
	}

	stream := core.NewEventStream(8)
	emitTokenUsageEvent(stream, assembly)
	event := <-stream.Events()
	require.NotNil(t, event.TokenUsage)
	require.Equal(t, math.MaxInt, event.TokenUsage.InputTokens)
	require.Equal(t, llm.UsageCoveragePartial, event.TokenUsage.PrimaryCoverage)
	require.Equal(t, 0, event.TokenUsage.UnknownUsageAttempts)

	messages := assembly.Agent.Messages()
	require.NotEmpty(t, messages)
	latest := messages[len(messages)-1]
	require.NotNil(t, latest.Usage)
	require.Equal(t, math.MaxInt, latest.Usage.InputTokens)
	require.Equal(t, llm.UsageCoveragePartial, latest.Usage.PrimaryCoverage)
}

func TestTurnRunnerMissingContinuationUsageStaysPartial(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = fmt.Fprintf(w,
				`{"choices":[{"message":{"role":"assistant","content":"part"},"finish_reason":"length"}],"usage":{"prompt_tokens":%d,"completion_tokens":0,"total_tokens":%d}}`,
				math.MaxInt, math.MaxInt)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	cfg := newE2ETestConfig(t, server.URL)
	assembly, err := AssembleAgent(t.Context(), cfg, cfg.Provider.Name, cfg.Provider.Model, io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	defer assembly.Cleanup()

	_, err = assembly.TurnRunner.RunTurn(t.Context(), core.Submission{Content: "fixture"})
	require.NoError(t, err)
	require.Equal(t, int32(2), calls.Load())

	rows, summary := assembly.CostTracker.AttemptCostSnapshot()
	require.Len(t, rows, 2)
	require.Equal(t, math.MaxInt, summary.KnownInputTokens)
	require.False(t, summary.PrimarySubtotalIncomplete, "missing usage is tracked as unknown, separate from an unrepresentable known subtotal")
	require.Equal(t, 1, summary.UnknownUsageAttempts)

	stream := core.NewEventStream(8)
	emitTokenUsageEvent(stream, assembly)
	event := <-stream.Events()
	require.NotNil(t, event.TokenUsage)
	require.Equal(t, math.MaxInt, event.TokenUsage.InputTokens)
	require.Equal(t, llm.UsageCoveragePartial, event.TokenUsage.PrimaryCoverage)
	require.Equal(t, 1, event.TokenUsage.UnknownUsageAttempts)

	messages := assembly.Agent.Messages()
	require.NotEmpty(t, messages)
	latest := messages[len(messages)-1]
	require.NotNil(t, latest.Usage)
	require.Equal(t, math.MaxInt, latest.Usage.InputTokens)
	require.Equal(t, llm.UsageCoveragePartial, latest.Usage.PrimaryCoverage)
}
