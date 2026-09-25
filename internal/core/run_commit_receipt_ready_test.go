package core

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

var errReviewResolutionBeforeProof = errors.New("review: resolution attempted before completion proof was durable")

type reviewReceiptReadySink struct {
	*consumerTestSink
	owner        *runOwner
	release      chan struct{}
	releaseOnce  sync.Once
	readyDetails chan runCompletionTaskDetails
	mu           sync.Mutex
	earlyResolve int
}

func (sink *reviewReceiptReadySink) RecordReceipt(ctx context.Context, receipt RunToolReceipt) error {
	if err := sink.consumerTestSink.RecordReceipt(ctx, receipt); err != nil {
		return err
	}
	sink.releaseOnce.Do(func() { close(sink.release) })
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if details, ok := sink.owner.completionDetails(1); ok && details.state == runCompletionTaskReady {
			sink.readyDetails <- details
			return nil
		}
		select {
		case <-timer.C:
			return errors.New("review: completion did not become ready while receipt was blocked")
		case <-ticker.C:
		}
	}
}

func (sink *reviewReceiptReadySink) ResolveExecution(ctx context.Context, resolution ExecutionResolution) error {
	for _, execution := range sink.executionSnapshot() {
		if execution.ActionID == resolution.ActionID && execution.CompletionProof != "" && execution.ResourceState == RunExecutionQuiescent {
			return sink.consumerTestSink.ResolveExecution(ctx, resolution)
		}
	}
	sink.mu.Lock()
	sink.earlyResolve++
	sink.mu.Unlock()
	return errReviewResolutionBeforeProof
}

func (sink *reviewReceiptReadySink) earlyResolveCount() int {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.earlyResolve
}

func TestRunCommitSequentialRechecksReceiptTimeProofBeforeResolve(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	var cancelDispatch context.CancelFunc
	queue := tools.NewDefaultFileMutationQueue().(*tools.DefaultFileMutationQueue) //nolint:errcheck
	t.Cleanup(func() { _ = queue.Close() })

	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	deferred := tools.NewDeferredToolRegistryAdapter(base)
	require.NoError(t, tools.RegisterDefaults(ctx, deferred, tools.WithRegisteredBuiltinWhitelist([]string{"write"})))
	registry := tools.NewMiddlewareToolRegistry(deferred)
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	wrapper := tools.ToolExecutorWrapper(func(next func(context.Context, tools.ToolCall) (*tools.ToolResult, error)) func(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
		return func(callCtx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
			cancelDispatch()
			<-release
			return next(context.WithoutCancel(callCtx), call)
		}
	})
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{
		MutationQueue:                queue,
		ExecutionWrappers:            []tools.ToolExecutorWrapper{wrapper},
		SynchronousExecutionWrappers: true,
	}))

	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "receipt-ready-race"}, nil)
	sink := &reviewReceiptReadySink{
		consumerTestSink: &consumerTestSink{},
		owner:            owner,
		release:          release,
		readyDetails:     make(chan runCompletionTaskDetails, 1),
	}
	owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "receipt-race-session", WorkspaceID: "receipt-race-workspace"})
	call := llm.ToolCall{ID: "write-race", Name: "write", Args: map[string]any{
		"path":    filepath.Join(t.TempDir(), "out.txt"),
		"content": "queued content",
	}}
	assistant := AgentMessage{Role: string(llm.RoleAssistant), ToolCalls: []llm.ToolCall{call}}
	transcript := []TranscriptEntry{{Message: AgentMessage{Role: string(llm.RoleUser), Content: "write"}}, {Message: assistant}}
	messages := []llm.Message{{Role: llm.RoleUser, Content: "write"}, {Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call}}}

	_, runErr := owner.run(3*time.Second, func(runCtx context.Context) (Result, error) {
		consumer, err := newRunCommitConsumer(runCtx)
		if err != nil {
			return Result{}, err
		}
		dispatchCtx, cancel := context.WithCancel(runCtx)
		cancelDispatch = cancel
		_, callErr := consumer.executeSequentialCall(dispatchCtx, registry, 2, assistant, call, nil, &transcript, &messages)
		cancel()
		terminalErr := consumer.commitTerminal(transcript, nil, RunOutcomeUnknown)
		return Result{Outcome: RunOutcomeUnknown}, errors.Join(callErr, terminalErr)
	})

	select {
	case details := <-sink.readyDetails:
		require.False(t, details.pendingResourceUnproven)
		require.NotEmpty(t, details.proof)
	case <-time.After(time.Second):
		t.Fatal("receipt hook did not observe completion proof becoming ready")
	}
	require.NotErrorIs(t, runErr, errReviewResolutionBeforeProof)
	require.Zero(t, sink.earlyResolveCount(), "resolution must follow a durable positive proof execution even when readiness arrives during receipt persistence")
	for _, execution := range sink.executionSnapshot() {
		if execution.ActionID != sink.intents[0].Intents[0].ActionID || execution.CompletionProof == "" {
			continue
		}
		require.Equal(t, RunExecutionQuiescent, execution.ResourceState)
		return
	}
	t.Fatal("no positive proof execution was durably recorded before resolution")
}
