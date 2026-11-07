package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type stagedTestDispatcher struct {
	calls atomic.Int32
	fail  bool
}

func (d *stagedTestDispatcher) Dispatch(context.Context, tools.SubagentTask) (tools.SubagentResult, error) {
	d.calls.Add(1)
	if d.fail {
		return tools.SubagentResult{}, errors.New("known child error")
	}
	return tools.SubagentResult{PhysicalSessionID: "child-physical", Content: "child done"}, nil
}
func (*stagedTestDispatcher) ParallelDispatch(context.Context, []tools.SubagentTask) ([]tools.SubagentResult, error) {
	return nil, errors.New("unexpected parallel subagent dispatch")
}
func (*stagedTestDispatcher) ListRunning() []tools.SubagentTask { return nil }

var errStagedTestCommit = errors.New("injected staged commit failure")

type stagedTestSink struct {
	*consumerTestSink
	mode   string
	cancel context.CancelFunc
	fence  *tools.DispatchFence
}

func (s *stagedTestSink) RecordReceipt(ctx context.Context, receipt RunToolReceipt) error {
	if s.mode == "receipt" && receipt.ToolCallID == "read-one" {
		return errStagedTestCommit
	}
	return s.consumerTestSink.RecordReceipt(ctx, receipt)
}
func (s *stagedTestSink) ResolveExecution(ctx context.Context, resolution ExecutionResolution) error {
	if s.mode == "resolution" {
		return errStagedTestCommit
	}
	if err := s.consumerTestSink.ResolveExecution(ctx, resolution); err != nil {
		return err
	}
	if s.mode == "cancel" {
		s.cancel()
	}
	if s.mode == "fence" {
		if _, err := s.fence.Publish(tools.DispatchFencePlan, nil, func() {}); err != nil {
			return err
		}
	}
	return nil
}

func stagedTestRegistry(t *testing.T, root string, owned bool, includeWrite bool, dispatcher *stagedTestDispatcher, prepared map[string]int, mu *sync.Mutex) *tools.MiddlewareToolRegistry {
	t.Helper()
	ctx := context.Background()
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	names := []string{"read"}
	if includeWrite {
		names = append(names, "write")
	}
	require.NoError(t, tools.RegisterDefaults(ctx, base,
		tools.WithRegisteredBuiltinWhitelist(names),
		tools.WithRegisteredPathWhitelist([]string{root}),
		tools.WithRegisteredCanonicalWorkspaceRoot(root),
	))
	registry := tools.NewMiddlewareToolRegistry(base)
	var delegate tools.ToolDefinition = tools.NewSubagentTool(dispatcher)
	if owned {
		delegate = tools.NewOwnedSubagentTool(dispatcher)
	}
	require.NoError(t, registry.RegisterTrustedInlineControl(ctx, delegate))
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, req tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		mu.Lock()
		prepared[req.Call.ToolCall().ID]++
		mu.Unlock()
		return tools.PreparedPolicyDecision{Allowed: true, Generations: req.Generations}, nil
	}))
	stages := tools.PreparedToolStages{}
	if includeWrite {
		queue := tools.NewDefaultFileMutationQueue().(*tools.DefaultFileMutationQueue) //nolint:errcheck
		t.Cleanup(func() { require.NoError(t, queue.Close()) })
		stages.MutationQueue = queue
	}
	require.NoError(t, registry.SetPreparedToolStages(stages))
	return registry
}

func TestMixedReadDelegationPreparesOnceAndFallsBackForUntrustedOrWrite(t *testing.T) {
	for _, tc := range []struct {
		name         string
		owned        bool
		includeWrite bool
		missingRead  bool
		childError   bool
		wantBatches  int
	}{{"scoped-read-and-owned", true, false, false, false, 3}, {"known-read-error-continues", true, false, true, false, 3}, {"known-child-error-continues", true, false, false, true, 3}, {"same-name-unowned", false, false, false, false, 1}, {"mutation-mixed", true, true, false, false, 3}} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			first, second := filepath.Join(root, "first.txt"), filepath.Join(root, "second.txt")
			if !tc.missingRead {
				require.NoError(t, os.WriteFile(first, []byte("first"), 0o600))
			}
			require.NoError(t, os.WriteFile(second, []byte("second"), 0o600))
			calls := []llm.ToolCall{
				{ID: "read-one", Name: "read", Args: map[string]any{"path": first}},
				{ID: "delegate", Name: "dispatch_subagent", Args: map[string]any{"prompt": "research"}},
				{ID: "read-two", Name: "read", Args: map[string]any{"path": second}},
			}
			if tc.includeWrite {
				calls = append(calls, llm.ToolCall{ID: "write", Name: "write", Args: map[string]any{"path": filepath.Join(root, "new.txt"), "content": "done"}})
			}
			dispatcher := &stagedTestDispatcher{fail: tc.childError}
			prepared := map[string]int{}
			var mu sync.Mutex
			registry := stagedTestRegistry(t, root, tc.owned, tc.includeWrite, dispatcher, prepared, &mu)
			sink := &consumerTestSink{}
			model := &scriptedChatModel{seq: []*llm.Message{{Role: llm.RoleAssistant, ToolCalls: calls}, {Role: llm.RoleAssistant, Content: "done"}}}
			agent := NewAgentImpl("staged-mixed", NewLoopAgent(WithLLM(model), WithTools(registry), WithExecutionMode(ExecutionModeParallel)))
			owner := newRunOwner(context.Background(), nil, RunClaim{ID: "staged-run"}, nil)
			owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "parent-session", WorkspaceID: "workspace", CanonicalWorkspaceRoot: root})
			_, err = owner.run(5*time.Second, func(ctx context.Context) (Result, error) {
				return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "perform actions"})
			})
			require.NoError(t, err)
			steps, batches, receipts, _ := sink.snapshot()
			require.Len(t, batches, tc.wantBatches)
			require.Len(t, receipts, len(calls))
			for i, call := range calls {
				require.Equal(t, 1, prepared[call.ID], "each original invocation is prepared once")
				require.Equal(t, call.ID, receipts[i].ToolCallID)
			}
			require.Equal(t, int32(1), dispatcher.calls.Load())
			if tc.missingRead {
				require.Equal(t, ReceiptOutcomeError, receipts[0].ReceiptOutcome, "known read errors remain visible and do not prevent later safe segments")
			}
			if tc.childError {
				require.Equal(t, ReceiptOutcomeError, receipts[1].ReceiptOutcome, "joined child errors remain visible")
				require.Equal(t, ReceiptOutcomeSuccess, receipts[2].ReceiptOutcome, "known child errors do not prevent later safe segments")
			}
			if tc.wantBatches == 3 {
				require.NotNil(t, batches[0].Intents[0].ScopedReadProof)
				require.NotNil(t, batches[1].Intents[0].ManagedDelegationProof)
				require.NotNil(t, batches[2].Intents[0].ScopedReadProof)
				if tc.includeWrite {
					require.Len(t, batches[2].Intents, 2, "adjacent ordinary calls remain one phase")
					require.Equal(t, "write", batches[2].Intents[1].ToolCallID)
					require.Nil(t, batches[2].Intents[1].ScopedReadProof, "a write is never promoted to a read")
					var starts []int
					for i, step := range steps {
						if step == "begin" {
							starts = append(starts, i)
						}
					}
					require.Len(t, starts, 3)
					require.Contains(t, steps[starts[0]+1:starts[1]], "resolution", "the first phase must resolve before the delegate is reserved")
					require.Contains(t, steps[starts[1]+1:starts[2]], "resolution", "the delegate must resolve before the last phase is reserved")
				}
			}
		})
	}
}

func TestMixedReadDelegationStopsAfterCancellationOrCommitFailure(t *testing.T) {
	for _, mode := range []string{"cancel", "receipt", "resolution", "fence"} {
		t.Run(mode, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			path := filepath.Join(root, "first.txt")
			require.NoError(t, os.WriteFile(path, []byte("first"), 0o600))
			parentCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sink := &stagedTestSink{consumerTestSink: &consumerTestSink{}, mode: mode, cancel: cancel}
			dispatcher := &stagedTestDispatcher{}
			prepared := map[string]int{}
			var mu sync.Mutex
			registry := stagedTestRegistry(t, root, true, false, dispatcher, prepared, &mu)
			sink.fence = registry.DispatchFence()
			model := &scriptedChatModel{seq: []*llm.Message{{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "read-one", Name: "read", Args: map[string]any{"path": path}},
				{ID: "delegate", Name: "dispatch_subagent", Args: map[string]any{"prompt": "must not run"}},
			}}, {Role: llm.RoleAssistant, Content: "must not be requested"}}}
			agent := NewAgentImpl("staged-failure", NewLoopAgent(WithLLM(model), WithTools(registry), WithExecutionMode(ExecutionModeParallel)))
			owner := newRunOwner(parentCtx, nil, RunClaim{ID: "staged-failure-run"}, nil)
			owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "parent-session", WorkspaceID: "workspace", CanonicalWorkspaceRoot: root})
			_, err = owner.run(5*time.Second, func(ctx context.Context) (Result, error) {
				return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "do not execute after failure"})
			})
			require.Error(t, err)
			if mode == "cancel" {
				require.ErrorIs(t, err, context.Canceled)
			} else if mode == "fence" {
				require.ErrorIs(t, err, tools.ErrDispatchFenceChanged)
			} else {
				require.ErrorIs(t, err, errStagedTestCommit)
			}
			require.Zero(t, dispatcher.calls.Load(), "unreserved later delegate cannot execute")
			_, batches, receipts, _ := sink.snapshot()
			if mode != "fence" {
				for _, batch := range batches {
					for _, intent := range batch.Intents {
						require.NotEqual(t, "delegate", intent.ToolCallID, "later delegate was not reserved")
					}
				}
			}
			if mode == "cancel" {
				require.Len(t, receipts, 2)
				require.Equal(t, "delegate", receipts[1].ToolCallID)
				require.Equal(t, DispatchStateNotDispatched, receipts[1].DispatchState)
				require.Equal(t, ReceiptOutcomeInterrupted, receipts[1].ReceiptOutcome)
			}
			if mode == "receipt" || mode == "resolution" {
				require.Empty(t, sink.resolutionSnapshot(), "uncertain persistence must not fabricate resolution")
			}
			require.Equal(t, 1, model.idx)
		})
	}
}
