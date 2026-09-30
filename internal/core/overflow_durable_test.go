package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
)

type overflowProjectionSink struct {
	*consumerTestSink
	committed atomic.Bool
}

func (sink *overflowProjectionSink) CompactionSourceSnapshot() (CompactionSourceSnapshot, error) {
	return CompactionSourceSnapshot{
		Revision:       "overflow-revision",
		BranchID:       "main",
		Messages:       []AgentMessage{{Role: string(llm.RoleUser), Content: "prior durable context"}},
		SourceEntryIDs: []string{"overflow-prior-entry"},
	}, nil
}

func (sink *overflowProjectionSink) CommitCompaction(ctx context.Context, projection CompactionProjection) error {
	if err := sink.consumerTestSink.CommitCompaction(ctx, projection); err != nil {
		return err
	}
	sink.committed.Store(true)
	return nil
}

type overflowSummaryCompactor struct {
	onCompact func(context.Context, []compaction.TurnItem)
}

func (compactor overflowSummaryCompactor) Compact(ctx context.Context, items []compaction.TurnItem, _ int, _ compaction.TokenEstimator) (compaction.CompactionResult, error) {
	if compactor.onCompact != nil {
		compactor.onCompact(ctx, items)
	}
	var runID string
	through := llm.SourceThrough{}
	var retained []compaction.TurnItem
	for _, item := range items {
		source := item.Runtime.Source
		switch source.Kind {
		case llm.SourcePriorPublished:
			if source.PriorOrdinal+1 > through.PriorHistoryCount {
				through.PriorHistoryCount = source.PriorOrdinal + 1
			}
		case llm.SourceRunTranscript:
			runID = source.RunID
			retained = append(retained, item)
		}
	}
	resultItems := []compaction.TurnItem{
		items[0],
		{
			ID:           "overflow-summary",
			Role:         compaction.RoleUser,
			Content:      "compacted context",
			IsCompaction: true,
			Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{
				Kind: llm.SourceRunSummary, RunID: runID, Through: through,
			}},
		},
	}
	resultItems = append(resultItems, retained...)
	result := compaction.CompactionResult{
		Items:         resultItems,
		Strategy:      compaction.StrategySummary,
		NewProjection: &compaction.NewProjection{Summary: "compacted context", Through: through},
	}
	return result, nil
}

func TestDurableOverflowRetryWaitsForSynchronizedProjection(t *testing.T) {
	sink := &overflowProjectionSink{consumerTestSink: &consumerTestSink{}}
	var requests atomic.Int32
	var secondRequestSawProjection atomic.Bool
	var secondRequestSawCurrent atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requestNumber := requests.Add(1)
		if requestNumber == 1 {
			http.Error(w, "context_length_exceeded", http.StatusBadRequest)
			return
		}
		secondRequestSawProjection.Store(sink.committed.Load())
		currentInputCount := 0
		for _, message := range request.Messages {
			if message.Content == "current durable request" {
				currentInputCount++
			}
		}
		secondRequestSawCurrent.Store(currentInputCount == 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"durable recovery\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	model, cleanup := buildOverflowTestModel(t, server)
	defer cleanup()
	recorder := &attemptResultRecorder{}
	loop := NewLoopAgent(WithLLM(model), WithMaxIterations(1), WithProviderAttemptRecorder(recorder.record))
	loop.SetMidTurnCompaction(nil, overflowSummaryCompactor{}, compaction.NewHeuristicTokenEstimator(), 10000)
	agent := NewAgentImpl("durable-overflow", loop)
	require.NoError(t, agent.SetHistoryFromSourceSnapshot(CompactionSourceSnapshot{
		Revision: "overflow-revision", BranchID: "main",
		Messages:       []AgentMessage{{Role: string(llm.RoleUser), Content: "prior durable context"}},
		SourceEntryIDs: []string{"overflow-prior-entry"},
	}))
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "overflow-run"}, nil)
	owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "overflow-session", WorkspaceID: "overflow-workspace"})

	result, runErr := owner.run(time.Second, func(ctx context.Context) (Result, error) {
		return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "current durable request"})
	})
	require.NoError(t, runErr)
	require.Equal(t, "durable recovery", result.Message)
	require.Equal(t, 2, int(requests.Load()))
	require.True(t, secondRequestSawProjection.Load(), "the durable projection must be committed before the retry crosses client.Do")
	require.True(t, secondRequestSawCurrent.Load(), "the overflow retry must retain the exact current input exactly once")
	steps, _, _, _ := sink.snapshot()
	require.Contains(t, steps, "compaction")
	require.Len(t, recorder.snapshot(), 2)
}

func TestDurableOverflowRetryFailsClosedWithoutCapturedSourceSnapshot(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "context_length_exceeded", http.StatusBadRequest)
	}))
	defer server.Close()

	model, cleanup := buildOverflowTestModel(t, server)
	defer cleanup()
	loop := NewLoopAgent(WithLLM(model), WithMaxIterations(1))
	loop.SetMidTurnCompaction(nil, overflowSummaryCompactor{}, compaction.NewHeuristicTokenEstimator(), 10000)
	agent := NewAgentImpl("durable-overflow-no-source", loop)
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "overflow-no-source-run"}, nil)
	owner.ctx = WithRunCommit(owner.ctx, &consumerTestSink{}, RunCommitBinding{SessionID: "overflow-session", WorkspaceID: "overflow-workspace"})

	_, runErr := owner.run(time.Second, func(ctx context.Context) (Result, error) {
		return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "current durable request"})
	})
	require.ErrorIs(t, runErr, ErrRunCommitCompactionProjectionUnavailable)
	require.Equal(t, 1, int(requests.Load()), "no second HTTP request may occur without a captured source view")
}

func buildOverflowTestModel(t *testing.T, server *httptest.Server) (llm.BaseChatModel, func()) {
	t.Helper()
	registry := llm.NewProviderRegistry()
	require.NoError(t, registry.Register(llm.NewEinoProvider(
		llm.WithProviderName("core-durable-overflow-test"),
		llm.WithHTTPClient(server.Client()),
	)))
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID: "core-durable-overflow-test",
		ModelID:    "core-durable-overflow-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	model = llm.NewOverflowRecoveryMiddleware(llm.WithOverflowMaxRetries(3)).WrapModel(model)
	return attemptGuardedModel{BaseChatModel: model}, cleanup
}
