package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/production"
)

const coverageWireRequirement = "NEW_REQUIREMENT: negative quantities must return ErrNegative and never be silently clamped."
const coverageWireFailure = "FAIL TestNegativeQuantity: negative quantity was silently accepted; expected ErrNegative."

func coverageWireHistory() []compaction.TurnItem {
	var items []compaction.TurnItem
	add := func(item compaction.TurnItem) {
		index := uint64(len(items))
		item.Runtime.Source = llm.SourceRef{Kind: llm.SourcePriorPublished, HistoryRevision: "rev", BranchID: "main", SourceEntryID: fmt.Sprint(index), PriorOrdinal: index}
		item.Runtime.ReceiptOutcome = item.ReceiptOutcome
		items = append(items, item)
	}
	group := func(index int, body, outcome string) {
		id := fmt.Sprintf("call-%d", index)
		add(compaction.TurnItem{Role: compaction.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: id, Name: "bash"}}})
		add(compaction.TurnItem{Role: compaction.RoleTool, ToolCallID: id, ToolName: "bash", ReceiptOutcome: outcome, ToolResult: body})
	}
	add(compaction.TurnItem{Role: compaction.RoleUser, Content: "Inspect the package before changing it."})
	for i := 0; i < 4; i++ {
		group(i, strings.Repeat("package dependency import path\n", 500), "success")
	}
	add(compaction.TurnItem{Role: compaction.RoleUser, Content: coverageWireRequirement})
	group(4, coverageWireFailure, "error")
	for i := 5; i < 9; i++ {
		group(i, strings.Repeat("package dependency import path\n", 500), "success")
	}
	count := len(items)
	return append(items, compaction.TurnItem{Role: compaction.RoleUser, Content: "Continue the implementation.", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunTranscript, RunID: "run", PriorSourceCount: uint64(count)}}})
}

type coverageWireFactory struct {
	next  llm.RunOperationFactory
	count int
}

func (f *coverageWireFactory) NewOperation(ctx context.Context, kind llm.ProviderOperationKind, max int, candidates []llm.ResolvedCandidateHandle) (llm.OperationDispatchAuthority, error) {
	f.count++
	return f.next.NewOperation(ctx, kind, max, candidates)
}

func coverageWireState(t *testing.T, endpoint, strategy string) (*assembleState, *production.CostTracker, *coverageWireFactory, context.Context) {
	t.Helper()
	raw := fmt.Sprintf(`{"provider":{"name":"openai","base_url":%q},"small_model":{"model":"fixture-model","max_tokens":4096},"compaction":{"strategy":%q}}`, endpoint, strategy)
	cfg, err := config.NewLoader().WithEnvPrefix("SUMMARY_COVERAGE_TEST").WithFileBytes("fixture.json", []byte(raw)).Load(context.Background())
	require.NoError(t, err)
	small, _, cleanup, err := buildSmallModel(context.Background(), cfg, "openai", "openai", "fixture-model")
	require.NoError(t, err)
	if cleanup != nil {
		t.Cleanup(cleanup)
	}
	state := &assembleState{ctx: context.Background(), rc: cfg, ac: assembleConfig{maxTokens: 2500}, logger: slog.Default(), reg: &core.DefaultRegistry{}, smallModel: small}
	require.NoError(t, state.assembleCompactor())
	tracker := production.NewCostTracker(nil)
	ctx := core.WithAuxiliaryOperations(context.Background(), func(r llm.ProviderAttemptResult) error { _, err := tracker.RecordAttempt(r); return err }, nil)
	base, ok := llm.RunOperationFactoryFromContext(ctx)
	require.True(t, ok)
	factory := &coverageWireFactory{next: base}
	ctx = llm.WithRunOperationFactory(ctx, factory)
	return state, tracker, factory, ctx
}

func TestCoverageAssembledProviderReceivesSourcesAndProjection(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad fixture", http.StatusBadRequest)
			return
		}
		bodies <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"provider accepted summary"},"finish_reason":"stop"}],"usage":{"prompt_tokens":77,"completion_tokens":9}}`)
	}))
	defer server.Close()
	state, tracker, factory, ctx := coverageWireState(t, server.URL, "unified")
	items := coverageWireHistory()
	original := append([]compaction.TurnItem(nil), items...)
	result, err := state.compactor.Compact(ctx, items, 2500, state.estimator)
	require.NoError(t, err)
	require.Equal(t, original, items)
	require.Equal(t, compaction.StrategySummary, result.Strategy)
	require.NotNil(t, result.NewProjection)
	require.Equal(t, "provider accepted summary", result.NewProjection.Summary)
	require.Equal(t, llm.SourceThrough{PriorHistoryCount: 20}, result.NewProjection.Through)
	require.Equal(t, "provider accepted summary", result.Items[len(result.Items)-2].Content)
	retained := result.Items[len(result.Items)-1]
	retained.EstimatedTokens = 0 // router populates this derived cache on its copy
	require.Equal(t, items[len(items)-1], retained)
	body := <-bodies
	messages, ok := body["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 1)
	message, ok := messages[0].(map[string]any)
	require.True(t, ok)
	prompt, ok := message["content"].(string)
	require.True(t, ok)
	require.Contains(t, prompt, coverageWireRequirement)
	require.Contains(t, prompt, coverageWireFailure)
	require.Contains(t, prompt, `[tool result name="bash" outcome="error"]`)
	require.Contains(t, prompt, "[truncated]")
	require.True(t, utf8.ValidString(prompt))
	promptTokens, err := state.estimator.Estimate(prompt)
	require.NoError(t, err)
	require.LessOrEqual(t, promptTokens, 6000)
	require.Equal(t, 1, factory.count)
	records, usage := tracker.AttemptCostSnapshot()
	require.Len(t, records, 1)
	require.Equal(t, llm.AttemptSuccess, records[0].Result.Outcome)
	require.Equal(t, llm.UsageKnown, records[0].Result.Usage.State)
	require.Equal(t, 77, usage.KnownInputTokens)
	require.Equal(t, 9, usage.KnownOutputTokens)
}

func TestCoverageAssembledOverflowMakesNoProviderAttempt(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected provider request", http.StatusBadRequest)
	}))
	defer server.Close()
	state, tracker, factory, ctx := coverageWireState(t, server.URL, "unified")
	items := coverageWireHistory()
	items[9].Content = strings.Repeat("很重要的约束", 8000)
	original := append([]compaction.TurnItem(nil), items...)
	result, err := state.compactor.Compact(ctx, items, 2500, state.estimator)
	require.ErrorContains(t, err, "essential summary input")
	require.Equal(t, original, items)
	require.Equal(t, original, result.Items)
	require.Nil(t, result.NewProjection)
	require.Zero(t, requests.Load())
	require.Zero(t, factory.count)
	records, usage := tracker.AttemptCostSnapshot()
	require.Empty(t, records)
	require.Zero(t, usage.Attempts)
}

func TestCoverageAssembledProviderFailureKeepsUnknownUsage(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"synthetic summary rejection"}}`)
	}))
	defer server.Close()
	state, tracker, factory, ctx := coverageWireState(t, server.URL, "unified")
	items := coverageWireHistory()
	original := append([]compaction.TurnItem(nil), items...)
	result, err := state.compactor.Compact(ctx, items, 2500, state.estimator)
	require.ErrorContains(t, err, "summary source failed")
	require.Equal(t, original, items)
	require.Nil(t, result.NewProjection)
	require.EqualValues(t, 1, requests.Load())
	require.Equal(t, 1, factory.count)
	records, usage := tracker.AttemptCostSnapshot()
	require.Len(t, records, 1)
	require.Equal(t, llm.UsageUnknown, records[0].Result.Usage.State)
	require.Nil(t, records[0].Result.Usage.Usage)
	require.Equal(t, 1, usage.UnknownUsageAttempts)
	require.Equal(t, 1, usage.Attempts)
}
