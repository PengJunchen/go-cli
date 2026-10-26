package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type runCommitCountingCompactor struct{ calls int }

func (compactor *runCommitCountingCompactor) Compact(_ context.Context, items []compaction.TurnItem, _ int, _ compaction.TokenEstimator) (compaction.CompactionResult, error) {
	compactor.calls++
	return compaction.CompactionResult{Items: items}, nil
}

type consumerTestSink struct {
	mu                sync.Mutex
	steps             []string
	intents           []RunIntentBatch
	executions        []ActionExecution
	receipts          []RunToolReceipt
	resolutions       []ExecutionResolution
	terminal          *RunTerminal
	beginErr          error
	recordReceiptErr  error
	cancelAfterBegin  context.CancelFunc
	afterBegin        func()
	recordReceiptHook func(RunToolReceipt)
	commitContexts    []consumerCommitContext
}

type consumerCommitContext struct {
	runID       string
	bound       bool
	hasDeadline bool
	deadline    time.Time
}

type runCommitPendingTestTracker struct {
	pendingResources   int
	pendingAttachments int
	attachedDone       []<-chan struct{}
	handoffs           int
}

func (*runCommitPendingTestTracker) RegisterWorker(string, string) func() { return func() {} }
func (tracker *runCommitPendingTestTracker) RegisterPendingResource(string, string, <-chan struct{}) {
	tracker.pendingResources++
}
func (*runCommitPendingTestTracker) RegisterCompletionTask(ActionIntent, tools.PreparedCompletionSignal) uint64 {
	return 1
}
func (tracker *runCommitPendingTestTracker) AttachPendingExecution(_ uint64, done <-chan struct{}) bool {
	tracker.pendingAttachments++
	tracker.attachedDone = append(tracker.attachedDone, done)
	return true
}
func (tracker *runCommitPendingTestTracker) HandoffCompletionTask(uint64) { tracker.handoffs++ }

type runCommitPendingPreparedInvocation struct {
	tools.PreparedInvocation
	completionSignal    *tools.PreparedCompletionSignal
	pendingDoneOverride <-chan struct{}
	overridePendingDone bool
}

func (invocation runCommitPendingPreparedInvocation) CompletionSignal() tools.PreparedCompletionSignal {
	if invocation.completionSignal != nil {
		return *invocation.completionSignal
	}
	return invocation.PreparedInvocation.CompletionSignal()
}

func (invocation runCommitPendingPreparedInvocation) Dispatch(ctx context.Context) (*tools.ToolResult, error) {
	_, err := invocation.PreparedInvocation.Dispatch(ctx)
	if err != nil {
		return nil, err
	}
	done := invocation.PreparedInvocation.CompletionSignal().Done()
	if invocation.overridePendingDone {
		done = invocation.pendingDoneOverride
	}
	return nil, runCommitPreparedPendingError{done: done}
}

type runCommitPreparedPendingError struct{ done <-chan struct{} }

func (runCommitPreparedPendingError) Error() string                      { return "prepared action outcome is pending" }
func (runCommitPreparedPendingError) OutcomeUnknown() bool               { return true }
func (err runCommitPreparedPendingError) ExecutionDone() <-chan struct{} { return err.done }
func (runCommitPreparedPendingError) Unwrap() error                      { return errors.New("prepared action interrupted") }

func (s *consumerTestSink) recordContext(ctx context.Context) {
	deadline, hasDeadline := ctx.Deadline()
	_, _, bound := RunCommitFromContext(ctx)
	s.mu.Lock()
	s.commitContexts = append(s.commitContexts, consumerCommitContext{
		runID:       RunIDFromContext(ctx),
		bound:       bound,
		hasDeadline: hasDeadline,
		deadline:    deadline,
	})
	s.mu.Unlock()
}

func (s *consumerTestSink) add(step string) {
	s.mu.Lock()
	s.steps = append(s.steps, step)
	s.mu.Unlock()
}

func (s *consumerTestSink) CommitInput(ctx context.Context, _ RunCommitInput) error {
	s.recordContext(ctx)
	s.add("input")
	return nil
}

func (s *consumerTestSink) BeginIntentBatch(ctx context.Context, batch RunIntentBatch) error {
	s.recordContext(ctx)
	s.mu.Lock()
	s.steps = append(s.steps, "begin")
	s.intents = append(s.intents, batch)
	err := s.beginErr
	cancel := s.cancelAfterBegin
	s.cancelAfterBegin = nil
	afterBegin := s.afterBegin
	s.afterBegin = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if afterBegin != nil {
		afterBegin()
	}
	return err
}

func (s *consumerTestSink) RecordExecution(ctx context.Context, execution ActionExecution) error {
	s.recordContext(ctx)
	s.mu.Lock()
	s.steps = append(s.steps, "execution")
	s.executions = append(s.executions, execution)
	s.mu.Unlock()
	return nil
}

func (s *consumerTestSink) RecordReceipt(ctx context.Context, receipt RunToolReceipt) error {
	s.recordContext(ctx)
	s.mu.Lock()
	s.steps = append(s.steps, "receipt")
	err := s.recordReceiptErr
	if err == nil {
		s.receipts = append(s.receipts, receipt)
	}
	hook := s.recordReceiptHook
	s.mu.Unlock()
	if hook != nil {
		hook(receipt)
	}
	return err
}

func (s *consumerTestSink) ResolveExecution(ctx context.Context, resolution ExecutionResolution) error {
	s.recordContext(ctx)
	s.mu.Lock()
	s.steps = append(s.steps, "resolution")
	s.resolutions = append(s.resolutions, resolution)
	s.mu.Unlock()
	return nil
}

func (s *consumerTestSink) CommitCompaction(ctx context.Context, _ CompactionProjection) error {
	s.recordContext(ctx)
	s.add("compaction")
	return nil
}

func (s *consumerTestSink) CommitTerminal(ctx context.Context, terminal RunTerminal) error {
	s.recordContext(ctx)
	s.mu.Lock()
	s.steps = append(s.steps, "terminal")
	copy := terminal
	s.terminal = &copy
	s.mu.Unlock()
	return nil
}

func (s *consumerTestSink) contextSnapshot() []consumerCommitContext {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]consumerCommitContext(nil), s.commitContexts...)
}

func (s *consumerTestSink) executionSnapshot() []ActionExecution {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ActionExecution(nil), s.executions...)
}

func (s *consumerTestSink) resolutionSnapshot() []ExecutionResolution {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ExecutionResolution(nil), s.resolutions...)
}

func (s *consumerTestSink) snapshot() ([]string, []RunIntentBatch, []RunToolReceipt, *RunTerminal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.steps...), append([]RunIntentBatch(nil), s.intents...), append([]RunToolReceipt(nil), s.receipts...), s.terminal
}

func newPreparedConsumerRegistry(t *testing.T, names ...string) *tools.MiddlewareToolRegistry {
	t.Helper()
	ctx := context.Background()
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	deferred := tools.NewDeferredToolRegistryAdapter(base)
	require.NoError(t, tools.RegisterDefaults(ctx, deferred, tools.WithRegisteredBuiltinWhitelist(names)))
	registry := tools.NewMiddlewareToolRegistry(deferred)
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	return registry
}

func newPreparedConsumerReadRegistry(t *testing.T) *tools.MiddlewareToolRegistry {
	t.Helper()
	return newPreparedConsumerRegistry(t, "read")
}

func newPreparedConsumerScopedSearchRegistry(t *testing.T, root, name string) *tools.MiddlewareToolRegistry {
	t.Helper()
	ctx := context.Background()
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	deferred := tools.NewDeferredToolRegistryAdapter(base)
	require.NoError(t, tools.RegisterDefaults(ctx, deferred,
		tools.WithRegisteredBuiltinWhitelist([]string{name}),
		tools.WithRegisteredPathWhitelist([]string{root}),
		tools.WithRegisteredCanonicalWorkspaceRoot(root),
	))
	registry := tools.NewMiddlewareToolRegistry(deferred)
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	return registry
}

func newPreparedConsumerDefaultSearchRegistry(t *testing.T, name string) *tools.MiddlewareToolRegistry {
	t.Helper()
	ctx := context.Background()
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	deferred := tools.NewDeferredToolRegistryAdapter(base)
	require.NoError(t, tools.RegisterDefaults(ctx, deferred, tools.WithRegisteredBuiltinWhitelist([]string{name})))
	registry := tools.NewMiddlewareToolRegistry(deferred)
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	return registry
}

func newPreparedConsumerOrdinaryToolRegistry(t *testing.T, def tools.ToolDefinition) *tools.MiddlewareToolRegistry {
	t.Helper()
	ctx := context.Background()
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	deferred := tools.NewDeferredToolRegistryAdapter(base)
	require.NoError(t, deferred.Register(ctx, def))
	registry := tools.NewMiddlewareToolRegistry(deferred)
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	return registry
}

type panicPreparedInvocation struct{ tools.PreparedInvocation }

func (panicPreparedInvocation) Dispatch(context.Context) (*tools.ToolResult, error) {
	panic("prepared dispatch panic")
}

type panicPreparedRegistry struct {
	tools.ToolRegistry
	tools.ToolCallPreparer
}

func (registry *panicPreparedRegistry) PrepareToolCall(ctx context.Context, call tools.ToolCall) (tools.PreparedInvocation, error) {
	invocation, err := registry.ToolCallPreparer.PrepareToolCall(ctx, call)
	if err != nil {
		return nil, err
	}
	return panicPreparedInvocation{PreparedInvocation: invocation}, nil
}

var _ tools.ToolCallPreparer = (*panicPreparedRegistry)(nil)

func TestRunCommitConsumerWritesSequentialFactsBeforeAcknowledging(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input.txt")
	require.NoError(t, os.WriteFile(path, []byte("durable read"), 0o600))
	sink := &consumerTestSink{}
	model := &scriptedChatModel{seq: []*llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "read-1", Name: "read", Args: map[string]any{"path": path}}}},
		{Role: llm.RoleAssistant, Content: "done"},
	}}
	agent := NewAgentImpl("durable", NewLoopAgent(WithLLM(model), WithTools(newPreparedConsumerReadRegistry(t))))
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "run-consumer-sequential"}, nil)
	owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "session-1", WorkspaceID: "workspace-1"})

	_, runErr := owner.run(time.Second, func(ctx context.Context) (Result, error) {
		return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "read file"})
	})
	require.NoError(t, runErr)
	steps, batches, receipts, terminal := sink.snapshot()
	require.Equal(t, []string{"input", "begin", "execution", "receipt", "resolution", "terminal"}, steps)
	require.Len(t, batches, 1)
	require.Len(t, batches[0].Intents, 1)
	require.Empty(t, batches[0].KnownReceipts)
	require.Len(t, receipts, 1)
	require.Equal(t, uint64(2), receipts[0].TranscriptIndex)
	require.Equal(t, DispatchStateDispatched, receipts[0].DispatchState)
	require.Equal(t, ReceiptOutcomeSuccess, receipts[0].ReceiptOutcome)
	require.NotNil(t, terminal)
	require.Equal(t, "run-consumer-sequential", terminal.RunID)
	require.Equal(t, RunOutcomeSuccess, terminal.Outcome)
	require.Equal(t, 4, len(terminal.Transcript))
	require.Equal(t, 2, model.idx, "the next provider request is allowed only after receipt and resolution sync")
}

func TestRunCommitReceiptPersistenceFailureDoesNotPublishSuccess(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input.txt")
	require.NoError(t, os.WriteFile(path, []byte("durable read"), 0o600))
	sink := &consumerTestSink{recordReceiptErr: errors.New("injected receipt sync failure")}
	model := &scriptedChatModel{seq: []*llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "read-receipt-sync-failure", Name: "read", Args: map[string]any{"path": path}}}},
		{Role: llm.RoleAssistant, Content: "must not be requested"},
	}}
	agent := NewAgentImpl("durable-receipt-sync-failure", NewLoopAgent(WithLLM(model), WithTools(newPreparedConsumerReadRegistry(t))))
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "run-receipt-sync-failure"}, nil)
	owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "session-1", WorkspaceID: "workspace-1"})
	events := NewEventStream(32)

	_, runErr := owner.run(time.Second, func(ctx context.Context) (Result, error) {
		return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "read file"}, events)
	})
	events.Close()
	require.Error(t, runErr)
	require.True(t, hasUnknownToolOutcome(runErr), "a possibly committed side effect without a durable receipt remains unknown")

	var toolResult *AgentEvent
	for event := range events.Events() {
		if event.Kind == "tool_result" && event.ToolCallID == "read-receipt-sync-failure" {
			copy := event
			toolResult = &copy
			break
		}
	}
	require.NotNil(t, toolResult)
	require.True(t, toolResult.IsError)
	require.Equal(t, ReceiptOutcomeUnknown, toolResult.ReceiptOutcome, "the TUI event must not claim success before receipt persistence")

	steps, _, receipts, terminal := sink.snapshot()
	require.Equal(t, []string{"input", "begin", "execution", "receipt", "terminal"}, steps)
	require.Empty(t, receipts, "the injected receipt sync failure means no receipt was durably recorded")
	require.NotNil(t, terminal)
	require.Equal(t, RunOutcomeUnknown, terminal.Outcome)
	var terminalReceipt *AgentMessage
	for index := range terminal.Transcript {
		if terminal.Transcript[index].Message.Role == string(llm.RoleTool) && terminal.Transcript[index].Message.ToolCallID == "read-receipt-sync-failure" {
			copy := terminal.Transcript[index].Message
			terminalReceipt = &copy
			break
		}
	}
	require.NotNil(t, terminalReceipt)
	require.Equal(t, ReceiptOutcomeUnknown, terminalReceipt.ReceiptOutcome, "the failed write must not leak success into the terminal projection")
	require.Equal(t, 1, model.idx, "receipt persistence failure must stop later provider work")
}

func TestRunCommitReceiptOutcomeKeepsUserQuestionDeclineDistinctFromTurnCancellation(t *testing.T) {
	declined := runCommitReceiptOutcome(context.Background(), runCommitDispatchResult{
		err:       fmt.Errorf("ask_user: %w", ErrHITLQuestionCanceled),
		committed: true,
	})
	require.Equal(t, ReceiptOutcomeError, declined)

	committedCancellation := runCommitReceiptOutcome(context.Background(), runCommitDispatchResult{
		err:       context.Canceled,
		committed: true,
	})
	require.Equal(t, ReceiptOutcomeUnknown, committedCancellation, "generic canceled mutating work must remain unknown")

	preDispatchCancellation := runCommitReceiptOutcome(context.Background(), runCommitDispatchResult{
		err:       context.Canceled,
		committed: false,
	})
	require.Equal(t, ReceiptOutcomeInterrupted, preDispatchCancellation)
}

func TestRunCommitDetectorCauseDoesNotMaskReceiptPersistenceUnknown(t *testing.T) {
	detectorParentCtx, cancelDetector := context.WithCancelCause(context.Background())
	defer cancelDetector(nil)
	receiptErr := errors.New("injected receipt sync failure after loop detection")
	sink := &consumerTestSink{recordReceiptErr: receiptErr}
	registry := newPreparedConsumerReadRegistry(t)
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{
		PreparedAttemptObserver: func(context.Context, tools.FrozenPreparedAttempt) error {
			cancelDetector(ErrLoopDetected)
			return ErrLoopDetected
		},
	}))
	model := &scriptedChatModel{seq: []*llm.Message{{
		Role:      llm.RoleAssistant,
		ToolCalls: []llm.ToolCall{{ID: "detector-receipt-failure", Name: "read", Args: map[string]any{"path": filepath.Join(t.TempDir(), "input.txt")}}},
	}}}
	agent := NewAgentImpl("durable-detector-receipt-failure", NewLoopAgent(WithLLM(model), WithTools(registry)))
	owner := newRunOwner(WithRunCommit(detectorParentCtx, sink, RunCommitBinding{
		SessionID: "session-detector-receipt-failure", WorkspaceID: "workspace-detector-receipt-failure",
	}), nil, RunClaim{ID: "run-detector-receipt-failure"}, nil)

	_, runErr := owner.run(time.Second, func(ctx context.Context) (Result, error) {
		return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "prepare the repeated tool attempt"})
	})
	require.ErrorIs(t, runErr, ErrLoopDetected, "the first cause remains visible")
	require.ErrorIs(t, runErr, receiptErr, "receipt failure must not be erased by detector normalization")
	require.True(t, hasUnknownToolOutcome(runErr), "journal failure remains conservatively unknown")
	status := owner.Snapshot()
	require.Equal(t, RunOutcomeUnknown, status.Outcome, "unknown persistence evidence outranks detector error classification")

	steps, _, receipts, terminal := sink.snapshot()
	require.Equal(t, []string{"input", "begin", "receipt", "terminal"}, steps)
	require.Empty(t, receipts, "the injected receipt sync failure leaves no durable receipt")
	require.NotNil(t, terminal)
	require.Equal(t, RunOutcomeUnknown, terminal.Outcome, "durable terminal classification preserves the failed receipt write")
	require.Equal(t, 1, model.idx, "detector rejection and journal failure stop the next provider call")
}

func TestTerminalOutcomeKeepsRunCommitQuiescenceUnknownAheadOfLoopError(t *testing.T) {
	detectorCtx, cancelDetector := context.WithCancelCause(context.Background())
	cancelDetector(ErrLoopDetected)
	defer cancelDetector(nil)
	quiescenceErr := runCommitQuiescenceError{cause: ErrRunCommitQuiescenceUnproven}
	runErr := normalizeLoopDetectedCause(detectorCtx, quiescenceErr)

	require.ErrorIs(t, runErr, ErrLoopDetected, "first cancellation cause remains typed")
	require.ErrorIs(t, runErr, ErrRunCommitQuiescenceUnproven, "R1 local-quiescence failure remains in the error chain")
	require.True(t, hasUnknownToolOutcome(runErr), "an unproven dispatched action remains unknown")
	require.Equal(t, RunOutcomeUnknown, terminalOutcome(detectorCtx, runErr), "unknown R1 evidence outranks detector error")
}

type runContextWaitChatModel struct {
	started chan struct{}
	once    sync.Once
}

func (model *runContextWaitChatModel) Generate(ctx context.Context, _ []llm.Message, _ ...llm.Option) (*llm.Message, error) {
	model.once.Do(func() { close(model.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (model *runContextWaitChatModel) Stream(ctx context.Context, messages []llm.Message, options ...llm.Option) (<-chan llm.MessageChunk, error) {
	_, err := model.Generate(ctx, messages, options...)
	return nil, err
}

func TestRunCommitTerminalOutcomeMatchesOwnerCancellationClassification(t *testing.T) {
	for _, testCase := range []struct {
		name                 string
		parentKind           string
		wantError            error
		wantOutcome          RunOutcome
		providerLocalTimeout bool
	}{
		{
			name:                 "provider-local deadline while parent remains live",
			parentKind:           "live",
			wantError:            context.DeadlineExceeded,
			wantOutcome:          RunOutcomeError,
			providerLocalTimeout: true,
		},
		{name: "parent cancellation", parentKind: "cancel", wantError: context.Canceled, wantOutcome: RunOutcomeCancelled},
		{name: "parent deadline", parentKind: "deadline", wantError: context.DeadlineExceeded, wantOutcome: RunOutcomeCancelled},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var parent context.Context
			var cancel context.CancelFunc
			var model llm.BaseChatModel
			var modelStarted <-chan struct{}
			switch testCase.parentKind {
			case "live":
				parent = context.Background()
				cancel = func() {}
				model = &scriptedChatModel{
					seq:  []*llm.Message{nil},
					errs: []error{fmt.Errorf("provider stream idle timeout: %w", context.DeadlineExceeded)},
				}
			case "cancel":
				parent, cancel = context.WithCancel(context.Background())
				waitingModel := &runContextWaitChatModel{started: make(chan struct{})}
				model, modelStarted = waitingModel, waitingModel.started
			case "deadline":
				parent, cancel = context.WithTimeout(context.Background(), 150*time.Millisecond)
				waitingModel := &runContextWaitChatModel{started: make(chan struct{})}
				model, modelStarted = waitingModel, waitingModel.started
			}
			defer cancel()

			sink := &consumerTestSink{}
			agent := NewAgentImpl("deadline-classification", NewLoopAgent(WithLLM(model), WithTools(newPreparedConsumerReadRegistry(t))))
			owner := newRunOwner(parent, nil, RunClaim{ID: "run-deadline-classification"}, nil)
			owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "session-timeout", WorkspaceID: "workspace-timeout"})

			runDone := make(chan error, 1)
			go func() {
				_, runErr := owner.run(time.Second, func(ctx context.Context) (Result, error) {
					return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "request a response"})
				})
				runDone <- runErr
			}()
			if modelStarted != nil {
				select {
				case <-modelStarted:
				case <-time.After(time.Second):
					t.Fatal("model did not begin before the parent cancellation test")
				}
				if testCase.parentKind == "cancel" {
					cancel()
				}
			}

			var runErr error
			select {
			case runErr = <-runDone:
			case <-time.After(2 * time.Second):
				t.Fatal("run did not complete after its local or parent deadline")
			}
			require.ErrorIs(t, runErr, testCase.wantError)
			assert.Equal(t, testCase.wantOutcome, owner.Snapshot().Outcome, "the owner outcome follows the parent run context")

			_, _, _, terminal := sink.snapshot()
			require.NotNil(t, terminal, "the durable terminal checkpoint must be acknowledged")
			assert.Equal(t, testCase.wantOutcome, terminal.Outcome, "the durable terminal outcome must match the owner")
			if testCase.providerLocalTimeout {
				assert.Nil(t, parent.Err(), "the provider-local timeout must leave the parent run live")
			}
		})
	}
}

func TestRunCommitConsumerBuffersParallelReceiptsInCanonicalOrder(t *testing.T) {
	root := t.TempDir()
	firstPath := filepath.Join(root, "first.txt")
	secondPath := filepath.Join(root, "second.txt")
	require.NoError(t, os.WriteFile(firstPath, []byte("first"), 0o600))
	require.NoError(t, os.WriteFile(secondPath, []byte("second"), 0o600))
	sink := &consumerTestSink{}
	model := &scriptedChatModel{seq: []*llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "read-1", Name: "read", Args: map[string]any{"path": firstPath}},
			{ID: "read-2", Name: "read", Args: map[string]any{"path": secondPath}},
		}},
		{Role: llm.RoleAssistant, Content: "done"},
	}}
	agent := NewAgentImpl("durable-parallel", NewLoopAgent(
		WithLLM(model),
		WithTools(newPreparedConsumerReadRegistry(t)),
		WithExecutionMode(ExecutionModeParallel),
	))
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "run-consumer-parallel"}, nil)
	owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "session-1", WorkspaceID: "workspace-1"})

	_, runErr := owner.run(time.Second, func(ctx context.Context) (Result, error) {
		return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "read both"})
	})
	require.NoError(t, runErr)
	steps, batches, receipts, terminal := sink.snapshot()
	require.Equal(t, []string{"input", "begin", "execution", "receipt", "execution", "receipt", "resolution", "resolution", "terminal"}, steps)
	require.Len(t, batches, 1)
	require.Len(t, batches[0].Intents, 2)
	require.Equal(t, []string{"read-1", "read-2"}, []string{batches[0].Intents[0].ToolCallID, batches[0].Intents[1].ToolCallID})
	require.Len(t, receipts, 2)
	require.Equal(t, []string{"read-1", "read-2"}, []string{receipts[0].ToolCallID, receipts[1].ToolCallID})
	require.Equal(t, []uint64{2, 3}, []uint64{receipts[0].TranscriptIndex, receipts[1].TranscriptIndex})
	require.NotNil(t, terminal)
	require.Equal(t, RunOutcomeSuccess, terminal.Outcome)
	require.Equal(t, 2, model.idx, "the next provider request follows the canonical receipt and resolution sequence")
}

func TestRunCommitConsumerRejectsPreparedActionWithoutCompletionSupport(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "match.txt"), []byte("needle\n"), 0o600))
	sink := &consumerTestSink{}
	model := &scriptedChatModel{seq: []*llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: "grep-1", Name: "grep", Args: map[string]any{"pattern": "needle", "path": root},
		}}},
		{Role: llm.RoleAssistant, Content: "completed after unsupported tool rejection"},
	}}
	agent := NewAgentImpl("durable-proofless", NewLoopAgent(
		WithLLM(model),
		WithTools(newPreparedConsumerOrdinaryToolRegistry(t, tools.NewGrepTool())),
	))
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "run-consumer-proofless"}, nil)
	owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "session-1", WorkspaceID: "workspace-1"})

	_, runErr := owner.run(time.Second, func(ctx context.Context) (Result, error) {
		return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "find needle"})
	})
	require.NoError(t, runErr)
	steps, batches, receipts, terminal := sink.snapshot()
	require.Equal(t, []string{"input", "begin", "receipt", "terminal"}, steps)
	require.Len(t, batches, 1)
	require.Empty(t, batches[0].Intents, "an unsupported action must not reserve dispatch authority")
	require.Len(t, receipts, 1)
	require.Equal(t, ReceiptOutcomeError, receipts[0].ReceiptOutcome)
	require.Equal(t, DispatchStateNotDispatched, receipts[0].DispatchState)
	require.Empty(t, receipts[0].ActionID)
	require.Contains(t, receipts[0].Message.Content, ErrRunCommitCompletionUnsupported.Error())
	require.Empty(t, sink.executionSnapshot())
	require.NotNil(t, terminal)
	require.Equal(t, RunOutcomeSuccess, terminal.Outcome)
	require.Equal(t, 2, model.idx, "the known pre-dispatch rejection permits a later provider response")
}

func TestRunCommitConsumerResolvesSearchAfterCompletionProof(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		toolName   string
		helperMode bool
		unscoped   bool
	}{
		{name: "scoped grep pure-Go fallback", toolName: "grep"},
		{name: "scoped grep managed helper", toolName: "grep", helperMode: true},
		{name: "scoped find pure-Go fallback", toolName: "find"},
		{name: "scoped find managed helper", toolName: "find", helperMode: true},
		{name: "default unscoped grep pure-Go fallback", toolName: "grep", unscoped: true},
		{name: "default unscoped grep managed helper", toolName: "grep", helperMode: true, unscoped: true},
		{name: "default unscoped find pure-Go fallback", toolName: "find", unscoped: true},
		{name: "default unscoped find managed helper", toolName: "find", helperMode: true, unscoped: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			var args map[string]any
			if testCase.toolName == "grep" {
				require.NoError(t, os.WriteFile(filepath.Join(root, "match.txt"), []byte("needle\n"), 0o600))
				args = map[string]any{"pattern": "needle", "path": root}
			} else {
				require.NoError(t, os.WriteFile(filepath.Join(root, "target.go"), []byte("package main\n"), 0o600))
				args = map[string]any{"pattern": "target.go", "type": "f", "path": root}
			}

			if testCase.helperMode {
				helperDir := t.TempDir()
				name, script := runCommitSearchHelper(t, testCase.toolName)
				require.NoError(t, os.WriteFile(filepath.Join(helperDir, name), []byte(script), 0o700))
				t.Setenv("PATH", helperDir)
			} else {
				t.Setenv("PATH", t.TempDir())
			}

			sink := &consumerTestSink{}
			model := &scriptedChatModel{seq: []*llm.Message{
				{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "search-1", Name: testCase.toolName, Args: args}}},
				{Role: llm.RoleAssistant, Content: "search completed"},
			}}
			registry := newPreparedConsumerScopedSearchRegistry(t, root, testCase.toolName)
			if testCase.unscoped {
				registry = newPreparedConsumerDefaultSearchRegistry(t, testCase.toolName)
			}
			agent := NewAgentImpl("durable-scoped-search", NewLoopAgent(
				WithLLM(model),
				WithTools(registry),
			))
			owner := newRunOwner(context.Background(), nil, RunClaim{ID: "run-consumer-scoped-search"}, nil)
			owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "session-1", WorkspaceID: "workspace-1"})

			_, runErr := owner.run(time.Second, func(ctx context.Context) (Result, error) {
				return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "search the workspace"})
			})
			require.NoError(t, runErr)
			require.Equal(t, 2, model.idx, "the follow-up model call must wait for durable proof and resolution")
			steps, batches, receipts, terminal := sink.snapshot()
			require.Equal(t, []string{"input", "begin", "execution", "receipt", "resolution", "terminal"}, steps)
			require.Len(t, batches, 1)
			require.Len(t, receipts, 1)
			require.NotNil(t, terminal)
			require.Equal(t, RunOutcomeSuccess, terminal.Outcome)

			executions := sink.executionSnapshot()
			require.Len(t, executions, 1)
			require.Equal(t, RunExecutionQuiescent, executions[0].ResourceState)
			if testCase.helperMode {
				require.Equal(t, tools.CompletionOwnedProcessGroupJoined, executions[0].CompletionProof)
			} else {
				require.Equal(t, tools.CompletionInlinePipelineJoined, executions[0].CompletionProof)
			}
			resolutions := sink.resolutionSnapshot()
			require.Len(t, resolutions, 1)
			require.Equal(t, ResolutionLocalResourcesStopped, resolutions[0].Evidence)
		})
	}
}

func runCommitSearchHelper(t *testing.T, toolName string) (string, string) {
	t.Helper()
	switch toolName {
	case "grep":
		return "rg", "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"match\",\"data\":{\"path\":{\"text\":\"match.txt\"},\"lines\":{\"text\":\"needle\\n\"},\"line_number\":1}}'\n"
	case "find":
		return "fd", "#!/bin/sh\nprintf 'target.go\\000'\n"
	default:
		t.Fatalf("unsupported search helper test fixture %q", toolName)
		return "", ""
	}
}

func TestRunCommitConsumerFailsClosedBeforeInitialOverBudgetProvider(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input.txt")
	require.NoError(t, os.WriteFile(path, []byte("durable read"), 0o600))
	sink := &consumerTestSink{}
	model := &scriptedChatModel{seq: []*llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "read-1", Name: "read", Args: map[string]any{"path": path}}}},
		{Role: llm.RoleAssistant, Content: "must not be requested"},
	}}
	loop := NewLoopAgent(WithLLM(model), WithTools(newPreparedConsumerReadRegistry(t)))
	compactor := &runCommitCountingCompactor{}
	loop.SetMidTurnCompaction(compaction.NewMidTurnCompact(compaction.WithThresholdRatio(0.5)), compactor, compaction.NewHeuristicTokenEstimator(), 1)
	agent := NewAgentImpl("durable-compaction", loop)
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "run-consumer-compaction"}, nil)
	owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "session-1", WorkspaceID: "workspace-1"})

	_, runErr := owner.run(time.Second, func(ctx context.Context) (Result, error) {
		return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "read file"})
	})
	require.ErrorIs(t, runErr, ErrRunCommitCompactionProjectionUnavailable)
	require.Zero(t, compactor.calls, "durable projection is unavailable before the real compactor or summarizer runs")
	steps, _, receipts, terminal := sink.snapshot()
	require.Equal(t, []string{"input", "terminal"}, steps)
	require.Empty(t, receipts)
	require.NotNil(t, terminal)
	require.Equal(t, RunOutcomeError, terminal.Outcome)
	require.Equal(t, 0, model.idx, "an over-budget first request is rejected before any provider call when the sink cannot prove its source view")
}

func TestRunCommitConsumerUsesOneBoundedCleanupContextAfterCancellation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input.txt")
	require.NoError(t, os.WriteFile(path, []byte("durable read"), 0o600))
	sink := &consumerTestSink{}
	parentCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink.cancelAfterBegin = cancel
	model := &scriptedChatModel{seq: []*llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "read-1", Name: "read", Args: map[string]any{"path": path}},
			{ID: "read-2", Name: "read", Args: map[string]any{"path": path}},
		}},
		{Role: llm.RoleAssistant, Content: "must not be requested"},
	}}
	agent := NewAgentImpl("durable-cancel", NewLoopAgent(WithLLM(model), WithTools(newPreparedConsumerReadRegistry(t))))
	owner := newRunOwner(parentCtx, nil, RunClaim{ID: "run-consumer-cancel"}, nil)
	owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "session-1", WorkspaceID: "workspace-1"})

	_, runErr := owner.run(time.Second, func(ctx context.Context) (Result, error) {
		return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "read file"})
	})
	require.ErrorIs(t, runErr, context.Canceled)
	steps, _, receipts, terminal := sink.snapshot()
	require.Equal(t, []string{"input", "begin", "execution", "receipt", "resolution", "begin", "receipt", "terminal"}, steps)
	require.Len(t, receipts, 2)
	require.Equal(t, []string{ReceiptOutcomeInterrupted, ReceiptOutcomeInterrupted}, []string{receipts[0].ReceiptOutcome, receipts[1].ReceiptOutcome})
	require.NotNil(t, terminal)
	require.Equal(t, RunOutcomeCancelled, terminal.Outcome)
	require.Equal(t, 1, model.idx, "cancellation prevents a follow-up provider request")

	contexts := sink.contextSnapshot()
	require.Len(t, contexts, len(steps))
	for _, commitCtx := range contexts {
		require.True(t, commitCtx.bound, "cleanup bookkeeping retains the original run binding")
		require.Equal(t, "run-consumer-cancel", commitCtx.runID)
	}
	cleanupContexts := contexts[2:]
	require.NotEmpty(t, cleanupContexts)
	deadline := cleanupContexts[0].deadline
	require.True(t, cleanupContexts[0].hasDeadline)
	for _, commitCtx := range cleanupContexts {
		require.True(t, commitCtx.hasDeadline)
		require.Equal(t, deadline, commitCtx.deadline, "cancellation bookkeeping shares one absolute cleanup deadline")
	}
}

func TestRunCommitConsumerRejectsDirectRealChildBeforeProvider(t *testing.T) {
	sink := &consumerTestSink{}
	model := &scriptedChatModel{seq: []*llm.Message{{Role: llm.RoleAssistant, Content: "must not be requested"}}}
	runner := &realSubAgentRunner{model: model}
	ctx := WithRunCommit(context.Background(), sink, RunCommitBinding{
		SessionID:       "parent-session",
		WorkspaceID:     "workspace-1",
		OwnerGeneration: "parent-owner",
	})

	_, runErr := runner.Run(ctx, "same text as parent is irrelevant", nil, func(AgentEvent) {})
	require.ErrorIs(t, runErr, ErrRunCommitChildUnsupported)
	require.Zero(t, model.idx, "a child without an independent physical-session factory fails before provider work")
	steps, _, _, _ := sink.snapshot()
	require.Empty(t, steps, "the child must not reuse or mutate the parent's durable sink")
}

func TestRunCommitConsumerTreatsDispatchPanicAsPossiblyDispatched(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input.txt")
	require.NoError(t, os.WriteFile(path, []byte("durable read"), 0o600))
	sink := &consumerTestSink{}
	base := newPreparedConsumerReadRegistry(t)
	panicWrapper := tools.ToolExecutorWrapper(func(next func(context.Context, tools.ToolCall) (*tools.ToolResult, error)) func(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
		return func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
			_, _ = next(ctx, call)
			panic("prepared dispatch panic")
		}
	})
	require.NoError(t, base.SetPreparedToolStages(tools.PreparedToolStages{ExecutionWrappers: []tools.ToolExecutorWrapper{panicWrapper}, SynchronousExecutionWrappers: true}))
	registry := base
	model := &scriptedChatModel{seq: []*llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "read-panic", Name: "read", Args: map[string]any{"path": path}}}},
		{Role: llm.RoleAssistant, Content: "must not be requested"},
	}}
	agent := NewAgentImpl("durable-panic", NewLoopAgent(WithLLM(model), WithTools(registry)))
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "run-consumer-panic"}, nil)
	owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "session-1", WorkspaceID: "workspace-1"})

	_, runErr := owner.run(time.Second, func(ctx context.Context) (Result, error) {
		return agent.Run(ctx, Submission{Type: SubmissionUserMessage, Content: "read file"})
	})
	require.Error(t, runErr)
	require.True(t, hasUnknownToolOutcome(runErr))
	require.Equal(t, 1, model.idx, "a dispatch panic cannot authorize a later provider request")
	steps, _, receipts, terminal := sink.snapshot()
	require.Equal(t, []string{"input", "begin", "execution", "receipt", "terminal"}, steps)
	require.Len(t, receipts, 1)
	require.Equal(t, ReceiptOutcomeUnknown, receipts[0].ReceiptOutcome)
	executions := sink.executionSnapshot()
	require.Len(t, executions, 1)
	require.Equal(t, DispatchStateDispatched, executions[0].DispatchState, "only a normally returned committed=false result proves pre-dispatch rejection")
	require.Equal(t, RunExecutionCleanupPending, executions[0].ResourceState)
	require.Empty(t, sink.resolutionSnapshot(), "panic is not a not-dispatched proof or a local completion proof")
	require.NotNil(t, terminal)
	require.Equal(t, RunOutcomeUnknown, terminal.Outcome)
}

func TestRunCommitConsumerParallelCancellationDoesNotReadOrCopyLiveWorkerResult(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input.txt")
	require.NoError(t, os.WriteFile(path, []byte("durable read"), 0o600))
	parentCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dispatchStarted := make(chan struct{})
	releaseDispatch := make(chan struct{})
	commitRead := make(chan struct{})
	allowCommitRead := make(chan struct{})
	var releaseDispatchOnce, allowCommitOnce sync.Once
	defer func() { releaseDispatchOnce.Do(func() { close(releaseDispatch) }) }()
	defer func() { allowCommitOnce.Do(func() { close(allowCommitRead) }) }()
	lastKnownID := "known-127"
	sink := &consumerTestSink{}
	sink.afterBegin = func() {
		go func() {
			<-dispatchStarted
			cancel()
		}()
	}
	sink.recordReceiptHook = func(receipt RunToolReceipt) {
		if receipt.ToolCallID != lastKnownID {
			return
		}
		releaseDispatchOnce.Do(func() { close(releaseDispatch) })
		<-commitRead
		allowCommitOnce.Do(func() { close(allowCommitRead) })
	}
	base := newPreparedConsumerReadRegistry(t)
	gateWrapper := tools.ToolExecutorWrapper(func(next func(context.Context, tools.ToolCall) (*tools.ToolResult, error)) func(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
		return func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
			result, err := next(ctx, call)
			close(dispatchStarted)
			<-releaseDispatch // Keep the actual sealed pipeline running after cancellation.
			return result, err
		}
	})
	require.NoError(t, base.SetPreparedToolStages(tools.PreparedToolStages{ExecutionWrappers: []tools.ToolExecutorWrapper{gateWrapper}, SynchronousExecutionWrappers: true}))
	require.NoError(t, base.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations, TerminalObserver: func(tools.PreparedTerminalOutcome) error {
			close(commitRead)
			<-allowCommitRead // Release the worker's result assignment after the cancellation snapshot.
			return nil
		}}, nil
	}))
	registry := base
	calls := make([]llm.ToolCall, 128)
	calls[0] = llm.ToolCall{ID: "read-gated", Name: "read", Args: map[string]any{"path": path}}
	canceled := make(map[string]string, len(calls)-1)
	for index := 1; index < len(calls); index++ {
		calls[index] = llm.ToolCall{ID: fmt.Sprintf("known-%03d", index), Name: "read"}
		canceled[calls[index].ID] = toolInterceptorCanceledOutput
	}
	canceled[lastKnownID] = "safe plan-mode denial"
	owner := newRunOwner(parentCtx, nil, RunClaim{ID: "run-consumer-parallel-cancel"}, nil)
	owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "session-1", WorkspaceID: "workspace-1"})
	assistant := AgentMessage{Role: string(llm.RoleAssistant), ToolCalls: calls}

	_, runErr := owner.run(time.Second, func(ctx context.Context) (Result, error) {
		consumer, err := newRunCommitConsumer(ctx)
		if err != nil {
			return Result{}, err
		}
		user := AgentMessage{Role: string(llm.RoleUser), Content: "cancel gated parallel dispatch"}
		if err := consumer.commitInput(0, user); err != nil {
			return Result{}, err
		}
		transcript := []TranscriptEntry{{Message: user}, {Message: assistant}}
		messages := []llm.Message{{Role: llm.RoleUser, Content: user.Content}, {Role: llm.RoleAssistant, ToolCalls: calls}}
		groupErr := consumer.executeParallelGroup(ctx, registry, 1, assistant, calls, canceled, nil, &transcript, &messages)
		terminalErr := consumer.commitTerminal(transcript, nil, RunOutcomeUnknown)
		return Result{Outcome: RunOutcomeUnknown}, errors.Join(groupErr, terminalErr)
	})
	require.Error(t, runErr)
	require.True(t, hasUnknownToolOutcome(runErr))
	steps, _, receipts, terminal := sink.snapshot()
	require.Len(t, receipts, len(calls))
	require.Equal(t, []string{"input", "begin"}, steps[:2])
	require.Equal(t, "read-gated", receipts[0].ToolCallID)
	require.Equal(t, ReceiptOutcomeUnknown, receipts[0].ReceiptOutcome)
	require.Equal(t, lastKnownID, receipts[len(receipts)-1].ToolCallID)
	require.Equal(t, "safe plan-mode denial", receipts[len(receipts)-1].Message.Content)
	require.NotNil(t, terminal)
	require.Equal(t, RunOutcomeUnknown, terminal.Outcome)
}

func TestDispatchPendingExecutionErrorDoesNotDoubleTrackPreparedSignal(t *testing.T) {
	for _, testCase := range []struct {
		name                   string
		completionSignal       bool
		pendingDoneDistinct    bool
		pendingDoneNil         bool
		registerPreparedTask   bool
		wantPendingAttachments int
		wantPendingResources   int
		wantHandoffs           int
	}{
		{name: "same prepared signal", completionSignal: true, registerPreparedTask: true, wantPendingAttachments: 1, wantHandoffs: 1},
		{name: "pending error with nil resource channel", completionSignal: false, pendingDoneNil: true, registerPreparedTask: true, wantPendingAttachments: 1, wantHandoffs: 1},
		{name: "wrapped distinct resource channel", completionSignal: true, pendingDoneDistinct: true, registerPreparedTask: true, wantPendingAttachments: 1, wantHandoffs: 1},
		{name: "legacy caller without prepared task", wantPendingResources: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			base, closeQueue := prepareCompletionTestWrite(context.Background(), t)
			defer closeQueue()
			tracker := &runCommitPendingTestTracker{}
			ctx := context.WithValue(context.Background(), runOwnerContextKey{}, tracker)
			signal := base.CompletionSignal()
			if !testCase.completionSignal {
				empty := tools.PreparedCompletionSignal{}
				signal = empty
			}
			call := runCommitPendingPreparedInvocation{PreparedInvocation: base}
			if !testCase.completionSignal {
				empty := tools.PreparedCompletionSignal{}
				call.completionSignal = &empty
			}
			if testCase.pendingDoneDistinct || testCase.pendingDoneNil {
				call.overridePendingDone = true
				if testCase.pendingDoneDistinct {
					call.pendingDoneOverride = make(chan struct{})
				}
			}
			var taskID uint64
			if testCase.registerPreparedTask {
				taskID = registerRunCompletionTask(ctx, ActionIntent{}, signal)
			}
			dispatch := dispatchPreparedRunCommitCall(ctx, call, nil, taskID)

			require.True(t, dispatch.returned)
			require.True(t, dispatch.committed)
			require.Error(t, dispatch.err)
			require.Equal(t, testCase.wantHandoffs, tracker.handoffs, "prepared placeholder is handed off once after Dispatch returns")
			require.Equal(t, testCase.wantPendingAttachments, tracker.pendingAttachments)
			if testCase.pendingDoneNil {
				require.Len(t, tracker.attachedDone, 1)
				require.Nil(t, tracker.attachedDone[0], "nil PEE handle is attached as unproven state, not replaced with a fake channel")
			}
			require.Equal(t, testCase.wantPendingResources, tracker.pendingResources, "only callers without a prepared task use the legacy resource hook")
		})
	}
}

func TestSourceTruncatedFromMetadataRequiresExplicitBoolean(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]any
		want     *bool
	}{
		{name: "true", metadata: map[string]any{"truncated": true}, want: boolPointer(true)},
		{name: "false", metadata: map[string]any{"truncated": false}, want: boolPointer(false)},
		{name: "missing", metadata: map[string]any{"other": true}},
		{name: "non-boolean", metadata: map[string]any{"truncated": "true"}},
		{name: "nil", metadata: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := sourceTruncatedFromMetadata(test.metadata)
			if test.want == nil {
				require.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			require.Equal(t, *test.want, *got)
			*test.want = !*test.want
			require.Equal(t, !*test.want, *got, "the returned optional value must not alias its caller")
		})
	}
}

func boolPointer(value bool) *bool { return &value }
