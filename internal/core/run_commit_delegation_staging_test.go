package core

import (
	"context"
	"errors"
	"fmt"
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

type stagedOrdinaryBarrier struct {
	started chan string
	release <-chan struct{}
}

func (*stagedOrdinaryBarrier) Name() string        { return "ordinary-barrier" }
func (*stagedOrdinaryBarrier) Description() string { return "test ordinary parallel dispatch" }
func (tool *stagedOrdinaryBarrier) Execute(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	tool.started <- call.ID
	select {
	case <-tool.release:
		return &tools.ToolResult{Output: "joined"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type stagedUnknownTool struct{ calls atomic.Int32 }

func (*stagedUnknownTool) Name() string        { return "unknown-stage" }
func (*stagedUnknownTool) Description() string { return "test unknown action result" }
func (tool *stagedUnknownTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	tool.calls.Add(1)
	return nil, toolOutcomeUnknownError{cause: errors.New("unknown test action")}
}

type stagedCountingTool struct{ calls atomic.Int32 }

func (*stagedCountingTool) Name() string        { return "ordinary-after" }
func (*stagedCountingTool) Description() string { return "test later ordinary action" }
func (tool *stagedCountingTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	tool.calls.Add(1)
	return &tools.ToolResult{Output: "done"}, nil
}

type stagedPrepareFaultRegistry struct {
	*tools.MiddlewareToolRegistry
	err error
}

type stagedMixedAuthorityRegistry struct {
	*tools.MiddlewareToolRegistry
	unowned *tools.MiddlewareToolRegistry
}

func (registry *stagedMixedAuthorityRegistry) PrepareToolCall(ctx context.Context, call tools.ToolCall) (tools.PreparedInvocation, error) {
	if call.ID == "unowned" {
		return registry.unowned.PrepareToolCall(ctx, call)
	}
	return registry.MiddlewareToolRegistry.PrepareToolCall(ctx, call)
}

func (registry *stagedPrepareFaultRegistry) PrepareToolCall(ctx context.Context, call tools.ToolCall) (tools.PreparedInvocation, error) {
	if call.ID == "fenced" {
		return nil, registry.err
	}
	return registry.MiddlewareToolRegistry.PrepareToolCall(ctx, call)
}

func TestDelegationStagesPreserveKnownPrepareReceipts(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	first, second := filepath.Join(root, "first.txt"), filepath.Join(root, "second.txt")
	require.NoError(t, os.WriteFile(first, []byte("first"), 0o600))
	require.NoError(t, os.WriteFile(second, []byte("second"), 0o600))
	calls := []llm.ToolCall{
		{ID: "known-before", Name: "missing-tool", Args: map[string]any{}},
		{ID: "read-before", Name: "read", Args: map[string]any{"path": first}},
		{ID: "delegate", Name: "dispatch_subagent", Args: map[string]any{"prompt": "research"}},
		{ID: "read-after", Name: "read", Args: map[string]any{"path": second}},
		{ID: "known-after", Name: "another-missing-tool", Args: map[string]any{}},
	}
	dispatcher := &stagedTestDispatcher{}
	prepared := map[string]int{}
	var mu sync.Mutex
	registry := stagedTestRegistry(t, root, true, false, dispatcher, prepared, &mu)
	sink := &consumerTestSink{}
	model := &scriptedChatModel{seq: []*llm.Message{{Role: llm.RoleAssistant, ToolCalls: calls}, {Role: llm.RoleAssistant, Content: "done"}}}
	agent := NewAgentImpl("staged-known", NewLoopAgent(WithLLM(model), WithTools(registry), WithExecutionMode(ExecutionModeParallel)))
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "staged-known-run"}, nil)
	owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "parent-session", WorkspaceID: "workspace", CanonicalWorkspaceRoot: root})
	_, err = owner.run(5*time.Second, func(ctx context.Context) (Result, error) {
		return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "perform actions"})
	})
	require.NoError(t, err)
	_, batches, receipts, _ := sink.snapshot()
	require.Len(t, batches, 3, "ordinary actions on either side of the delegation are separately reserved")
	require.Len(t, receipts, len(calls))
	for i, call := range calls {
		require.Equal(t, call.ID, receipts[i].ToolCallID, "canonical tool receipt order")
	}
	require.Equal(t, ReceiptOutcomeError, receipts[0].ReceiptOutcome)
	require.Equal(t, ReceiptOutcomeError, receipts[4].ReceiptOutcome)
	require.Equal(t, ReceiptOutcomeSuccess, receipts[2].ReceiptOutcome)
	require.Len(t, batches[0].KnownReceipts, 1)
	require.Equal(t, "known-before", batches[0].KnownReceipts[0].ToolCallID)
	require.Equal(t, receipts[0].TranscriptIndex, batches[0].KnownReceipts[0].TranscriptIndex)
	require.Len(t, batches[2].KnownReceipts, 1)
	require.Equal(t, "known-after", batches[2].KnownReceipts[0].ToolCallID)
	require.Equal(t, receipts[4].TranscriptIndex, batches[2].KnownReceipts[0].TranscriptIndex)
	require.Equal(t, int32(1), dispatcher.calls.Load())
}

func TestDelegationStagesContinueAfterKnownInterceptorDenial(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	path := filepath.Join(root, "input.txt")
	require.NoError(t, os.WriteFile(path, []byte("fixture"), 0o600))
	dispatcher := &stagedTestDispatcher{}
	prepared := map[string]int{}
	var mu sync.Mutex
	registry := stagedTestRegistry(t, root, true, false, dispatcher, prepared, &mu)
	sink := &consumerTestSink{}
	model := &scriptedChatModel{seq: []*llm.Message{{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
		{ID: "denied", Name: "read", Args: map[string]any{"path": path}},
		{ID: "delegate", Name: "dispatch_subagent", Args: map[string]any{"prompt": "research"}},
	}}, {Role: llm.RoleAssistant, Content: "done"}}}
	loop := NewLoopAgent(WithLLM(model), WithTools(registry), WithExecutionMode(ExecutionModeParallel),
		WithToolInterceptor(func(_ string, callID string, _ map[string]any) error {
			if callID == "denied" {
				return errors.New("known test denial")
			}
			return nil
		}))
	agent := NewAgentImpl("staged-interceptor", loop)
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "staged-interceptor-run"}, nil)
	owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "parent-session", WorkspaceID: "workspace", CanonicalWorkspaceRoot: root})
	_, err = owner.run(5*time.Second, func(ctx context.Context) (Result, error) {
		return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "perform actions"})
	})
	require.NoError(t, err)
	_, batches, receipts, _ := sink.snapshot()
	require.Len(t, batches, 2)
	require.Len(t, batches[0].KnownReceipts, 1)
	require.Empty(t, batches[0].Intents)
	require.Len(t, receipts, 2)
	require.Equal(t, "denied", receipts[0].ToolCallID)
	require.Equal(t, ReceiptOutcomeInterrupted, receipts[0].ReceiptOutcome)
	require.Equal(t, DispatchStateNotDispatched, receipts[0].DispatchState)
	require.Equal(t, "delegate", receipts[1].ToolCallID)
	require.Equal(t, ReceiptOutcomeSuccess, receipts[1].ReceiptOutcome)
	require.Equal(t, int32(1), dispatcher.calls.Load())
	require.Zero(t, prepared["denied"], "the rejected call is never prepared")
}

func TestDelegationStagesContinueAfterKnownPolicyDenial(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	dispatcher := &stagedTestDispatcher{}
	prepared := map[string]int{}
	var mu sync.Mutex
	registry := stagedTestRegistry(t, root, true, false, dispatcher, prepared, &mu)
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, req tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: req.Call.ToolCall().ID != "denied", Generations: req.Generations}, nil
	}))
	sink := &consumerTestSink{}
	model := &scriptedChatModel{seq: []*llm.Message{{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
		{ID: "denied", Name: "read", Args: map[string]any{"path": filepath.Join(root, "input.txt")}},
		{ID: "delegate", Name: "dispatch_subagent", Args: map[string]any{"prompt": "research"}},
	}}, {Role: llm.RoleAssistant, Content: "done"}}}
	agent := NewAgentImpl("staged-policy", NewLoopAgent(WithLLM(model), WithTools(registry), WithExecutionMode(ExecutionModeParallel)))
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "staged-policy-run"}, nil)
	owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "parent-session", WorkspaceID: "workspace", CanonicalWorkspaceRoot: root})
	_, err = owner.run(5*time.Second, func(ctx context.Context) (Result, error) {
		return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "perform actions"})
	})
	require.NoError(t, err)
	_, batches, receipts, _ := sink.snapshot()
	require.Len(t, batches, 2)
	require.Len(t, batches[0].KnownReceipts, 1)
	require.Empty(t, batches[0].Intents)
	require.Len(t, receipts, 2)
	require.Equal(t, "denied", receipts[0].ToolCallID)
	require.Equal(t, ReceiptOutcomeError, receipts[0].ReceiptOutcome)
	require.Equal(t, DispatchStateNotDispatched, receipts[0].DispatchState)
	require.Contains(t, receipts[0].Message.Content, tools.ErrPreparedToolPolicyDenied.Error())
	require.Equal(t, ReceiptOutcomeSuccess, receipts[1].ReceiptOutcome)
	require.Equal(t, int32(1), dispatcher.calls.Load())
}

func TestDelegationStagesUnknownOutcomeDoesNotReserveSuffix(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	dispatcher := &stagedTestDispatcher{}
	prepared := map[string]int{}
	var mu sync.Mutex
	registry := stagedTestRegistry(t, root, true, false, dispatcher, prepared, &mu)
	unknown := &stagedUnknownTool{}
	require.NoError(t, registry.RegisterTrustedInlineControl(t.Context(), unknown))
	sink := &consumerTestSink{}
	model := &scriptedChatModel{seq: []*llm.Message{{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
		{ID: "unknown", Name: unknown.Name(), Args: map[string]any{}},
		{ID: "delegate", Name: "dispatch_subagent", Args: map[string]any{"prompt": "must not run"}},
	}}, {Role: llm.RoleAssistant, Content: "must not be requested"}}}
	agent := NewAgentImpl("staged-unknown", NewLoopAgent(WithLLM(model), WithTools(registry), WithExecutionMode(ExecutionModeParallel)))
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "staged-unknown-run"}, nil)
	owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "parent-session", WorkspaceID: "workspace", CanonicalWorkspaceRoot: root})
	_, err = owner.run(5*time.Second, func(ctx context.Context) (Result, error) {
		return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "perform actions"})
	})
	require.Error(t, err)
	require.Equal(t, int32(1), unknown.calls.Load())
	require.Zero(t, dispatcher.calls.Load())
	_, batches, receipts, _ := sink.snapshot()
	for _, batch := range batches {
		for _, intent := range batch.Intents {
			require.NotEqual(t, "delegate", intent.ToolCallID, "unknown prior effects forbid suffix reservation")
		}
	}
	require.Len(t, receipts, 1)
	require.Equal(t, ReceiptOutcomeUnknown, receipts[0].ReceiptOutcome)
	require.Empty(t, sink.resolutionSnapshot(), "unknown results cannot be upgraded to a closure proof")
	require.Equal(t, 1, model.idx)
}

func TestDelegationStagesDoNotUpgradeUnownedSameName(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	own, unowned := &stagedTestDispatcher{}, &stagedTestDispatcher{}
	prepared := map[string]int{}
	var mu sync.Mutex
	registry := &stagedMixedAuthorityRegistry{
		MiddlewareToolRegistry: stagedTestRegistry(t, root, true, false, own, prepared, &mu),
		unowned:                stagedTestRegistry(t, root, false, false, unowned, prepared, &mu),
	}
	sink := &consumerTestSink{}
	model := &scriptedChatModel{seq: []*llm.Message{{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
		{ID: "unowned", Name: "dispatch_subagent", Args: map[string]any{"prompt": "unowned"}},
		{ID: "owned", Name: "dispatch_subagent", Args: map[string]any{"prompt": "owned"}},
	}}, {Role: llm.RoleAssistant, Content: "done"}}}
	agent := NewAgentImpl("staged-authority", NewLoopAgent(WithLLM(model), WithTools(registry), WithExecutionMode(ExecutionModeParallel)))
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "staged-authority-run"}, nil)
	owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "parent-session", WorkspaceID: "workspace", CanonicalWorkspaceRoot: root})
	_, err = owner.run(5*time.Second, func(ctx context.Context) (Result, error) {
		return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "perform actions"})
	})
	require.NoError(t, err)
	steps, batches, receipts, _ := sink.snapshot()
	require.Len(t, batches, 2)
	require.Len(t, batches[0].Intents, 1)
	require.Len(t, batches[1].Intents, 1)
	require.Equal(t, "unowned", batches[0].Intents[0].ToolCallID)
	require.Nil(t, batches[0].Intents[0].ManagedDelegationProof, "an unowned same-name tool receives no delegation authority")
	require.Equal(t, "owned", batches[1].Intents[0].ToolCallID)
	require.NotNil(t, batches[1].Intents[0].ManagedDelegationProof)
	var starts []int
	for i, step := range steps {
		if step == "begin" {
			starts = append(starts, i)
		}
	}
	require.Len(t, starts, 2)
	require.Contains(t, steps[starts[0]+1:starts[1]], "resolution")
	require.Len(t, receipts, 2)
	require.Equal(t, int32(1), own.calls.Load())
	require.Equal(t, int32(1), unowned.calls.Load())
}

func TestDelegationStagesPreparationFenceFailureStopsSuffix(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault error
		want  error
	}{
		{"fence-changed", tools.ErrDispatchFenceChanged, tools.ErrDispatchFenceChanged},
		{"fence-unavailable", tools.ErrDispatchFenceUnavailable, tools.ErrDispatchFenceUnavailable},
		{"stale-proxy", tools.ErrPreparedProxyRegistrationStale, tools.ErrPreparedProxyRegistrationStale},
		{"wrapped-fence", fmt.Errorf("prepare boundary: %w", tools.ErrDispatchFenceChanged), tools.ErrDispatchFenceChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			path := filepath.Join(root, "input.txt")
			require.NoError(t, os.WriteFile(path, []byte("fixture"), 0o600))
			dispatcher := &stagedTestDispatcher{}
			prepared := map[string]int{}
			var mu sync.Mutex
			base := stagedTestRegistry(t, root, true, false, dispatcher, prepared, &mu)
			later := &stagedCountingTool{}
			require.NoError(t, base.RegisterTrustedInlineControl(t.Context(), later))
			registry := &stagedPrepareFaultRegistry{MiddlewareToolRegistry: base, err: tc.fault}
			sink := &consumerTestSink{}
			model := &scriptedChatModel{seq: []*llm.Message{{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "read-before", Name: "read", Args: map[string]any{"path": path}},
				{ID: "fenced", Name: "read", Args: map[string]any{"path": path}},
				{ID: "after", Name: later.Name(), Args: map[string]any{}},
				{ID: "delegate", Name: "dispatch_subagent", Args: map[string]any{"prompt": "must not run"}},
			}}, {Role: llm.RoleAssistant, Content: "must not be requested"}}}
			agent := NewAgentImpl("staged-fence", NewLoopAgent(WithLLM(model), WithTools(registry), WithExecutionMode(ExecutionModeParallel)))
			owner := newRunOwner(context.Background(), nil, RunClaim{ID: "staged-fence-run"}, nil)
			owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "parent-session", WorkspaceID: "workspace", CanonicalWorkspaceRoot: root})
			_, err = owner.run(5*time.Second, func(ctx context.Context) (Result, error) {
				return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "perform actions"})
			})
			require.ErrorIs(t, err, tc.want)
			require.Zero(t, later.calls.Load())
			require.Zero(t, dispatcher.calls.Load())
			steps, batches, receipts, _ := sink.snapshot()
			require.Len(t, batches, 2, "the known authority rejection is its own barrier")
			require.Len(t, batches[0].Intents, 1)
			require.Equal(t, "read-before", batches[0].Intents[0].ToolCallID)
			require.Len(t, batches[1].KnownReceipts, 1)
			require.Empty(t, batches[1].Intents)
			for _, batch := range batches {
				for _, intent := range batch.Intents {
					require.NotContains(t, []string{"after", "delegate"}, intent.ToolCallID)
				}
			}
			require.Len(t, receipts, 2)
			require.Equal(t, "read-before", receipts[0].ToolCallID)
			require.Equal(t, ReceiptOutcomeSuccess, receipts[0].ReceiptOutcome)
			require.Equal(t, "fenced", receipts[1].ToolCallID)
			require.Equal(t, ReceiptOutcomeError, receipts[1].ReceiptOutcome)
			require.Equal(t, DispatchStateNotDispatched, receipts[1].DispatchState)
			require.ErrorContains(t, tc.fault, tc.want.Error())
			require.Contains(t, receipts[1].Message.Content, tc.fault.Error())
			var begins []int
			for i, step := range steps {
				if step == "begin" {
					begins = append(begins, i)
				}
			}
			require.Len(t, begins, 2)
			require.Contains(t, steps[begins[0]+1:begins[1]], "resolution", "the earlier action must resolve before the fatal known receipt")
			require.Len(t, sink.resolutionSnapshot(), 1)
			require.Equal(t, 1, model.idx)
		})
	}
}

func TestDelegationStagesOrdinaryOnlyRemainParallel(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	dispatcher := &stagedTestDispatcher{}
	prepared := map[string]int{}
	var mu sync.Mutex
	registry := stagedTestRegistry(t, root, true, false, dispatcher, prepared, &mu)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	probe := &stagedOrdinaryBarrier{started: make(chan string, 2), release: release}
	require.NoError(t, registry.RegisterTrustedInlineControl(t.Context(), probe))
	sink := &consumerTestSink{}
	model := &scriptedChatModel{seq: []*llm.Message{{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
		{ID: "ordinary-one", Name: probe.Name(), Args: map[string]any{}},
		{ID: "ordinary-two", Name: probe.Name(), Args: map[string]any{}},
	}}, {Role: llm.RoleAssistant, Content: "done"}}}
	agent := NewAgentImpl("ordinary-parallel", NewLoopAgent(WithLLM(model), WithTools(registry), WithExecutionMode(ExecutionModeParallel)))
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "ordinary-parallel-run"}, nil)
	owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "parent-session", WorkspaceID: "workspace", CanonicalWorkspaceRoot: root})
	done := make(chan error, 1)
	go func() {
		_, runErr := owner.run(5*time.Second, func(ctx context.Context) (Result, error) {
			return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "perform actions"})
		})
		done <- runErr
	}()
	started := make(map[string]bool)
	for len(started) < 2 {
		select {
		case id := <-probe.started:
			started[id] = true
		case <-time.After(2 * time.Second):
			t.Fatal("both ordinary executors must start before either is released")
		}
	}
	releaseOnce.Do(func() { close(release) })
	require.NoError(t, <-done)
	_, batches, receipts, _ := sink.snapshot()
	require.Len(t, batches, 1)
	require.Len(t, batches[0].Intents, 2)
	require.Len(t, receipts, 2)
	require.Zero(t, dispatcher.calls.Load())
}
