package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type editRecoveryTestModel = editRecoveryContractModel
type editRecoveryWireRequest = editRecoveryContractRequest

type editRecoveryCleanupFailureModel struct{ editRecoveryTestModel }

func (m editRecoveryCleanupFailureModel) RunProviderOperation(ctx context.Context, run func(context.Context, llm.BaseChatModel) error) error {
	err := run(ctx, m.BaseChatModel)
	if err != nil {
		return errors.Join(err, errors.New("synthetic provider operation cleanup failure"))
	}
	return nil
}

type editRecoveryCancelRunner struct {
	editRecoveryTestModel
	cancel context.CancelFunc
}

func (m editRecoveryCancelRunner) RunProviderOperation(ctx context.Context, run func(context.Context, llm.BaseChatModel) error) error {
	err := run(ctx, m.BaseChatModel)
	var repeated *llm.RepeatedResponseError
	if errors.As(err, &repeated) {
		m.cancel()
	}
	return err
}

type editRecoveryVisibleModel struct {
	llm.BaseChatModel
	source                 llm.BaseChatModel
	calls                  atomic.Int32
	sameErrorChunk         bool
	errorChunkTextInjected atomic.Bool
}

func (m *editRecoveryVisibleModel) ProviderCandidates() []llm.ResolvedCandidateHandle {
	return llm.ProviderCandidatesFromModel(m.source)
}

func (m *editRecoveryVisibleModel) CandidateSelectionSource() llm.CandidateSelectionSource {
	source, _ := llm.CandidateSelectionSourceFromModel(m.source)
	return source
}

func (m *editRecoveryVisibleModel) Stream(ctx context.Context, messages []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	upstream, err := m.BaseChatModel.Stream(ctx, messages, opts...)
	if err != nil || m.calls.Add(1) != 3 {
		return upstream, err
	}
	out := make(chan llm.MessageChunk, 2)
	go func() {
		defer close(out)
		if m.sameErrorChunk {
			for chunk := range upstream {
				if chunk.Error != nil {
					var repeated *llm.RepeatedResponseError
					if errors.As(chunk.Error, &repeated) {
						chunk.Content = "synthetic error-chunk partial"
						m.errorChunkTextInjected.Store(true)
					}
				}
				out <- chunk
			}
			return
		}
		out <- llm.MessageChunk{Content: "synthetic visible partial"}
		for chunk := range upstream {
			out <- chunk
		}
	}()
	return out, nil
}

func TestEditLoopRecoverySafetyStops(t *testing.T) {
	tests := []struct {
		name, scenario     string
		requests, receipts int
		iterations         int
	}{
		{"truncated response", "length", 3, 2, 6},
		{"iteration exhausted", "limit", 3, 2, 3},
		{"provider failure", "provider", 3, 2, 6},
		{"new steering", "steering", 3, 2, 6},
		{"operation cleanup failure", "cleanup", 3, 2, 6},
		{"already visible output", "visible", 3, 2, 6},
		{"buffered partial output", "buffered", 3, 2, 6},
		{"partial text shares repeated-error chunk", "same-error-chunk", 3, 2, 6},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := filepath.Join(root, "example.txt")
			require.NoError(t, os.WriteFile(path, []byte("actual text\n"), 0o600))
			var mu sync.Mutex
			var requests []editRecoveryWireRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request editRecoveryWireRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				mu.Lock()
				requests = append(requests, request)
				n := len(requests)
				mu.Unlock()
				if tc.scenario == "provider" && n == 3 {
					http.Error(w, "synthetic invalid request", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if n > 4 || (n == 4 && tc.scenario != "repeat") {
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"unexpected\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
					return
				}
				args := map[string]any{"file_path": path, "old_string": "missing text", "new_string": "changed"}
				if tc.scenario == "success" {
					args["old_string"], args["new_string"] = "actual text", "actual text"
				}
				if tc.scenario == "unrelated" {
					args["file_path"] = filepath.Join(root, "missing.txt")
				}
				encoded, _ := json.Marshal(args)
				calls := []any{map[string]any{"index": 0, "id": fmt.Sprintf("call-%d", n), "type": "function", "function": map[string]any{"name": "edit", "arguments": string(encoded)}}}
				if tc.scenario == "mixed" {
					readArgs, _ := json.Marshal(map[string]any{"path": path})
					calls = append(calls, map[string]any{"index": 1, "id": fmt.Sprintf("read-%d", n), "type": "function", "function": map[string]any{"name": "read", "arguments": string(readArgs)}})
				}
				chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"role": "assistant", "tool_calls": calls}}}})
				finish := "tool_calls"
				if tc.scenario == "length" && n == 3 {
					finish = "length"
				}
				_, _ = fmt.Fprintf(w, "data: %s\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":%q}]}\n\ndata: [DONE]\n\n", chunk, finish)
			}))
			defer server.Close()
			providerRegistry := llm.NewProviderRegistry()
			_, source, cleanup := r3RegisterCandidate(t, providerRegistry, "edit-recovery-stop", llm.ModelInfo{Name: "edit-recovery-stop-model", InputTokenLimit: 50000, MaxOutputTokens: 128}, server.URL, 128)
			defer cleanup()
			wrapped := editRecoveryTestModel{BaseChatModel: llm.NewLoopDetectionModelMiddleware().WrapModel(source), source: source}
			var model llm.BaseChatModel = wrapped
			var sameErrorChunkModel *editRecoveryVisibleModel
			if tc.scenario == "cleanup" {
				model = editRecoveryCleanupFailureModel{wrapped}
			}
			if tc.scenario == "visible" {
				model = &editRecoveryVisibleModel{BaseChatModel: wrapped.BaseChatModel, source: source}
			}
			if tc.scenario == "buffered" {
				model = attemptGuardedModel{BaseChatModel: &editRecoveryVisibleModel{BaseChatModel: wrapped.BaseChatModel, source: source}}
			}
			if tc.scenario == "same-error-chunk" {
				sameErrorChunkModel = &editRecoveryVisibleModel{BaseChatModel: wrapped.BaseChatModel, source: source, sameErrorChunk: true}
				model = sameErrorChunkModel
			}
			if tc.scenario == "cancel-after" {
				model = editRecoveryCancelRunner{editRecoveryTestModel: wrapped, cancel: cancel}
			}
			base := tools.NewDeferredToolRegistryAdapter(tools.NewDefaultToolRegistry())
			require.NoError(t, tools.RegisterDefaults(context.Background(), base, tools.WithRegisteredBuiltinWhitelist([]string{"edit", "read"}), tools.WithRegisteredPathWhitelist([]string{root}), tools.WithRegisteredCanonicalWorkspaceRoot(root)))
			registry := tools.NewMiddlewareToolRegistry(base)
			require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, req tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
				return tools.PreparedPolicyDecision{Allowed: true, Generations: req.Generations}, nil
			}))
			queue := tools.NewDefaultFileMutationQueue().(*tools.DefaultFileMutationQueue) //nolint:errcheck
			defer func() { require.NoError(t, queue.Close()) }()
			require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{MutationQueue: queue}))
			sink := &consumerTestSink{}
			steering := make(chan string, 1)
			var target RunCommitSink = sink
			if tc.scenario == "receipt" {
				sink.recordReceiptErr = errors.New("synthetic receipt failure")
			}
			if tc.scenario == "resolution" {
				target = &stagedTestSink{consumerTestSink: sink, mode: "resolution"}
			}
			if tc.scenario == "cancel" {
				sink.recordReceiptHook = func(receipt RunToolReceipt) {
					if receipt.ToolCallID == "call-2" {
						cancel()
					}
				}
			}
			if tc.scenario == "steering" {
				sink.recordReceiptHook = func(receipt RunToolReceipt) {
					if receipt.ToolCallID == "call-2" {
						steering <- "new synthetic instruction"
					}
				}
			}
			options := []LoopOption{WithLLM(model), WithTools(registry), WithMaxIterations(tc.iterations)}
			if tc.scenario == "steering" {
				options = append(options, WithSteeringChannel(steering))
			}
			attempts := &attemptResultRecorder{}
			options = append(options, WithProviderAttemptRecorder(attempts.record))
			loop := NewLoopAgent(options...)
			loop.SetMidTurnCompaction(nil, nil, compaction.NewHeuristicTokenEstimator(), 50000)
			agent := NewAgentImpl("edit-recovery-stop", loop)
			owner := newRunOwner(parent, nil, RunClaim{ID: "edit-recovery-stop-run"}, nil)
			owner.ctx = WithRunCommit(owner.ctx, target, RunCommitBinding{SessionID: "edit-recovery-stop-session", WorkspaceID: "edit-recovery-stop-workspace", CanonicalWorkspaceRoot: root})
			eventStream := NewEventStream(128, WithEventDiscardPolicy(DiscardNewest))
			_, runErr := owner.run(10*time.Second, func(ctx context.Context) (Result, error) {
				return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "edit synthetic file"}, eventStream)
			})
			eventStream.Close()
			visible := false
			sameChunkTextPublished := false
			for event := range eventStream.Events() {
				if event.Incremental && event.Content == "synthetic visible partial" {
					visible = true
				}
				if event.Incremental && event.Content == "synthetic error-chunk partial" {
					sameChunkTextPublished = true
				}
			}
			if tc.scenario == "visible" {
				require.True(t, visible, "the partial content actually crossed the event boundary")
			}
			if tc.scenario == "buffered" {
				require.False(t, visible, "guarded partial content must remain unpublished")
			}
			if tc.scenario == "same-error-chunk" {
				require.True(t, sameErrorChunkModel.errorChunkTextInjected.Load(), "the third stream must carry partial text on the typed repeated-response error chunk")
				require.False(t, sameChunkTextPublished, "the error chunk's partial content is not emitted before the stream error")
			}
			require.Error(t, runErr)
			if tc.scenario == "cancel" || tc.scenario == "cancel-after" {
				require.ErrorIs(t, runErr, context.Canceled)
			}
			_, _, receipts, _ := sink.snapshot()
			require.Len(t, receipts, tc.receipts)
			mu.Lock()
			defer mu.Unlock()
			require.Len(t, requests, tc.requests)
			require.Len(t, attempts.snapshot(), tc.requests)
			controls := 0
			for _, request := range requests {
				for _, message := range request.Messages {
					var content string
					_ = json.Unmarshal(message["content"], &content)
					if strings.Contains(content, "A repeated failed exact edit was blocked") {
						controls++
					}
				}
			}
			require.Zero(t, controls, "stop conditions must not open the recovery request window")
		})
	}
}
