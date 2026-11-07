package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/session"
)

var errBatchedInputCommit = errors.New("synthetic input commit failure")

type batchedInputFailWriter struct {
	session.DurableSessionWriter
	failContent string
	failures    atomic.Int32
}

func (writer *batchedInputFailWriter) AppendAndSync(ctx context.Context, rows ...session.SessionEntry) ([]session.SessionEntry, error) {
	for _, row := range rows {
		if row.Type == session.EntryTypeUser && row.Content == writer.failContent {
			writer.failures.Add(1)
			return nil, errBatchedInputCommit
		}
	}
	return writer.DurableSessionWriter.AppendAndSync(ctx, rows...)
}

type batchedInputObservation struct {
	messages []llm.Message
	snapshot session.PreparedSessionSnapshot
}

type batchedInputWireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func TestDurableBatchedInputsReachPreparedResolver(t *testing.T) {
	for _, tc := range []struct {
		name      string
		steering  []string
		followUps []string
	}{
		{name: "two steering", steering: []string{"steer-a", "steer-b"}},
		{name: "two follow-ups", followUps: []string{"follow-a", "follow-b"}},
		{name: "mixed", steering: []string{"steer-a", "steer-b"}, followUps: []string{"follow-a", "follow-b"}},
		{name: "identical inputs retain distinct identities", steering: []string{"same", "same"}, followUps: []string{"same", "same"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			t.Setenv("HOME", root)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			workspace, err := session.WorkspaceIDForCanonicalRoot(root)
			require.NoError(t, err)
			index, err := session.NewWorkspaceQuarantineIndex()
			require.NoError(t, err)
			require.NoError(t, index.EnsureWorkspace(ctx, workspace))
			store := filepath.Join(root, "durable")
			prepared, err := session.NewDurableJSONLSessionPreparer(store, index).PrepareNew(ctx, "batched inputs")
			require.NoError(t, err)
			open := true
			t.Cleanup(func() {
				if open {
					_ = prepared.Close()
				}
			})
			sink, err := session.NewSessionRunCommitSink(prepared, index, workspace, nil)
			require.NoError(t, err)

			var requestMu sync.Mutex
			var requests [][]batchedInputWireMessage
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Messages []batchedInputWireMessage `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode synthetic request: %v", err)
					return
				}
				requestMu.Lock()
				requests = append(requests, request.Messages)
				requestMu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"synthetic final\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			registry := llm.NewProviderRegistry()
			require.NoError(t, registry.Register(llm.NewEinoProvider(llm.WithProviderName("batched-inputs"), llm.WithModels([]llm.ModelInfo{{Name: "batched-inputs-model", InputTokenLimit: 100000}}), llm.WithHTTPClient(server.Client()))))
			resolved, err := registry.SnapshotResolver().ResolveModel(ctx, llm.ModelRequest{ProviderID: "batched-inputs", ModelID: "batched-inputs-model", Endpoint: server.URL})
			require.NoError(t, err)
			base, cleanup, err := resolved.Build(ctx)
			require.NoError(t, err)
			defer cleanup()
			var observedMu sync.Mutex
			var observed []batchedInputObservation
			model := &durableResolverCheckingModel{&snapshotCheckingChatModel{base: base, check: func(messages []llm.Message) error {
				snapshot, err := prepared.Snapshot()
				if err != nil {
					return err
				}
				observedMu.Lock()
				observed = append(observed, batchedInputObservation{messages: append([]llm.Message(nil), messages...), snapshot: snapshot})
				observedMu.Unlock()
				return nil
			}}}
			steering := make(chan string, len(tc.steering))
			followUps := make(chan string, len(tc.followUps))
			for _, value := range tc.steering {
				steering <- value
			}
			for _, value := range tc.followUps {
				followUps <- value
			}
			loop := core.NewLoopAgent(core.WithLLM(model), core.WithSystemPrompt("s"), core.WithMaxIterations(1), core.WithSteeringChannel(steering), core.WithFollowUpChannel(followUps))
			loop.SetMidTurnCompaction(nil, nil, compaction.NewHeuristicTokenEstimator(), 50000)
			harness := core.NewHarnessImpl(core.NewAgentImpl("batched-inputs", loop))
			runCtx := core.WithRunCommit(ctx, sink, core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspace, CanonicalWorkspaceRoot: root})
			stream, err := harness.Submit(runCtx, "initial")
			require.NoError(t, err)
			for range stream.Events() {
			}
			_, err = stream.Result()
			require.NoError(t, err)
			observedMu.Lock()
			observations := append([]batchedInputObservation(nil), observed...)
			observedMu.Unlock()
			requestMu.Lock()
			wireRequests := append([][]batchedInputWireMessage(nil), requests...)
			requestMu.Unlock()
			require.Len(t, observations, 1)
			require.Len(t, wireRequests, 1, "one existing provider request, without replay")
			observation := observations[0]
			wire := wireRequests[0]
			want := append([]string{"initial"}, tc.steering...)
			want = append(want, tc.followUps...)
			var userMessages []llm.Message
			for _, message := range observation.messages {
				if message.Role == llm.RoleUser {
					userMessages = append(userMessages, message)
				}
			}
			require.Len(t, userMessages, len(want))
			runID := userMessages[0].Runtime.Source.RunID
			require.NotEmpty(t, runID)
			for index, message := range userMessages {
				require.Equal(t, want[index], message.Content)
				require.Equal(t, llm.SourceRunTranscript, message.Runtime.Source.Kind)
				require.Equal(t, runID, message.Runtime.Source.RunID)
				require.EqualValues(t, index, message.Runtime.Source.TranscriptIndex)
				require.Zero(t, message.Runtime.Source.PriorSourceCount)
			}
			var wireUsers []string
			for _, message := range wire {
				if message.Role == "user" {
					wireUsers = append(wireUsers, message.Content)
				}
			}
			require.Equal(t, want, wireUsers)
			beforeIDs := assertBatchedInputRows(t, observation.snapshot, runID, want, tc.steering, tc.followUps)
			live, err := prepared.Snapshot()
			require.NoError(t, err)
			require.NoError(t, prepared.Close())
			open = false
			reopened, err := session.NewDurableJSONLSessionPreparer(store, index).PrepareResume(ctx, live.SessionID)
			require.NoError(t, err)
			defer reopened.Close()
			restored, err := reopened.Snapshot()
			require.NoError(t, err)
			require.Equal(t, beforeIDs, assertBatchedInputRows(t, restored, runID, want, tc.steering, tc.followUps))
			var publishedUsers []string
			for _, message := range restored.PublishedView.Messages {
				if message.Role == "user" {
					publishedUsers = append(publishedUsers, message.Content)
				}
			}
			require.Equal(t, want, publishedUsers, "reopening must not duplicate or drop published inputs")
		})
	}
}

func assertBatchedInputRows(t *testing.T, snapshot session.PreparedSessionSnapshot, runID string, want, steering, followUps []string) []string {
	t.Helper()
	var inputs []session.SessionEntry
	for _, row := range snapshot.DurableEntries {
		if row.Type == session.EntryTypeUser && row.RunID == runID {
			inputs = append(inputs, row)
		}
	}
	require.Len(t, inputs, len(want))
	identities := make(map[string]bool)
	var orderedIDs []string
	for index, row := range inputs {
		require.Equal(t, want[index], row.Content)
		require.NotEmpty(t, row.ID)
		require.False(t, identities[row.ID], "each accepted input has one distinct durable identity")
		identities[row.ID] = true
		orderedIDs = append(orderedIDs, row.ID)
		require.NotNil(t, row.TranscriptIndex)
		require.EqualValues(t, index, *row.TranscriptIndex)
		kind := core.InputKindUser
		if index > 0 && index <= len(steering) {
			kind = core.InputKindSteering
		} else if index > len(steering) {
			kind = core.InputKindFollowUp
		}
		require.Equal(t, kind, row.InputKind)
	}
	require.Equal(t, len(want), 1+len(steering)+len(followUps))
	return orderedIDs
}

func TestDurableBatchedInputCommitFailureDoesNotDispatch(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	workspace, err := session.WorkspaceIDForCanonicalRoot(root)
	require.NoError(t, err)
	index, err := session.NewWorkspaceQuarantineIndex()
	require.NoError(t, err)
	require.NoError(t, index.EnsureWorkspace(ctx, workspace))
	prepared, err := session.NewDurableJSONLSessionPreparer(filepath.Join(root, "durable"), index).PrepareNew(ctx, "batched commit failure")
	require.NoError(t, err)
	defer prepared.Close()
	writer := &batchedInputFailWriter{DurableSessionWriter: prepared.DurableWriter(), failContent: "follow-b"}
	sinkSession := &durableWriterOverrideSession{PreparedSession: prepared, writer: writer}
	sink, err := session.NewSessionRunCommitSink(sinkSession, index, workspace, nil)
	require.NoError(t, err)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	registry := llm.NewProviderRegistry()
	require.NoError(t, registry.Register(llm.NewEinoProvider(llm.WithProviderName("batched-input-failure"), llm.WithModels([]llm.ModelInfo{{Name: "batched-input-failure-model", InputTokenLimit: 100000}}), llm.WithHTTPClient(server.Client()))))
	resolved, err := registry.SnapshotResolver().ResolveModel(ctx, llm.ModelRequest{ProviderID: "batched-input-failure", ModelID: "batched-input-failure-model", Endpoint: server.URL})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(ctx)
	require.NoError(t, err)
	defer cleanup()
	steering := make(chan string, 2)
	followUps := make(chan string, 2)
	steering <- "steer-a"
	steering <- "steer-b"
	followUps <- "follow-a"
	followUps <- "follow-b"
	loop := core.NewLoopAgent(core.WithLLM(model), core.WithSystemPrompt("s"), core.WithMaxIterations(1), core.WithSteeringChannel(steering), core.WithFollowUpChannel(followUps))
	loop.SetMidTurnCompaction(nil, nil, compaction.NewHeuristicTokenEstimator(), 50000)
	runCtx := core.WithRunCommit(ctx, sink, core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspace, CanonicalWorkspaceRoot: root})
	stream, err := core.NewHarnessImpl(core.NewAgentImpl("batched-input-failure", loop)).Submit(runCtx, "initial")
	require.NoError(t, err)
	for range stream.Events() {
	}
	_, err = stream.Result()
	require.ErrorIs(t, err, errBatchedInputCommit)
	require.EqualValues(t, 1, writer.failures.Load())
	require.Zero(t, requests.Load(), "a failed durable input must block provider dispatch")
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	var committed []string
	for _, row := range snapshot.DurableEntries {
		if row.Type == session.EntryTypeUser {
			committed = append(committed, row.Content)
		}
	}
	require.Equal(t, []string{"initial", "steer-a", "steer-b", "follow-a"}, committed)
}
