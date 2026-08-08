package compaction

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// newTracedCtx returns a context whose spans are collected by a fresh
// MockTraceExporter, together with the exporter for assertions.
func newTracedCtx(t testing.TB) (context.Context, *mock.MockTraceExporter) {
	t.Helper()
	exp := mock.NewMockTraceExporter()
	tracer := tracing.NewTracer("compaction-test", exp)
	span, ctx := tracer.Start(context.Background(), "compaction.root", tracing.SpanKindInternal)
	span.End() // collect the root synchronously; child spans resolve via ctx tracer
	return ctx, exp
}

// assertSpanEventually waits (briefly) for an asynchronously exported span with
// the given name, since End() exports on a background goroutine.
func assertSpanEventually(t testing.TB, exp *mock.MockTraceExporter, name string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range exp.Spans() {
			if s.Name == name {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected span %q not found", name)
}

// tracedEstimator is a convenience for tests that need a real estimator.
func tracedEstimator() *HeuristicTokenEstimator {
	return NewHeuristicTokenEstimator()
}

func TestStrategyString(t *testing.T) {
	assert.Equal(t, "micro", StrategyMicro.String())
	assert.Equal(t, "summary", StrategySummary.String())
	assert.Equal(t, "truncating", StrategyTruncating.String())
	assert.Equal(t, "none", StrategyNone.String())
	assert.Equal(t, "none", Strategy(99).String())
}

func TestTurnItemJSONRoundTrip(t *testing.T) {
	item := TurnItem{
		ID:              "t1",
		ParentID:        "t0",
		Role:            RoleTool,
		ToolName:        "read",
		ToolResult:      "file contents",
		IsCompaction:    false,
		EstimatedTokens: 4,
	}
	data, err := json.Marshal(item)
	require.NoError(t, err)

	var out TurnItem
	require.NoError(t, json.Unmarshal(data, &out))
	assert.Equal(t, item, out)
}

func TestEstimateLengthFallsBackToHeuristicOnEstimatorError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	errEst := &erroringEstimator{}
	// estimateLength uses the heuristic fallback when the estimator errors.
	assert.Equal(t, len("abcdefgh")/4, estimateLength("abcdefgh", errEst))
}

// erroringEstimator always returns an error to exercise the fallback path.
type erroringEstimator struct{}

func (erroringEstimator) Estimate(_ string) (int, error) {
	return 0, assert.AnError
}

func TestErrRequiresTruncating(t *testing.T) {
	assert.Error(t, ErrRequiresTruncating)
	assert.Contains(t, ErrRequiresTruncating.Error(), "truncating")
}

// TestTurnItemJSONRoundTrip_NewFieldsOmitEmpty verifies that the new
// ContentBlocks, ToolCalls, and ToolCallID fields are omitted from JSON when
// empty, and that a round-trip with populated values preserves them.
func TestTurnItemJSONRoundTrip_NewFieldsOmitEmpty(t *testing.T) {
	// Empty fields should not appear in JSON.
	empty := TurnItem{ID: "t1", Role: RoleUser, Content: "hi"}
	data, err := json.Marshal(empty)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "content_blocks")
	assert.NotContains(t, string(data), "tool_calls")
	assert.NotContains(t, string(data), "tool_call_id")

	// Populated fields should survive a round-trip.
	populated := TurnItem{
		ID:      "t2",
		Role:    RoleAssistant,
		Content: "calling tool",
		ContentBlocks: []llm.ContentBlock{
			{Type: "text", Text: "hello"},
		},
		ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "read", Args: map[string]any{"path": "/tmp"}},
		},
		ToolCallID: "call-1",
	}
	data, err = json.Marshal(populated)
	require.NoError(t, err)

	var out TurnItem
	require.NoError(t, json.Unmarshal(data, &out))
	assert.Equal(t, populated.ContentBlocks, out.ContentBlocks)
	assert.Equal(t, populated.ToolCalls, out.ToolCalls)
	assert.Equal(t, populated.ToolCallID, out.ToolCallID)
}
