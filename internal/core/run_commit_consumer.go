package core

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

var (
	ErrRunCommitOwnerUnavailable                = errors.New("core: durable run requires its run owner")
	ErrRunCommitPreparedInvocationUnavailable   = errors.New("core: durable run requires prepared tool dispatch")
	ErrRunCommitChildUnsupported                = errors.New("core: durable child run requires an independent physical session")
	ErrRunCommitStreamingUnsupported            = errors.New("core: durable streaming dispatch requires an attested synchronous stream sink")
	ErrRunCommitQuiescenceUnproven              = errors.New("core: tool completion did not prove local resource quiescence")
	ErrRunCommitCompactionProjectionUnavailable = errors.New("core: durable compaction requires a typed history projection")
)

type runCommitLoopScopeKey struct{}

type runCommitPersistenceError struct {
	operation string
	cause     error
}

func (err runCommitPersistenceError) Error() string {
	return fmt.Sprintf("core: run commit %s failed: %v", err.operation, err.cause)
}

func (err runCommitPersistenceError) Unwrap() error    { return err.cause }
func (runCommitPersistenceError) OutcomeUnknown() bool { return true }

type runCommitConsumer struct {
	ctx        context.Context
	owner      *runOwner
	sink       RunCommitSink
	binding    RunCommitBinding
	runID      string
	terminalID TerminalCheckpointID
}

type runCommitDispatchResult struct {
	output    string
	isError   bool
	err       error
	returned  bool
	committed bool
}

type runCommitToolResult struct {
	output  string
	isError bool
	outcome string
}

type runCommitQuiescenceError struct{ cause error }

func (err runCommitQuiescenceError) Error() string {
	if err.cause == nil {
		return ErrRunCommitQuiescenceUnproven.Error()
	}
	return fmt.Sprintf("%s: %v", ErrRunCommitQuiescenceUnproven, err.cause)
}

func (err runCommitQuiescenceError) Unwrap() error {
	return errors.Join(ErrRunCommitQuiescenceUnproven, err.cause)
}
func (runCommitQuiescenceError) OutcomeUnknown() bool { return true }

func dispatchPreparedRunCommitCall(ctx context.Context, invocation tools.PreparedInvocation, taskID uint64) (result runCommitDispatchResult) {
	if invocation == nil {
		return runCommitDispatchResult{output: "Error: prepared invocation is unavailable", isError: true, err: ErrRunCommitPreparedInvocationUnavailable}
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			_ = recovered
			result.output = "Error: tool execution panicked after dispatch"
			result.isError = true
			result.err = toolDispatchPanicError{}
			// Dispatch did not return, so DispatchCommitted=false cannot prove
			// that the action was never dispatched. Keep the durable marker in
			// the dispatched/unknown state until owner cleanup observes proof.
			result.committed = true
			markRunOutcomeUnknown(ctx)
		}
		handoffRunCompletionTask(ctx, taskID)
	}()

	toolResult, err := invocation.Dispatch(ctx)
	result.returned = true
	result.committed = invocation.DispatchCommitted()
	if err != nil {
		result.output = "Error: " + err.Error()
		result.isError = true
		result.err = err
		var pending tools.PendingExecutionError
		if errors.As(err, &pending) {
			registerRunPendingResource(ctx, invocation.NormalizedCall().ID, invocation.NormalizedCall().Name, pending.ExecutionDone())
			if pending.OutcomeUnknown() {
				markRunOutcomeUnknown(ctx)
			}
		}
		var unknown runOutcomeUnknown
		if errors.As(err, &unknown) && unknown.OutcomeUnknown() {
			markRunOutcomeUnknown(ctx)
		}
		return result
	}
	if toolResult == nil {
		result.output = "Error: " + errToolMissingReceipt.Error()
		result.isError = true
		result.err = errToolMissingReceipt
		return result
	}
	result.output = toolResult.Output
	result.isError = toolResult.IsError
	return result
}

func newRunCommitConsumer(ctx context.Context) (*runCommitConsumer, error) {
	sink, binding, ok := RunCommitFromContext(ctx)
	if !ok {
		return nil, nil
	}
	if active, _ := ctx.Value(runCommitLoopScopeKey{}).(bool); active {
		return nil, ErrRunCommitChildUnsupported
	}
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	runID := RunIDFromContext(ctx)
	if runID == "" {
		return nil, fmt.Errorf("%w: run_id is required", ErrInvalidRunCommitBinding)
	}
	owner, ok := ctx.Value(runOwnerContextKey{}).(*runOwner)
	if !ok || owner == nil {
		return nil, ErrRunCommitOwnerUnavailable
	}
	owner.mu.Lock()
	ownerRunID, ownerGeneration := owner.claim.ID, owner.ownerGeneration
	owner.mu.Unlock()
	if ownerRunID != runID || ownerGeneration != binding.OwnerGeneration {
		return nil, fmt.Errorf("%w: owner run identity does not match durable binding", ErrInvalidRunCommitBinding)
	}
	return &runCommitConsumer{
		ctx:        ctx,
		owner:      owner,
		sink:       sink,
		binding:    binding,
		runID:      runID,
		terminalID: TerminalCheckpointID("terminal-" + newClaimID()),
	}, nil
}

func withRunCommitLoopScope(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, runCommitLoopScopeKey{}, true)
}

func (consumer *runCommitConsumer) commitContext() context.Context {
	if consumer == nil {
		return nil
	}
	return consumer.owner.CleanupCommitContext(consumer.ctx)
}

func (consumer *runCommitConsumer) commitInput(index uint64, message AgentMessage) error {
	if consumer == nil {
		return nil
	}
	err := consumer.sink.CommitInput(consumer.commitContext(), RunCommitInput{
		RunID:           consumer.runID,
		TranscriptIndex: index,
		Message:         message,
		InputKind:       message.InputKind,
	})
	return persistenceError("input", err)
}

func (consumer *runCommitConsumer) beginIntentBatch(batch RunIntentBatch) error {
	if consumer == nil {
		return nil
	}
	batch.RunID = consumer.runID
	if err := consumer.sink.BeginIntentBatch(consumer.commitContext(), batch); err != nil {
		return runCommitPersistenceError{operation: "intent batch", cause: err}
	}
	return nil
}

func (consumer *runCommitConsumer) recordExecution(execution ActionExecution) error {
	if consumer == nil {
		return nil
	}
	if err := consumer.sink.RecordExecution(consumer.commitContext(), execution); err != nil {
		return runCommitPersistenceError{operation: "execution", cause: err}
	}
	return nil
}

func (consumer *runCommitConsumer) recordTaskExecution(taskID uint64, execution ActionExecution) error {
	if err := consumer.recordExecution(execution); err != nil {
		return err
	}
	if taskID > 0 && execution.CompletionProof != "" {
		markRunCompletionProofExecutionDurable(consumer.ctx, taskID)
	}
	return nil
}

func (consumer *runCommitConsumer) recordReceipt(receipt RunToolReceipt) error {
	if consumer == nil {
		return nil
	}
	if err := consumer.sink.RecordReceipt(consumer.commitContext(), receipt); err != nil {
		return runCommitPersistenceError{operation: "receipt", cause: err}
	}
	return nil
}

func (consumer *runCommitConsumer) recordTaskReceipt(taskID uint64, receipt RunToolReceipt) error {
	if err := consumer.recordReceipt(receipt); err != nil {
		return err
	}
	if taskID > 0 {
		markRunCompletionReceiptDurable(consumer.ctx, taskID)
	}
	return nil
}

func (consumer *runCommitConsumer) resolveExecution(resolution ExecutionResolution) error {
	if consumer == nil {
		return nil
	}
	if err := consumer.sink.ResolveExecution(consumer.commitContext(), resolution); err != nil {
		return runCommitPersistenceError{operation: "execution resolution", cause: err}
	}
	return nil
}

func (consumer *runCommitConsumer) resolveTask(taskID uint64, resolution ExecutionResolution) error {
	if err := consumer.resolveExecution(resolution); err != nil {
		return err
	}
	if taskID > 0 {
		markRunCompletionResolved(consumer.ctx, taskID)
	}
	return nil
}

func (consumer *runCommitConsumer) commitTerminal(transcript []TranscriptEntry, projections []CompactionProjection, outcome RunOutcome) error {
	if consumer == nil {
		return nil
	}
	terminal := RunTerminal{
		RunID:                consumer.runID,
		Transcript:           cloneTranscriptEntries(transcript),
		Projections:          append([]CompactionProjection(nil), projections...),
		Outcome:              outcome,
		TerminalCheckpointID: consumer.terminalID,
	}
	if err := consumer.sink.CommitTerminal(consumer.commitContext(), terminal); err != nil {
		return runCommitPersistenceError{operation: "terminal", cause: err}
	}
	markRunCommitTerminalSynced(consumer.ctx)
	return nil
}

func (consumer *runCommitConsumer) actionIntent(batchID string, ordinal uint64, call llm.ToolCall, invocation tools.PreparedInvocation) ActionIntent {
	return ActionIntent{
		SessionID:                  consumer.binding.SessionID,
		WorkspaceID:                consumer.binding.WorkspaceID,
		RunID:                      consumer.runID,
		ActionBatchID:              batchID,
		OwnerGeneration:            consumer.binding.OwnerGeneration,
		AssistantTranscriptOrdinal: ordinal,
		ToolCallID:                 call.ID,
		ActionID:                   "action-" + newClaimID(),
		ToolIdentity:               invocation.ToolIdentity(),
		SchemaDigest:               invocation.SchemaDigest(),
		ArgumentDigest:             invocation.ArgumentDigest(),
		EffectSet:                  invocation.EffectSet(),
	}
}

func (consumer *runCommitConsumer) knownReceipt(ordinal uint64, call llm.ToolCall, index uint64, output, outcome string) RunToolReceipt {
	return RunToolReceipt{
		RunID:                      consumer.runID,
		AssistantTranscriptOrdinal: ordinal,
		ToolCallID:                 call.ID,
		TranscriptIndex:            index,
		DispatchState:              DispatchStateNotDispatched,
		Message: AgentMessage{
			Role:           string(llm.RoleTool),
			Content:        output,
			ToolCallID:     call.ID,
			ToolName:       call.Name,
			ReceiptOutcome: outcome,
		},
		ReceiptOutcome: outcome,
	}
}

func (consumer *runCommitConsumer) recordTranscriptReceipt(ordinal uint64, intent *ActionIntent, index uint64, entry TranscriptEntry) error {
	if consumer == nil {
		return nil
	}
	receipt := RunToolReceipt{
		RunID:                      consumer.runID,
		AssistantTranscriptOrdinal: ordinal,
		ToolCallID:                 entry.Message.ToolCallID,
		TranscriptIndex:            index,
		DispatchState:              DispatchStateNotDispatched,
		Message:                    entry.Message,
		ReceiptOutcome:             entry.Message.ReceiptOutcome,
	}
	if intent != nil {
		receipt.ActionID = intent.ActionID
	}
	return consumer.recordReceipt(receipt)
}

func (consumer *runCommitConsumer) recordActionReceipt(taskID uint64, ordinal uint64, intent ActionIntent, dispatchState DispatchState, index uint64, entry TranscriptEntry) error {
	receipt := RunToolReceipt{
		RunID:                      consumer.runID,
		AssistantTranscriptOrdinal: ordinal,
		ToolCallID:                 entry.Message.ToolCallID,
		ActionID:                   intent.ActionID,
		TranscriptIndex:            index,
		DispatchState:              dispatchState,
		Message:                    entry.Message,
		ReceiptOutcome:             entry.Message.ReceiptOutcome,
	}
	return consumer.recordTaskReceipt(taskID, receipt)
}

func (consumer *runCommitConsumer) prepareInvocation(ctx context.Context, registry tools.ToolRegistry, call llm.ToolCall) (tools.PreparedInvocation, error) {
	if call.Name == "dispatch_subagent" {
		return nil, ErrRunCommitChildUnsupported
	}
	preparer, ok := registry.(tools.ToolCallPreparer)
	if !ok {
		return nil, ErrRunCommitPreparedInvocationUnavailable
	}
	invocation, err := preparer.PrepareToolCall(ctx, toToolsCall(call))
	if err != nil {
		return nil, err
	}
	if invocation == nil {
		return nil, ErrRunCommitPreparedInvocationUnavailable
	}
	return invocation, nil
}

func (consumer *runCommitConsumer) executeSequentialCall(
	ctx context.Context,
	registry tools.ToolRegistry,
	ordinal uint64,
	assistant AgentMessage,
	call llm.ToolCall,
	stream EventStream,
	transcript *[]TranscriptEntry,
	messages *[]llm.Message,
) (runCommitToolResult, error) {
	invocation, prepareErr := consumer.prepareInvocation(ctx, registry, call)
	if prepareErr != nil {
		output := "Error: " + prepareErr.Error()
		outcome := ReceiptOutcomeError
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			outcome = ReceiptOutcomeInterrupted
		}
		if err := consumer.commitKnownCall(ordinal, assistant, "batch-"+newClaimID(), call, uint64(len(*transcript)), output, outcome, transcript, messages); err != nil {
			return runCommitToolResult{output: output, isError: true, outcome: outcome}, err
		}
		if errors.Is(prepareErr, ErrRunCommitPreparedInvocationUnavailable) || errors.Is(prepareErr, ErrRunCommitChildUnsupported) {
			return runCommitToolResult{output: output, isError: true, outcome: outcome}, prepareErr
		}
		return runCommitToolResult{output: output, isError: true, outcome: outcome}, nil
	}
	if _, streaming := invocation.(tools.PreparedStreamingInvocation); streaming && stream != nil {
		output := "Error: " + ErrRunCommitStreamingUnsupported.Error()
		if err := consumer.commitKnownCall(ordinal, assistant, "batch-"+newClaimID(), call, uint64(len(*transcript)), output, ReceiptOutcomeError, transcript, messages); err != nil {
			return runCommitToolResult{output: output, isError: true, outcome: ReceiptOutcomeError}, err
		}
		return runCommitToolResult{output: output, isError: true, outcome: ReceiptOutcomeError}, ErrRunCommitStreamingUnsupported
	}

	batchID := "batch-" + newClaimID()
	intent := consumer.actionIntent(batchID, ordinal, call, invocation)
	if err := consumer.beginIntentBatch(RunIntentBatch{
		ActionBatchID:              batchID,
		AssistantTranscriptOrdinal: ordinal,
		AssistantMessage:           assistant,
		Intents:                    []ActionIntent{intent},
	}); err != nil {
		return runCommitToolResult{}, err
	}
	taskID := registerRunCompletionTask(ctx, intent, invocation.CompletionSignal())
	dispatch := runCommitDispatchResult{output: "Error: run owner cannot track prepared completion", isError: true, err: ErrRunCommitOwnerUnavailable}
	if taskID > 0 {
		dispatch = dispatchPreparedRunCommitCall(ctx, invocation, taskID)
	}
	dispatchState := DispatchStateDispatched
	resourceState := RunExecutionCleanupPending
	proof := tools.PreparedCompletionProof("")
	details, hasDetails := runCompletionDetails(ctx, taskID)
	if !dispatch.committed {
		dispatchState = DispatchStateNotDispatched
		resourceState = RunExecutionQuiescent
	} else if hasDetails && details.state == runCompletionTaskReady && validRunCompletionProof(details.proof) {
		resourceState = RunExecutionQuiescent
		proof = details.proof
	}
	execution := ActionExecution{
		RunID:                      intent.RunID,
		AssistantTranscriptOrdinal: intent.AssistantTranscriptOrdinal,
		ToolCallID:                 intent.ToolCallID,
		ActionID:                   intent.ActionID,
		OwnerGeneration:            intent.OwnerGeneration,
		DispatchState:              dispatchState,
		ResourceState:              resourceState,
		CompletionProof:            proof,
	}
	if err := consumer.recordTaskExecution(taskID, execution); err != nil {
		return runCommitToolResult{}, err
	}
	outcome := runCommitReceiptOutcome(ctx, dispatch)
	appendToolReceipt(messages, transcript, call, dispatch.output, outcome)
	index := uint64(len(*transcript) - 1)
	if err := consumer.recordActionReceipt(taskID, ordinal, intent, dispatchState, index, (*transcript)[len(*transcript)-1]); err != nil {
		return runCommitToolResult{output: dispatch.output, isError: true, outcome: outcome}, err
	}
	if !dispatch.committed {
		if err := consumer.resolveTask(taskID, ExecutionResolution{RunID: intent.RunID, ActionID: intent.ActionID, Evidence: ResolutionNeverDispatched}); err != nil {
			return runCommitToolResult{output: dispatch.output, isError: true, outcome: outcome}, err
		}
		if ctx.Err() != nil {
			return runCommitToolResult{output: dispatch.output, isError: true, outcome: outcome}, ctx.Err()
		}
	} else {
		resolved, err := consumer.resolvePreparedTask(ctx, taskID, intent, execution)
		if err != nil {
			return runCommitToolResult{output: dispatch.output, isError: true, outcome: outcome}, err
		}
		if !resolved {
			return runCommitToolResult{output: dispatch.output, isError: true, outcome: outcome}, runCommitQuiescenceError{cause: ErrRunCommitQuiescenceUnproven}
		}
	}
	if outcome == ReceiptOutcomeUnknown {
		return runCommitToolResult{output: dispatch.output, isError: true, outcome: outcome}, markUnknownToolOutcome(dispatch.err)
	}
	return runCommitToolResult{output: dispatch.output, isError: dispatch.isError, outcome: outcome}, nil
}

func (consumer *runCommitConsumer) resolvePreparedTask(ctx context.Context, taskID uint64, intent ActionIntent, firstExecution ActionExecution) (bool, error) {
	details, ok := runCompletionDetails(ctx, taskID)
	if !ok {
		return false, nil
	}
	if details.state != runCompletionTaskReady || !validRunCompletionProof(details.proof) {
		if details.state != runCompletionTaskResourceOpen {
			return false, nil
		}
		ready, err := consumer.owner.waitForCompletionTasks(ctx, []uint64{taskID})
		if !ready {
			return false, err
		}
		details, ok = runCompletionDetails(ctx, taskID)
		if !ok || details.state != runCompletionTaskReady || !validRunCompletionProof(details.proof) {
			return false, nil
		}
		if firstExecution.CompletionProof == "" {
			lateExecution := firstExecution
			lateExecution.ResourceState = RunExecutionQuiescent
			lateExecution.CompletionProof = details.proof
			if err := consumer.recordTaskExecution(taskID, lateExecution); err != nil {
				return false, err
			}
		}
	}
	if err := consumer.resolveTask(taskID, ExecutionResolution{
		RunID:    intent.RunID,
		ActionID: intent.ActionID,
		Evidence: ResolutionLocalResourcesStopped,
	}); err != nil {
		return false, err
	}
	return true, nil
}

func runCommitReceiptOutcome(_ context.Context, dispatch runCommitDispatchResult) string {
	if dispatch.err != nil {
		if hasUnknownToolOutcome(dispatch.err) {
			return ReceiptOutcomeUnknown
		}
		if errors.Is(dispatch.err, context.Canceled) || errors.Is(dispatch.err, context.DeadlineExceeded) {
			if !dispatch.committed {
				return ReceiptOutcomeInterrupted
			}
			return ReceiptOutcomeUnknown
		}
		return ReceiptOutcomeError
	}
	if dispatch.isError {
		return ReceiptOutcomeError
	}
	return ReceiptOutcomeSuccess
}

type runCommitParallelAction struct {
	call       llm.ToolCall
	invocation tools.PreparedInvocation
	intent     ActionIntent
	taskID     uint64
	known      *runCommitToolResult
	stopErr    error
	result     runCommitDispatchResult
	done       chan struct{}
	complete   bool
}

func (consumer *runCommitConsumer) executeParallelGroup(
	ctx context.Context,
	registry tools.ToolRegistry,
	ordinal uint64,
	assistant AgentMessage,
	calls []llm.ToolCall,
	canceled map[string]bool,
	stream EventStream,
	transcript *[]TranscriptEntry,
	messages *[]llm.Message,
) error {
	batchID := "batch-" + newClaimID()
	actions := make([]runCommitParallelAction, len(calls))
	batch := RunIntentBatch{
		ActionBatchID:              batchID,
		AssistantTranscriptOrdinal: ordinal,
		AssistantMessage:           assistant,
	}
	for index, call := range calls {
		action := runCommitParallelAction{call: call}
		if canceled[call.ID] {
			known := runCommitToolResult{output: "Tool call canceled by interceptor", isError: true, outcome: ReceiptOutcomeInterrupted}
			action.known = &known
			batch.KnownReceipts = append(batch.KnownReceipts, consumer.knownReceipt(ordinal, call, uint64(len(*transcript)+index), known.output, known.outcome))
			actions[index] = action
			continue
		}
		invocation, err := consumer.prepareInvocation(ctx, registry, call)
		if err == nil {
			if _, streaming := invocation.(tools.PreparedStreamingInvocation); streaming && stream != nil {
				err = ErrRunCommitStreamingUnsupported
			}
		}
		if err != nil {
			known := runCommitToolResult{output: "Error: " + err.Error(), isError: true, outcome: ReceiptOutcomeError}
			if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				known.outcome = ReceiptOutcomeInterrupted
			}
			action.known = &known
			if errors.Is(err, ErrRunCommitPreparedInvocationUnavailable) || errors.Is(err, ErrRunCommitChildUnsupported) || errors.Is(err, ErrRunCommitStreamingUnsupported) {
				action.stopErr = err
			}
			batch.KnownReceipts = append(batch.KnownReceipts, consumer.knownReceipt(ordinal, call, uint64(len(*transcript)+index), known.output, known.outcome))
			actions[index] = action
			continue
		}
		action.invocation = invocation
		action.intent = consumer.actionIntent(batchID, ordinal, call, invocation)
		batch.Intents = append(batch.Intents, action.intent)
		actions[index] = action
	}
	if err := consumer.beginIntentBatch(batch); err != nil {
		return err
	}

	var workers sync.WaitGroup
	for index := range actions {
		action := &actions[index]
		if action.invocation == nil {
			continue
		}
		action.taskID = registerRunCompletionTask(ctx, action.intent, action.invocation.CompletionSignal())
		action.done = make(chan struct{})
		workers.Add(1)
		go func(action *runCommitParallelAction) {
			defer workers.Done()
			defer close(action.done)
			if action.taskID == 0 {
				action.result = runCommitDispatchResult{output: "Error: run owner cannot track prepared completion", isError: true, err: ErrRunCommitOwnerUnavailable}
				return
			}
			action.result = dispatchPreparedRunCommitCall(ctx, action.invocation, action.taskID)
		}(action)
	}
	allDone := make(chan struct{})
	go func() {
		workers.Wait()
		close(allDone)
	}()
	select {
	case <-allDone:
	case <-ctx.Done():
	}
	for index := range actions {
		action := &actions[index]
		if action.invocation == nil {
			continue
		}
		select {
		case <-action.done:
			action.complete = true
		default:
			action.complete = false
		}
	}

	var runErr error
	for index := range actions {
		action := &actions[index]
		transcriptIndex := uint64(len(*transcript))
		if action.known != nil {
			appendToolReceipt(messages, transcript, action.call, action.known.output, action.known.outcome)
			entry := (*transcript)[len(*transcript)-1]
			if err := consumer.recordTranscriptReceipt(ordinal, nil, transcriptIndex, entry); err != nil {
				return err
			}
			runErr = errors.Join(runErr, action.stopErr)
			continue
		}
		committed := action.result.committed
		output := action.result.output
		if action.taskID == 0 {
			runErr = errors.Join(runErr, ErrRunCommitOwnerUnavailable)
		}
		if !action.complete {
			// A cancellation snapshot cannot safely inspect DispatchCommitted.
			// Keep the reservation fenced as dispatched until the owner joins the
			// worker and observes the exact prepared completion signal.
			committed = true
			output = "Error: tool result is unknown because dispatch did not return before cancellation"
			action.result.err = toolOutcomeUnknownError{cause: ctx.Err()}
			action.result.committed = true
			runErr = errors.Join(runErr, markUnknownToolOutcome(action.result.err))
		}
		dispatchState := DispatchStateDispatched
		resourceState := RunExecutionCleanupPending
		proof := tools.PreparedCompletionProof("")
		if !committed {
			dispatchState = DispatchStateNotDispatched
			resourceState = RunExecutionQuiescent
		} else if details, ok := runCompletionDetails(ctx, action.taskID); ok && details.state == runCompletionTaskReady && validRunCompletionProof(details.proof) {
			resourceState = RunExecutionQuiescent
			proof = details.proof
		}
		execution := ActionExecution{
			RunID:                      action.intent.RunID,
			AssistantTranscriptOrdinal: action.intent.AssistantTranscriptOrdinal,
			ToolCallID:                 action.intent.ToolCallID,
			ActionID:                   action.intent.ActionID,
			OwnerGeneration:            action.intent.OwnerGeneration,
			DispatchState:              dispatchState,
			ResourceState:              resourceState,
			CompletionProof:            proof,
		}
		if err := consumer.recordTaskExecution(action.taskID, execution); err != nil {
			return err
		}
		outcome := runCommitReceiptOutcome(ctx, action.result)
		if !action.complete {
			outcome = ReceiptOutcomeUnknown
		}
		appendToolReceipt(messages, transcript, action.call, output, outcome)
		entry := (*transcript)[len(*transcript)-1]
		if err := consumer.recordActionReceipt(action.taskID, ordinal, action.intent, dispatchState, transcriptIndex, entry); err != nil {
			return err
		}
		if outcome == ReceiptOutcomeUnknown {
			runErr = errors.Join(runErr, markUnknownToolOutcome(action.result.err))
		}
		if !committed {
			if err := consumer.resolveTask(action.taskID, ExecutionResolution{RunID: action.intent.RunID, ActionID: action.intent.ActionID, Evidence: ResolutionNeverDispatched}); err != nil {
				return err
			}
		}
	}

	var waitIDs []uint64
	for _, action := range actions {
		if action.invocation == nil || action.taskID == 0 || !action.complete || !action.result.committed {
			continue
		}
		if details, ok := runCompletionDetails(ctx, action.taskID); ok && details.state == runCompletionTaskResourceOpen {
			waitIDs = append(waitIDs, action.taskID)
		}
	}
	if len(waitIDs) > 0 && ctx.Err() == nil {
		_, waitErr := consumer.owner.waitForCompletionTasks(ctx, waitIDs)
		if waitErr != nil {
			runErr = errors.Join(runErr, runCommitQuiescenceError{cause: waitErr})
		}
	}
	for _, action := range actions {
		if action.invocation == nil || action.taskID == 0 || !action.complete || !action.result.committed {
			continue
		}
		details, ok := runCompletionDetails(ctx, action.taskID)
		if !ok || details.state != runCompletionTaskReady || !validRunCompletionProof(details.proof) {
			runErr = errors.Join(runErr, runCommitQuiescenceError{cause: ErrRunCommitQuiescenceUnproven})
			continue
		}
		if !details.proofExecutionDurable {
			lateExecution := ActionExecution{
				RunID:                      action.intent.RunID,
				AssistantTranscriptOrdinal: action.intent.AssistantTranscriptOrdinal,
				ToolCallID:                 action.intent.ToolCallID,
				ActionID:                   action.intent.ActionID,
				OwnerGeneration:            action.intent.OwnerGeneration,
				DispatchState:              DispatchStateDispatched,
				ResourceState:              RunExecutionQuiescent,
				CompletionProof:            details.proof,
			}
			if err := consumer.recordTaskExecution(action.taskID, lateExecution); err != nil {
				return err
			}
		}
		if !details.resolutionDurable {
			if err := consumer.resolveTask(action.taskID, ExecutionResolution{RunID: action.intent.RunID, ActionID: action.intent.ActionID, Evidence: ResolutionLocalResourcesStopped}); err != nil {
				return err
			}
		}
	}
	return runErr
}

func (consumer *runCommitConsumer) commitKnownCall(ordinal uint64, assistant AgentMessage, batchID string, call llm.ToolCall, index uint64, output, outcome string, transcript *[]TranscriptEntry, messages *[]llm.Message) error {
	known := consumer.knownReceipt(ordinal, call, index, output, outcome)
	if err := consumer.beginIntentBatch(RunIntentBatch{
		ActionBatchID:              batchID,
		AssistantTranscriptOrdinal: ordinal,
		AssistantMessage:           assistant,
		KnownReceipts:              []RunToolReceipt{known},
	}); err != nil {
		return err
	}
	appendToolReceipt(messages, transcript, call, output, outcome)
	if err := consumer.recordTranscriptReceipt(ordinal, nil, index, (*transcript)[len(*transcript)-1]); err != nil {
		return err
	}
	return nil
}

func persistenceError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return runCommitPersistenceError{operation: operation, cause: err}
}

func terminalOutcome(err error) RunOutcome {
	if err == nil {
		return RunOutcomeSuccess
	}
	if hasUnknownToolOutcome(err) {
		return RunOutcomeUnknown
	}
	var unknown runOutcomeUnknown
	if errors.As(err, &unknown) && unknown.OutcomeUnknown() {
		return RunOutcomeUnknown
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return RunOutcomeCancelled
	}
	return RunOutcomeError
}

func cloneTranscriptEntries(entries []TranscriptEntry) []TranscriptEntry {
	messages := make([]AgentMessage, len(entries))
	for i := range entries {
		messages[i] = entries[i].Message
	}
	cloned := cloneAgentMessages(messages)
	result := make([]TranscriptEntry, len(cloned))
	for i := range cloned {
		result[i].Message = cloned[i]
	}
	return result
}

func validRunCompletionProof(proof tools.PreparedCompletionProof) bool {
	switch proof {
	case tools.CompletionInlinePipelineJoined, tools.CompletionQueuedJobJoined, tools.CompletionOwnedProcessGroupJoined:
		return true
	default:
		return false
	}
}

type unavailableRunCommitCompactor struct{}

func (unavailableRunCommitCompactor) Compact(_ context.Context, items []compaction.TurnItem, _ int, _ compaction.TokenEstimator) ([]compaction.TurnItem, error) {
	return items, ErrRunCommitCompactionProjectionUnavailable
}
