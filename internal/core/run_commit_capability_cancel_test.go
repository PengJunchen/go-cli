package core

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

type preparedCancelBoundaryTool struct {
	calls  int
	cancel context.CancelFunc
}

func (*preparedCancelBoundaryTool) Name() string { return "cancel_boundary" }
func (*preparedCancelBoundaryTool) Description() string {
	return "controlled prepared dispatch boundary"
}
func (tool *preparedCancelBoundaryTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	tool.calls++
	if tool.cancel != nil {
		tool.cancel()
	}
	return &tools.ToolResult{Output: "executor entered"}, nil
}

type preparedCancelBoundaryGuard struct {
	cancelBeforeCommit context.CancelFunc
	completionErr      error
}

func (guard preparedCancelBoundaryGuard) BeginCall(ctx context.Context, _ string) (RunCapabilityInvocation, error) {
	// Preparation has completed, but the real sealed invocation has not yet
	// entered Dispatch or crossed its final generation-fence commit.
	if guard.cancelBeforeCommit != nil {
		guard.cancelBeforeCommit()
	}
	return preparedCancelBoundaryInvocation{ctx: ctx, completionErr: guard.completionErr}, nil
}
func (preparedCancelBoundaryGuard) Check() error { return nil }

type preparedCancelBoundaryInvocation struct {
	ctx           context.Context
	completionErr error
}

func (invocation preparedCancelBoundaryInvocation) Context() context.Context { return invocation.ctx }
func (invocation preparedCancelBoundaryInvocation) Complete() error {
	if invocation.completionErr != nil {
		return invocation.completionErr
	}
	return invocation.ctx.Err()
}
func (preparedCancelBoundaryInvocation) Release() {}

func TestPreparedCapabilityCancellationKeepsExactNeverDispatchedBoundary(t *testing.T) {
	policyRevoked := errors.New("actual permission authority revoked")
	for _, test := range []struct {
		name          string
		beforeCommit  bool
		completionErr error
		wantUnknown   bool
		wantCalls     int
	}{
		{name: "known-before-commit", beforeCommit: true},
		{name: "committed-cancel-remains-unknown", wantUnknown: true, wantCalls: 1},
		{name: "compound-authority-failure-remains-unknown", beforeCommit: true, completionErr: errors.Join(context.Canceled, policyRevoked), wantUnknown: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tool := &preparedCancelBoundaryTool{}
			guard := preparedCancelBoundaryGuard{completionErr: test.completionErr}
			if test.beforeCommit {
				guard.cancelBeforeCommit = cancel
			} else {
				tool.cancel = cancel
			}
			registry := newPreparedConsumerOrdinaryToolRegistry(t, tool)
			invocation, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: tool.Name()})
			require.NoError(t, err)
			result := dispatchPreparedRunCommitCall(WithRunCapabilityGuard(ctx, guard), invocation, nil, 0)
			require.True(t, result.returned)
			require.Equal(t, !test.beforeCommit, result.committed)
			require.Equal(t, test.wantCalls, tool.calls, "DispatchCommitted=false must correspond to an executor that was never entered")
			require.ErrorIs(t, result.err, context.Canceled)
			require.Equal(t, test.wantUnknown, hasUnknownToolOutcome(result.err))
			if test.completionErr != nil {
				require.ErrorIs(t, result.err, policyRevoked)
			}
		})
	}
}
