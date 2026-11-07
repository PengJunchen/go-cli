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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type editRecoveryContractModel struct {
	llm.BaseChatModel
	source llm.BaseChatModel
}

func (m editRecoveryContractModel) ProviderCandidates() []llm.ResolvedCandidateHandle {
	return llm.ProviderCandidatesFromModel(m.source)
}

func (m editRecoveryContractModel) CandidateSelectionSource() llm.CandidateSelectionSource {
	source, _ := llm.CandidateSelectionSourceFromModel(m.source)
	return source
}

func (editRecoveryContractModel) MaxCandidateBytes() int { return llm.GuardedStreamMaxBytes }

func (editRecoveryContractModel) CheckCandidate(context.Context, string) (*CandidateOutputResult, error) {
	return &CandidateOutputResult{Allowed: true}, nil
}

type editRecoveryContractRequest struct {
	Messages []map[string]json.RawMessage `json:"messages"`
}

func TestEditLoopRecoveryUsesJoinedSamePathRead(t *testing.T) {
	for _, tc := range []struct {
		name               string
		changeBeforeRead   bool
		finalInsteadRead   bool
		parallel           bool
		companionRead      bool
		recordReceiptErr   bool
		resolveErr         bool
		failRecoveryRead   bool
		wrongReadPath      bool
		cancelBeforeWindow bool
		cancelAfterRead    bool
		wantRunError       bool
		wantCanceled       bool
		wantMissingFile    bool
		wantErr            error
		wantRequests       int
		wantExecutions     int
		wantReceipts       []string
		wantFile           string
	}{
		{name: "changed source permits exact match", changeBeforeRead: true, wantRequests: 6, wantExecutions: 4, wantReceipts: []string{"edit-1", "edit-2", "read-4", "edit-5"}, wantFile: "replacement\n"},
		{name: "unchanged source blocks same match", wantErr: ErrEditRecoveryStaleRetry, wantRequests: 5, wantExecutions: 3, wantReceipts: []string{"edit-1", "edit-2", "read-4", "edit-5"}, wantFile: "actual text\n"},
		{name: "parallel group blocks stale edit before dispatch", parallel: true, companionRead: true, wantErr: ErrEditRecoveryStaleRetry, wantRequests: 5, wantExecutions: 4, wantReceipts: []string{"edit-1", "edit-2", "read-4", "edit-5", "read-5"}, wantFile: "actual text\n"},
		{name: "receipt sync failure cannot authorize recovery", recordReceiptErr: true, wantRunError: true, wantRequests: 1, wantExecutions: 1, wantFile: "actual text\n"},
		{name: "resolution sync failure cannot authorize recovery", resolveErr: true, wantRunError: true, wantRequests: 1, wantExecutions: 1, wantReceipts: []string{"edit-1"}, wantFile: "actual text\n"},
		{name: "failed recovery read cannot authorize retry", failRecoveryRead: true, wantRunError: true, wantRequests: 4, wantExecutions: 3, wantReceipts: []string{"edit-1", "edit-2", "read-4"}, wantMissingFile: true},
		{name: "wrong path read cannot authorize retry", wrongReadPath: true, wantRunError: true, wantRequests: 4, wantExecutions: 3, wantReceipts: []string{"edit-1", "edit-2", "read-4"}, wantFile: "actual text\n"},
		{name: "cancellation before recovery window prevents another request", cancelBeforeWindow: true, wantRunError: true, wantCanceled: true, wantRequests: 2, wantExecutions: 2, wantReceipts: []string{"edit-1", "edit-2"}, wantFile: "actual text\n"},
		{name: "cancellation after read prevents retry", cancelAfterRead: true, wantRunError: true, wantCanceled: true, wantRequests: 4, wantExecutions: 3, wantReceipts: []string{"edit-1", "edit-2", "read-4"}, wantFile: "actual text\n"},
		{name: "recovery may finish without tools", finalInsteadRead: true, wantRequests: 4, wantExecutions: 2, wantReceipts: []string{"edit-1", "edit-2"}, wantFile: "actual text\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			path := filepath.Join(root, "example.txt")
			require.NoError(t, os.WriteFile(path, []byte("actual text\n"), 0o600))
			otherPath := filepath.Join(root, "other.txt")
			require.NoError(t, os.WriteFile(otherPath, []byte("other file\n"), 0o600))

			var mu sync.Mutex
			var requests []editRecoveryContractRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request editRecoveryContractRequest
				if decodeErr := json.NewDecoder(r.Body).Decode(&request); decodeErr != nil {
					t.Error(decodeErr)
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				mu.Lock()
				requests = append(requests, request)
				n := len(requests)
				mu.Unlock()

				w.Header().Set("Content-Type", "text/event-stream")
				if tc.finalInsteadRead && n == 4 {
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"I need more evidence.\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
					return
				}
				if n == 6 {
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
					return
				}

				name := "edit"
				args := map[string]any{"file_path": path, "old_string": "new marker", "new_string": "replacement"}
				id := fmt.Sprintf("edit-%d", n)
				switch n {
				case 4:
					name = "read"
					id = "read-4"
					args = map[string]any{"path": path}
					if tc.failRecoveryRead {
						if removeErr := os.Remove(path); removeErr != nil {
							t.Error(removeErr)
							http.Error(w, "could not invalidate fixture", http.StatusInternalServerError)
							return
						}
					}
					if tc.wrongReadPath {
						args["path"] = otherPath
					}
					if tc.changeBeforeRead {
						if writeErr := os.WriteFile(path, []byte("new marker\n"), 0o600); writeErr != nil {
							t.Error(writeErr)
							http.Error(w, "could not update fixture", http.StatusInternalServerError)
							return
						}
					}
				case 5:
					if tc.wantErr != nil {
						args["new_string"] = "another replacement"
					}
				}
				encoded, encodeErr := json.Marshal(args)
				if encodeErr != nil {
					t.Error(encodeErr)
					http.Error(w, "could not encode fixture", http.StatusInternalServerError)
					return
				}
				call := map[string]any{"index": 0, "id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(encoded)}}
				calls := []any{call}
				if tc.companionRead && n == 5 {
					readArgs, readErr := json.Marshal(map[string]any{"path": path})
					if readErr != nil {
						t.Error(readErr)
						http.Error(w, "could not encode fixture", http.StatusInternalServerError)
						return
					}
					calls = append(calls, map[string]any{"index": 1, "id": "read-5", "type": "function", "function": map[string]any{"name": "read", "arguments": string(readArgs)}})
				}
				chunk, encodeErr := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"role": "assistant", "tool_calls": calls}}}})
				if encodeErr != nil {
					t.Error(encodeErr)
					http.Error(w, "could not encode fixture", http.StatusInternalServerError)
					return
				}
				_, _ = fmt.Fprintf(w, "data: %s\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", chunk)
			}))
			defer server.Close()

			providerRegistry := llm.NewProviderRegistry()
			_, source, cleanup := r3RegisterCandidate(t, providerRegistry, "edit-recovery-contract", llm.ModelInfo{Name: "edit-recovery-contract-model", InputTokenLimit: 50000, MaxOutputTokens: 128}, server.URL, 128)
			defer cleanup()
			model := editRecoveryContractModel{BaseChatModel: llm.NewLoopDetectionModelMiddleware().WrapModel(source), source: source}
			parent := context.Background()
			var cancel context.CancelFunc
			if tc.cancelBeforeWindow || tc.cancelAfterRead {
				parent, cancel = context.WithCancel(context.Background())
				defer cancel()
			}
			base := tools.NewDeferredToolRegistryAdapter(tools.NewDefaultToolRegistry())
			require.NoError(t, tools.RegisterDefaults(context.Background(), base,
				tools.WithRegisteredBuiltinWhitelist([]string{"edit", "read"}),
				tools.WithRegisteredPathWhitelist([]string{root}),
				tools.WithRegisteredCanonicalWorkspaceRoot(root),
			))
			registry := tools.NewMiddlewareToolRegistry(base)
			require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, req tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
				return tools.PreparedPolicyDecision{Allowed: true, Generations: req.Generations}, nil
			}))
			queue := tools.NewDefaultFileMutationQueue().(*tools.DefaultFileMutationQueue) //nolint:errcheck
			defer func() { require.NoError(t, queue.Close()) }()
			require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{MutationQueue: queue}))

			maxIterations := 5
			if tc.finalInsteadRead {
				maxIterations = 4
			} else if tc.wantErr != nil {
				maxIterations = 5
			} else {
				maxIterations = 6
			}
			sink := &consumerTestSink{}
			if tc.recordReceiptErr {
				sink.recordReceiptErr = errors.New("synthetic receipt sync failure")
			}
			if tc.resolveErr {
				sink.resolveErr = errors.New("synthetic resolution sync failure")
			}
			mode := ExecutionModeSequential
			if tc.parallel {
				mode = ExecutionModeParallel
			}
			loop := NewLoopAgent(WithLLM(model), WithTools(registry), WithExecutionMode(mode), WithMaxIterations(maxIterations))
			loop.SetMidTurnCompaction(nil, nil, compaction.NewHeuristicTokenEstimator(), 50000)
			agent := NewAgentImpl("edit-recovery-contract", loop)
			owner := newRunOwner(parent, nil, RunClaim{ID: "edit-recovery-contract-run"}, nil)
			if tc.cancelBeforeWindow || tc.cancelAfterRead {
				sink.recordReceiptHook = func(receipt RunToolReceipt) {
					if (tc.cancelBeforeWindow && receipt.ToolCallID == "edit-2") ||
						(tc.cancelAfterRead && receipt.ToolCallID == "read-4") {
						cancel()
					}
				}
			}
			owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "edit-recovery-contract-session", WorkspaceID: "edit-recovery-contract-workspace", CanonicalWorkspaceRoot: root})
			result, runErr := owner.run(10*time.Second, func(ctx context.Context) (Result, error) {
				return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "edit the file"})
			})
			if tc.wantErr != nil {
				require.ErrorIs(t, runErr, tc.wantErr)
			} else if tc.wantRunError {
				require.Error(t, runErr)
			} else {
				require.NoError(t, runErr)
			}
			if tc.wantCanceled {
				require.ErrorIs(t, runErr, context.Canceled)
			}
			if tc.finalInsteadRead {
				require.Equal(t, "I need more evidence.", result.Message)
			} else if tc.wantErr == nil && !tc.wantRunError {
				require.Equal(t, "done", result.Message)
			}
			data, readErr := os.ReadFile(path)
			if tc.wantMissingFile {
				require.ErrorIs(t, readErr, os.ErrNotExist)
			} else {
				require.NoError(t, readErr)
				require.Equal(t, tc.wantFile, string(data))
			}
			_, _, receipts, terminal := sink.snapshot()
			require.Len(t, receipts, len(tc.wantReceipts))
			if tc.wantExecutions > 0 {
				require.Len(t, sink.executionSnapshot(), tc.wantExecutions)
			}
			for i, id := range tc.wantReceipts {
				require.Equal(t, id, receipts[i].ToolCallID)
			}
			require.NotNil(t, terminal)
			if errors.Is(tc.wantErr, ErrEditRecoveryStaleRetry) {
				var blockedReceipt *RunToolReceipt
				for index := range receipts {
					if receipts[index].ToolCallID == "edit-5" {
						blockedReceipt = &receipts[index]
						break
					}
				}
				require.NotNil(t, blockedReceipt)
				require.Equal(t, ReceiptOutcomeError, blockedReceipt.ReceiptOutcome)
				require.Equal(t, DispatchStateNotDispatched, blockedReceipt.DispatchState)
				require.Empty(t, blockedReceipt.ActionID)
				executions := sink.executionSnapshot()
				for _, execution := range executions {
					require.NotEqual(t, "edit-5", execution.ToolCallID)
				}
				require.Equal(t, RunOutcomeError, terminal.Outcome)
			} else if !tc.wantRunError {
				require.Equal(t, RunOutcomeSuccess, terminal.Outcome)
			}
			mu.Lock()
			require.Len(t, requests, tc.wantRequests)
			mu.Unlock()
		})
	}
}
