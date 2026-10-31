package core_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type consumerUnknownChildCause struct{ cause error }

func (err *consumerUnknownChildCause) Error() string {
	return "fixture child side effects remain unknown: " + err.cause.Error()
}
func (err *consumerUnknownChildCause) Unwrap() error    { return err.cause }
func (*consumerUnknownChildCause) OutcomeUnknown() bool { return true }

// TestRunCommitConsumerKeepsUnknownChildCauseWithoutResolvingParent exercises
// the actual Harness, sealed dispatcher and durable SessionRunCommitSink.
func TestRunCommitConsumerKeepsUnknownChildCauseWithoutResolvingParent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workspace"), 0o700))
	ctx := context.Background()
	workspaceID := "workspace-v1-unknown-child-cause"
	index, err := session.NewWorkspaceQuarantineIndex()
	require.NoError(t, err)
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	assertWorkspaceIndexUsesTempHome(t, root, workspaceID)
	prepared, err := session.NewDurableJSONLSessionPreparer(filepath.Join(root, "durable"), index).PrepareNew(ctx, "preserve unknown child cause")
	require.NoError(t, err)
	t.Cleanup(func() { _ = prepared.Close() })
	sink, err := session.NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)

	originalSentinel := errors.New("fixture child business operation outcome is unknown")
	original := &consumerUnknownChildCause{cause: originalSentinel}
	dispatcher := core.NewDefaultSubagentDispatcher(core.NewManagedSubAgentFactory(
		func(context.Context, core.SubAgentConfig, string, <-chan string, func(core.AgentEvent)) (core.AgentMessage, error) {
			return core.AgentMessage{}, original
		},
	))
	definition, err := core.NewManagedSubagentTool(dispatcher)
	require.NoError(t, err)
	registry := tools.NewMiddlewareToolRegistry(tools.NewDefaultToolRegistry())
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	require.NoError(t, registry.RegisterTrustedInlineControl(ctx, definition))
	model := mock.NewMockLLMServer(mock.NewConversationTemplate("unknown-child-cause", "preserve underlying cause",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{
			ID: "unknown-child-dispatch", Name: "dispatch_subagent", Args: map[string]any{"prompt": "fixture delegated task"},
		}}},
		mock.ConversationTurn{AssistantContent: "must not be requested"},
	))
	agent := core.NewAgentImpl("durable-child-unknown-cause", core.NewLoopAgent(core.WithLLM(model), core.WithTools(registry)))
	harness := core.NewHarnessImpl(agent)
	stream, err := harness.Submit(core.WithRunCommit(ctx, sink, core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID}), "delegate fixture")
	require.NoError(t, err)
	for range stream.Events() {
	}
	_, runErr := stream.Result()

	require.ErrorIs(t, runErr, core.ErrRunCommitQuiescenceUnproven)
	require.Equal(t, 1, model.CallCount(), "unknown child must fence later provider calls")
	require.Empty(t, dispatcher.ListRunning(), "dispatcher callback and event drain have returned")
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	var execution, receipt, terminal session.SessionEntry
	resolutionCount := 0
	for _, row := range snapshot.DurableEntries {
		if row.ActionState == session.ActionStateExecution {
			execution = row
		}
		if row.Type == session.EntryTypeTool && row.ReceiptOutcome != "" {
			receipt = row
		}
		if row.CheckpointKind == session.CheckpointExecutionResolution {
			resolutionCount++
		}
		if row.CheckpointKind == session.CheckpointRunTerminal {
			terminal = row
		}
	}
	require.NotEmpty(t, execution.ID)
	require.Equal(t, core.DispatchStateDispatched, execution.DispatchState)
	require.Equal(t, core.RunExecutionCleanupPending, execution.ResourceState)
	require.Empty(t, execution.CompletionProof)
	require.NotEmpty(t, receipt.ID)
	require.Equal(t, core.ReceiptOutcomeUnknown, receipt.ReceiptOutcome)
	require.Zero(t, resolutionCount, "unknown inline completion cannot authorize a resolution")
	require.NotEmpty(t, terminal.ID)
	require.Equal(t, core.RunOutcomeUnknown, terminal.RunOutcome)
	pending, err := session.InspectPendingActions(snapshot)
	require.NoError(t, err)
	require.Len(t, pending, 1, "preserving cause must not clear the unresolved parent action")

	// New RED assertions target the error of an actual consumed durable action,
	// rather than directly constructing the error wrapper under test.
	assert.ErrorIs(t, runErr, originalSentinel)
	var recovered *consumerUnknownChildCause
	if assert.ErrorAs(t, runErr, &recovered) {
		assert.Same(t, original, recovered)
	}
	assert.Contains(t, runErr.Error(), original.Error())
	assert.Equal(t, 1, strings.Count(runErr.Error(), core.ErrRunCommitQuiescenceUnproven.Error()))
}
