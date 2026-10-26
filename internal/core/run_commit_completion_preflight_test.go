package core

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestRunCommitCompletionPreflightRejectsBeforeIntentAndContinues(t *testing.T) {
	for _, test := range []struct {
		name string
		mode ExecutionMode
	}{{name: "sequential", mode: ExecutionModeSequential}, {name: "parallel", mode: ExecutionModeParallel}} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			registry := newPreparedConsumerOrdinaryToolRegistry(t, &testToolDef{name: "read", handler: func(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
				calls.Add(1)
				return &tools.ToolResult{Output: "must never execute"}, nil
			}})
			model := &scriptedChatModel{seq: []*llm.Message{
				{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "unsupported-one", Name: "read"}, {ID: "unsupported-two", Name: "read"}}},
				{Role: llm.RoleAssistant, Content: "completed after known rejection"},
			}}
			sink := &consumerTestSink{}
			agent := NewAgentImpl("durable-preflight", NewLoopAgent(WithLLM(model), WithTools(registry), WithExecutionMode(test.mode)))
			owner := newRunOwner(context.Background(), nil, RunClaim{ID: "preflight-" + test.name}, nil)
			owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "session-1", WorkspaceID: "workspace-1"})
			_, runErr := owner.run(time.Second, func(ctx context.Context) (Result, error) {
				return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "inspect"})
			})
			require.NoError(t, runErr)
			require.Zero(t, calls.Load(), "an unsupported action must not reach its executor")
			_, batches, receipts, terminal := sink.snapshot()
			for _, batch := range batches {
				require.Empty(t, batch.Intents, "unsupported calls have known receipts without durable dispatch reservations")
			}
			require.Len(t, receipts, 2)
			for _, receipt := range receipts {
				require.Equal(t, DispatchStateNotDispatched, receipt.DispatchState)
				require.Empty(t, receipt.ActionID)
				require.Equal(t, ReceiptOutcomeError, receipt.ReceiptOutcome)
				require.Contains(t, receipt.Message.Content, "completion")
			}
			require.Empty(t, sink.executionSnapshot())
			require.Empty(t, sink.resolutionSnapshot())
			require.NotNil(t, terminal)
			require.Equal(t, RunOutcomeSuccess, terminal.Outcome)
			require.Equal(t, 2, model.idx, "the next provider response follows the known error receipts")
		})
	}
}

func TestRunCommitCompletionPreflightPreservesGuardError(t *testing.T) {
	guardErr := errors.New("project generation revoked")
	ctx := WithRunCapabilityGuard(context.Background(), providerGenerationAdmissionGuard{check: func() error { return guardErr }})
	registry := newPreparedConsumerOrdinaryToolRegistry(t, scriptedTool{name: "read"})
	consumer := &runCommitConsumer{}
	_, _, err := consumer.prepareInvocation(ctx, registry, llm.ToolCall{Name: "read"}, nil)
	require.ErrorIs(t, err, guardErr)
}

func TestRunCommitCompletionPreflightRejectsOpaqueInvocation(t *testing.T) {
	base := newPreparedConsumerReadRegistry(t)
	registry := &panicPreparedRegistry{ToolRegistry: base, ToolCallPreparer: base}
	consumer := &runCommitConsumer{}
	_, _, err := consumer.prepareInvocation(context.Background(), registry, llm.ToolCall{Name: "read", Args: map[string]any{"path": "input.txt"}}, nil)
	require.ErrorIs(t, err, ErrRunCommitCompletionUnsupported, "opaque embedded dispatch must be rejected before its panic can execute")
}

type ignoredPreparedStreamSinkRegistry struct {
	tools.ToolRegistry
	tools.ToolCallPreparer
}

func (registry ignoredPreparedStreamSinkRegistry) PrepareStreamingToolCall(ctx context.Context, call tools.ToolCall, _ tools.SynchronousStreamSink) (tools.PreparedStreamingInvocation, error) {
	invocation, err := registry.ToolCallPreparer.PrepareToolCall(ctx, call)
	if err != nil {
		return nil, err
	}
	return invocation.(tools.PreparedStreamingInvocation), nil
}

func TestRunCommitCompletionPreflightRejectsIgnoredStreamingSink(t *testing.T) {
	ctx := context.Background()
	registered := tools.NewDeferredToolRegistryAdapter(tools.NewDefaultToolRegistry())
	require.NoError(t, tools.RegisterDefaults(ctx, registered, tools.WithRegisteredBuiltinWhitelist([]string{"bash"}), tools.WithRegisteredBashSandbox(tools.AllowAllSandbox{})))
	base := tools.NewMiddlewareToolRegistry(registered)
	require.NoError(t, base.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	require.NoError(t, base.SetPreparedToolStages(tools.PreparedToolStages{}))
	definition, err := base.Get(ctx, "bash")
	require.NoError(t, err)
	_, ok := definition.(tools.StreamingBashTool)
	require.True(t, ok)
	registry := ignoredPreparedStreamSinkRegistry{ToolRegistry: base, ToolCallPreparer: base}
	stream := NewEventStream(8)
	t.Cleanup(stream.Close)
	consumer := &runCommitConsumer{}
	_, _, err = consumer.prepareInvocation(ctx, registry, llm.ToolCall{Name: "bash", Args: map[string]any{"command": "printf must-not-execute"}}, stream)
	require.ErrorIs(t, err, ErrRunCommitCompletionUnsupported, "plain preparation cannot stand in for captured streaming preparation")
	invocation, _, err := consumer.prepareInvocation(ctx, base, llm.ToolCall{Name: "bash", Args: map[string]any{"command": "printf never-dispatched"}}, nil)
	require.NoError(t, err, "ordinary Bash Dispatch still has a reviewed completion path")
	require.NotNil(t, invocation)
	require.Nil(t, invocation.CompletionSignal().Done())
}
