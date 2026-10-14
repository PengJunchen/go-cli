package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
)

type requestEnvelopeSummarySink struct {
	*consumerTestSink
	committed atomic.Bool
}

func (sink *requestEnvelopeSummarySink) CompactionSourceSnapshot() (CompactionSourceSnapshot, error) {
	return CompactionSourceSnapshot{
		Revision:       "request-envelope-revision",
		BranchID:       "main",
		Messages:       []AgentMessage{{Role: string(llm.RoleUser), Content: strings.Repeat("a", 152)}},
		SourceEntryIDs: []string{"request-envelope-prior-entry"},
	}, nil
}

func (sink *requestEnvelopeSummarySink) CommitCompaction(ctx context.Context, projection CompactionProjection) error {
	if err := sink.consumerTestSink.CommitCompaction(ctx, projection); err != nil {
		return err
	}
	sink.committed.Store(true)
	return nil
}

func TestDurableRequestEnvelopeCompactsWithSummaryBeforeFirstDispatch(t *testing.T) {
	sink := &requestEnvelopeSummarySink{consumerTestSink: &consumerTestSink{}}
	var requests atomic.Int32
	requestMessages := make(chan []string, 2)
	var projectionWasCommittedBeforeDispatch atomic.Bool
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
		requests.Add(1)
		projectionWasCommittedBeforeDispatch.Store(sink.committed.Load())
		contents := make([]string, 0, len(request.Messages))
		for _, message := range request.Messages {
			contents = append(contents, message.Content)
		}
		requestMessages <- contents
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"summary accepted\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	registry := llm.NewProviderRegistry()
	require.NoError(t, registry.Register(llm.NewEinoProvider(
		llm.WithProviderName("core-request-envelope-summary-test"),
		llm.WithModels([]llm.ModelInfo{{Name: "core-request-envelope-summary-model", InputTokenLimit: 60}}),
		llm.WithHTTPClient(server.Client()),
	)))
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID: "core-request-envelope-summary-test",
		ModelID:    "core-request-envelope-summary-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	model = attemptGuardedModel{BaseChatModel: model}
	defer cleanup()
	var compactions atomic.Int32
	loop := NewLoopAgent(WithLLM(model), WithSystemPrompt("s"), WithMaxIterations(1))
	loop.SetMidTurnCompaction(nil, overflowSummaryCompactor{
		summary: "p",
		onCompact: func(context.Context, []compaction.TurnItem) {
			compactions.Add(1)
		},
	}, compaction.NewHeuristicTokenEstimator(), 70)
	agent := NewAgentImpl("request-envelope-durable-summary", loop)
	source, err := sink.CompactionSourceSnapshot()
	require.NoError(t, err)
	require.NoError(t, agent.SetHistoryFromSourceSnapshot(source))
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "request-envelope-summary-run"}, nil)
	owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{
		SessionID: "request-envelope-session", WorkspaceID: "request-envelope-workspace",
	})

	result, runErr := owner.run(time.Second, func(ctx context.Context) (Result, error) {
		return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "now"})
	})
	require.NoError(t, runErr, "valid durable summary compaction should let the same admitted request proceed")
	require.Equal(t, "summary accepted", result.Message)
	require.EqualValues(t, 1, compactions.Load(), "the request's full provider-visible message term must trigger compaction")
	require.EqualValues(t, 1, requests.Load())
	require.True(t, projectionWasCommittedBeforeDispatch.Load(), "the typed source projection must be committed before provider dispatch")
	dispatchedMessages := <-requestMessages
	require.Equal(t, "s", dispatchedMessages[0])
	require.Contains(t, dispatchedMessages[1], "p", "the prepared request must include the compacted summary")
	require.Equal(t, "now", dispatchedMessages[2], "the exact current submission must be retained")
	require.NotContains(t, strings.Join(dispatchedMessages, "\n"), strings.Repeat("a", 152), "the long published context must be summarized")
	steps, _, _, _ := sink.snapshot()
	require.Equal(t, []string{"input", "compaction", "terminal"}, steps)
}
