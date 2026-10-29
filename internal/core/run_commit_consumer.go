package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"unicode/utf8"

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
	ErrRunCommitSinkPanicked                    = errors.New("core: run commit sink panicked")
)

// ErrRunCommitCompletionUnsupported rejects a tool before any durable dispatch
// reservation when its captured executor has no reviewed completion contract.
var ErrRunCommitCompletionUnsupported = errors.New("core: tool is unavailable in durable sessions because its completion cannot be verified (tool_not_executed=true)")

type runCommitSinkPanicError struct{ operation string }

func (err runCommitSinkPanicError) Error() string {
	return fmt.Sprintf("%s during %s", ErrRunCommitSinkPanicked, err.operation)
}

func (runCommitSinkPanicError) Unwrap() error { return ErrRunCommitSinkPanicked }

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
	ctx            context.Context
	owner          *runOwner
	sink           RunCommitSink
	source         *CompactionSourceSnapshot
	projections    []CompactionProjection
	binding        RunCommitBinding
	runID          string
	terminalID     TerminalCheckpointID
	identityMu     sync.RWMutex
	identities     map[string]ExecutionIdentity
	completedTools map[string]completedToolAction
}

type completedToolAction struct {
	actionID string
	metadata map[string]any
}

type runCommitDispatchResult struct {
	metadata               map[string]any
	output                 string
	isError                bool
	err                    error
	returned               bool
	committed              bool
	sourceTruncated        *bool
	knownBashOwnTimeout    bool
	receiptOutputTruncated bool
}

type runCommitToolResult struct {
	output                 string
	isError                bool
	outcome                string
	sourceTruncated        *bool
	receiptOutputTruncated bool
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

func dispatchPreparedRunCommitCall(ctx context.Context, invocation tools.PreparedInvocation, streamSink tools.SynchronousStreamSink, taskID uint64) (result runCommitDispatchResult) {
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

	guardedCtx, finishCapability, admissionErr := beginRunCapability(ctx, invocation.NormalizedCall().Name)
	if admissionErr != nil {
		return runCommitDispatchResult{output: "Error: capability authorization revoked", isError: true, err: admissionErr}
	}
	defer func() {
		if fenceErr := finishCapability(); fenceErr != nil {
			if result.returned && !result.committed && ctx.Err() != nil && fenceErr == ctx.Err() {
				// The sealed prepared invocation positively never entered its
				// executor. Only the exact caller cancellation may retain this
				// known outcome; wrapped/combined authority errors stay unknown.
				result.isError = true
				result.err = errors.Join(result.err, fenceErr)
				return
			}
			result.output = "Error: capability authorization revoked; execution outcome unknown"
			result.isError = true
			result.err = toolOutcomeUnknownError{cause: errors.Join(result.err, fenceErr)}
			markRunOutcomeUnknown(ctx)
		}
	}()
	ctx = guardedCtx
	var toolResult *tools.ToolResult
	var err error
	if streamSink != nil {
		streaming, ok := invocation.(tools.PreparedStreamingInvocation)
		if !ok {
			return runCommitDispatchResult{output: "Error: " + ErrRunCommitStreamingUnsupported.Error(), isError: true, err: ErrRunCommitStreamingUnsupported}
		}
		toolResult, err = streaming.DispatchStreaming(ctx, streamSink)
	} else {
		toolResult, err = invocation.Dispatch(ctx)
	}
	if toolResult != nil {
		result.metadata = cloneStringAnyMap(toolResult.Metadata)
		result.sourceTruncated = sourceTruncatedFromMetadata(toolResult.Metadata)
	}
	result.returned = true
	result.committed = invocation.DispatchCommitted()
	if err != nil {
		partial := ""
		if toolResult != nil {
			partial = toolResult.Output
		}
		result.output, result.receiptOutputTruncated = boundedToolFailureOutput(partial, err)
		if result.metadata == nil {
			result.metadata = make(map[string]any)
		}
		result.metadata["receipt_output_truncated"] = result.receiptOutputTruncated
		result.isError = true
		result.err = err
		result.knownBashOwnTimeout = tools.PreparedBashOwnTimeoutKnown(invocation, err)
		var pending tools.PendingExecutionError
		if errors.As(err, &pending) {
			pendingDone := pending.ExecutionDone()
			if !attachRunCompletionPendingExecution(ctx, taskID, pendingDone) {
				// Non-R2/legacy callers have no prepared completion placeholder.
				// Preserve their independent pending-resource ownership path.
				registerRunPendingResource(ctx, invocation.NormalizedCall().ID, invocation.NormalizedCall().Name, pendingDone)
			}
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
	captureMaterializedArtifact(ctx, invocation.NormalizedCall(), toolResult)
	result.output = toolResult.Output
	result.isError = toolResult.IsError
	result.sourceTruncated = sourceTruncatedFromMetadata(toolResult.Metadata)
	return result
}

// Failed receipt text is a representation, not the complete captured source.
// 100000 bytes also leaves JSON's worst six-byte escaping well below the
// existing 1MiB durable row cap for ordinary receipt fields. Neither the Bash
// capture limit nor the session limit changes; arbitrary metadata remains
// subject to its existing independent validation and size constraints.
const maxFailedReceiptOutputBytes = 100000
const failedReceiptTruncatedMarker = "\n[partial output truncated for receipt]"

func boundedToolFailureOutput(partial string, cause error) (string, bool) {
	output := "Error: " + cause.Error()
	if partial != "" {
		output += "\n" + partial
	}
	if len(output) <= maxFailedReceiptOutputBytes {
		return output, false
	}
	end := maxFailedReceiptOutputBytes - len(failedReceiptTruncatedMarker)
	// Keep the retained prefix on a UTF-8 boundary. JSON's existing encoder
	// still handles any invalid source bytes; no new decoding authority exists.
	for end > 0 && !utf8.RuneStart(output[end]) {
		end--
	}
	return output[:end] + failedReceiptTruncatedMarker, true
}

func sourceTruncatedFromMetadata(metadata map[string]any) *bool {
	// This preserves only a tool-reported structured fact from the exact
	// prepared invocation. Transcript text, previews, and restored unknown
	// metadata never synthesize a truncation value.
	value, ok := metadata["truncated"].(bool)
	if !ok {
		return nil
	}
	return &value
}

func cloneOptionalBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
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
	consumer := &runCommitConsumer{
		ctx:        ctx,
		owner:      owner,
		sink:       sink,
		binding:    binding,
		runID:      runID,
		terminalID: TerminalCheckpointID("terminal-" + newClaimID()),
	}
	if sourceSink, ok := sink.(RunCommitCompactionSource); ok {
		snapshot, err := sourceSink.CompactionSourceSnapshot()
		if err != nil {
			return nil, fmt.Errorf("core: capture durable compaction source snapshot: %w", err)
		}
		if err := snapshot.Validate(); err != nil {
			return nil, fmt.Errorf("core: validate durable compaction source snapshot: %w", err)
		}
		snapshot = snapshot.Clone()
		consumer.source = &snapshot
	}
	return consumer, nil
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
		Generation:      runGenerationMetadataPointer(consumer.ctx),
	})
	return persistenceError("input", err)
}

func (consumer *runCommitConsumer) transcriptMetadata(index uint64, receiptOutcome string) llm.RuntimeMetadata {
	metadata := llm.RuntimeMetadata{ReceiptOutcome: receiptOutcome}
	if consumer == nil {
		return metadata
	}
	priorCount := uint64(0)
	if consumer.source != nil {
		priorCount = uint64(len(consumer.source.SourceEntryIDs))
	}
	metadata.Source = llm.SourceRef{
		Kind: llm.SourceRunTranscript, RunID: consumer.runID,
		PriorSourceCount: priorCount, TranscriptIndex: index,
	}
	return metadata
}

func (consumer *runCommitConsumer) commitCompaction(projection CompactionProjection) error {
	if consumer == nil {
		return nil
	}
	if err := consumer.sink.CommitCompaction(consumer.commitContext(), projection); err != nil {
		return runCommitPersistenceError{operation: "compaction", cause: err}
	}
	consumer.projections = append(consumer.projections, projection)
	return nil
}

func (consumer *runCommitConsumer) beginIntentBatch(batch RunIntentBatch, transcript *[]TranscriptEntry, messages *[]llm.Message) error {
	if consumer == nil {
		return nil
	}
	batch.RunID = consumer.runID
	batch.CanonicalWorkspaceRoot = consumer.binding.CanonicalWorkspaceRoot
	if err := consumer.sink.BeginIntentBatch(consumer.commitContext(), batch); err != nil {
		closed, closeErr := consumer.takeRejectedBatch(batch, transcript, messages)
		if closeErr != nil {
			return runCommitPersistenceError{operation: "rejected intent", cause: errors.Join(err, closeErr)}
		}
		if closed {
			return knownRejectedBatchError{cause: err}
		}
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

func (consumer *runCommitConsumer) processExecutionIdentityObserver(taskID uint64, intent ActionIntent) tools.ProcessExecutionIdentityObserver {
	return consumer.processExecutionIdentityObserverWithPIDNamespaceTokenSource(taskID, intent, CurrentPIDNamespaceToken)
}

func (consumer *runCommitConsumer) processExecutionIdentityObserverWithPIDNamespaceTokenSource(
	taskID uint64,
	intent ActionIntent,
	readPIDNamespaceToken func() (string, error),
) tools.ProcessExecutionIdentityObserver {
	return func(_ context.Context, observed tools.ProcessExecutionIdentity) error {
		identity := ExecutionIdentity{
			Kind:              ExecutionIdentityProcessGroup,
			Platform:          observed.Platform,
			HostGeneration:    observed.HostGeneration,
			LeaderPID:         observed.LeaderPID,
			ProcessStartToken: observed.ProcessStartToken,
			ProcessGroupID:    observed.ProcessGroupID,
		}
		if observed.Platform == "linux" {
			if readPIDNamespaceToken == nil {
				return errors.New("core: Linux PID namespace token source is unavailable")
			}
			pidNamespaceToken, err := readPIDNamespaceToken()
			if err != nil {
				return fmt.Errorf("core: read current Linux PID namespace token: %w", err)
			}
			if pidNamespaceToken == "" {
				return errors.New("core: current Linux PID namespace token is empty")
			}
			identity.PIDNamespaceToken = pidNamespaceToken
		}
		if err := identity.Validate(); err != nil {
			return fmt.Errorf("core: invalid prepared process execution identity: %w", err)
		}
		execution := ActionExecution{
			RunID:                      intent.RunID,
			AssistantTranscriptOrdinal: intent.AssistantTranscriptOrdinal,
			ToolCallID:                 intent.ToolCallID,
			ActionID:                   intent.ActionID,
			OwnerGeneration:            intent.OwnerGeneration,
			DispatchState:              DispatchStateDispatched,
			ResourceState:              RunExecutionCleanupPending,
			ExecutionIdentity:          &identity,
		}
		if err := consumer.recordTaskExecution(taskID, execution); err != nil {
			return err
		}
		consumer.identityMu.Lock()
		if consumer.identities == nil {
			consumer.identities = make(map[string]ExecutionIdentity)
		}
		consumer.identities[intent.ActionID] = identity
		consumer.identityMu.Unlock()
		return nil
	}
}

func (consumer *runCommitConsumer) executionIdentity(actionID string) *ExecutionIdentity {
	if consumer == nil || actionID == "" {
		return nil
	}
	consumer.identityMu.RLock()
	identity, ok := consumer.identities[actionID]
	consumer.identityMu.RUnlock()
	if !ok {
		return nil
	}
	copy := identity
	return &copy
}

func (consumer *runCommitConsumer) processExecutionIdentityContext(ctx context.Context, taskID uint64, intent ActionIntent, invocation tools.PreparedInvocation) context.Context {
	if consumer == nil || invocation == nil || taskID == 0 || invocation.NormalizedCall().Name != "bash" || invocation.ToolIdentity().RegistrationSource() != tools.RegistrationSourceBuiltin {
		return ctx
	}
	return tools.WithProcessExecutionIdentityObserver(ctx, consumer.processExecutionIdentityObserver(taskID, intent))
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
	if projections == nil {
		projections = consumer.projections
	}
	if err := CheckRunGeneration(consumer.commitContext()); err != nil {
		return toolOutcomeUnknownError{cause: err}
	}
	terminal := RunTerminal{
		Generation:           runGenerationMetadataPointer(consumer.ctx),
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
	readProof, _ := tools.ScopedWorkspaceReadProofFor(invocation, consumer.binding.CanonicalWorkspaceRoot)
	delegationProof, _ := tools.ManagedDelegationProofFor(invocation, consumer.binding.CanonicalWorkspaceRoot)
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
		ScopedReadProof:            readProof,
		ManagedDelegationProof:     delegationProof,
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

func markLatestToolReceiptUnknown(messages *[]llm.Message, transcript *[]TranscriptEntry) {
	if messages != nil && len(*messages) > 0 {
		message := &(*messages)[len(*messages)-1]
		if message.Role == llm.RoleTool {
			message.Runtime.ReceiptOutcome = ReceiptOutcomeUnknown
		}
	}
	if transcript != nil && len(*transcript) > 0 {
		entry := &(*transcript)[len(*transcript)-1]
		if entry.Message.Role == string(llm.RoleTool) {
			entry.Message.ReceiptOutcome = ReceiptOutcomeUnknown
			entry.Message.Runtime.ReceiptOutcome = ReceiptOutcomeUnknown
		}
	}
}

func (consumer *runCommitConsumer) prepareInvocation(ctx context.Context, registry tools.ToolRegistry, call llm.ToolCall, stream EventStream) (tools.PreparedInvocation, tools.SynchronousStreamSink, error) {
	if stream != nil {
		// Get is used only to determine whether this call has a streaming
		// capability. The streaming preparer still freezes the actual
		// registration identity, authorization, schema, and execution stages.
		if definition, err := registry.Get(ctx, call.Name); err == nil {
			if _, streaming := definition.(tools.StreamingBashTool); streaming {
				streamSink := attestedEventStreamSinkForContext(ctx, stream)
				if streamSink == nil {
					return nil, nil, ErrRunCommitStreamingUnsupported
				}
				preparer, ok := registry.(tools.StreamingToolCallPreparer)
				if !ok {
					return nil, nil, ErrRunCommitStreamingUnsupported
				}
				invocation, err := preparer.PrepareStreamingToolCall(ctx, toToolsCall(call), streamSink)
				if err != nil {
					return nil, nil, err
				}
				if invocation == nil {
					return nil, nil, ErrRunCommitPreparedInvocationUnavailable
				}
				if err := preflightDurableCompletion(ctx, invocation, streamSink); err != nil {
					return nil, nil, err
				}
				return invocation, streamSink, nil
			}
		}
	}
	preparer, ok := registry.(tools.ToolCallPreparer)
	if !ok {
		return nil, nil, ErrRunCommitPreparedInvocationUnavailable
	}
	invocation, err := preparer.PrepareToolCall(ctx, toToolsCall(call))
	if err != nil {
		return nil, nil, err
	}
	if invocation == nil {
		return nil, nil, ErrRunCommitPreparedInvocationUnavailable
	}
	if stream != nil {
		if _, streaming := invocation.(tools.PreparedStreamingInvocation); streaming {
			// A registry that did not expose its streaming capability through Get
			// cannot receive an attested sink through its preparer. Fail closed.
			return nil, nil, ErrRunCommitStreamingUnsupported
		}
	}
	if err := preflightDurableCompletion(ctx, invocation, nil); err != nil {
		return nil, nil, err
	}
	return invocation, nil, nil
}

func preflightDurableCompletion(ctx context.Context, invocation tools.PreparedInvocation, streamSink tools.SynchronousStreamSink) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := CheckRunGeneration(ctx); err != nil {
		return err
	}
	supported, err := tools.PreparedCompletionSupported(ctx, invocation, streamSink)
	if err != nil {
		return err
	}
	if !supported {
		return ErrRunCommitCompletionUnsupported
	}
	return nil
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
	invocation, streamSink, prepareErr := consumer.prepareInvocation(ctx, registry, call, stream)
	if prepareErr != nil {
		output := "Error: " + prepareErr.Error()
		outcome := ReceiptOutcomeError
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			outcome = ReceiptOutcomeInterrupted
		}
		if err := consumer.commitKnownCall(ordinal, assistant, "batch-"+newClaimID(), call, uint64(len(*transcript)), output, outcome, transcript, messages); err != nil {
			return runCommitToolResult{output: output, isError: true, outcome: outcome}, err
		}
		if errors.Is(prepareErr, ErrRunCommitPreparedInvocationUnavailable) || errors.Is(prepareErr, ErrRunCommitChildUnsupported) || errors.Is(prepareErr, ErrRunCommitStreamingUnsupported) || errors.Is(prepareErr, ErrLoopDetected) {
			return runCommitToolResult{output: output, isError: true, outcome: outcome}, prepareErr
		}
		return runCommitToolResult{output: output, isError: true, outcome: outcome}, nil
	}

	batchID := "batch-" + newClaimID()
	intent := consumer.actionIntent(batchID, ordinal, call, invocation)
	rejectionTranscriptStart := len(*transcript)
	if err := consumer.beginIntentBatch(RunIntentBatch{
		ActionBatchID:              batchID,
		AssistantTranscriptOrdinal: ordinal,
		AssistantMessage:           assistant,
		Intents:                    []ActionIntent{intent},
	}, transcript, messages); err != nil {
		var known knownRejectedBatchError
		if errors.As(err, &known) {
			for _, entry := range (*transcript)[rejectionTranscriptStart:] {
				if entry.Message.Role == "tool" && entry.Message.ToolCallID == call.ID {
					return runCommitToolResult{output: entry.Message.Content, isError: true, outcome: entry.Message.ReceiptOutcome}, err
				}
			}
		}
		return runCommitToolResult{}, err
	}
	taskID := registerRunCompletionTask(ctx, intent, invocation.CompletionSignal())
	dispatch := runCommitDispatchResult{output: "Error: run owner cannot track prepared completion", isError: true, err: ErrRunCommitOwnerUnavailable}
	if taskID > 0 {
		dispatchCtx := consumer.processExecutionIdentityContext(ctx, taskID, intent, invocation)
		dispatch = dispatchPreparedRunCommitCall(dispatchCtx, invocation, streamSink, taskID)
	}
	dispatchState := DispatchStateDispatched
	resourceState := RunExecutionCleanupPending
	proof := tools.PreparedCompletionProof("")
	details, hasDetails := runCompletionDetails(ctx, taskID)
	if !dispatch.committed {
		dispatchState = DispatchStateNotDispatched
		resourceState = RunExecutionQuiescent
	} else if hasDetails && details.state == runCompletionTaskReady && !details.pendingResourceUnproven && validRunCompletionProof(details.proof) {
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
		ExecutionIdentity:          consumer.executionIdentity(intent.ActionID),
		CompletionProof:            proof,
	}
	if err := consumer.recordTaskExecution(taskID, execution); err != nil {
		return runCommitToolResult{}, err
	}
	outcome := runCommitReceiptOutcome(ctx, dispatch)
	appendToolReceipt(messages, transcript, call, dispatch.output, outcome, consumer)
	index := uint64(len(*transcript) - 1)
	(*transcript)[len(*transcript)-1].SourceTruncated = cloneOptionalBool(dispatch.sourceTruncated)
	(*transcript)[len(*transcript)-1].ReceiptOutputTruncated = dispatch.receiptOutputTruncated
	toolResult := runCommitToolResult{
		output: dispatch.output, isError: dispatch.isError, outcome: outcome,
		sourceTruncated:        cloneOptionalBool(dispatch.sourceTruncated),
		receiptOutputTruncated: dispatch.receiptOutputTruncated,
	}
	if err := consumer.recordActionReceipt(taskID, ordinal, intent, dispatchState, index, (*transcript)[len(*transcript)-1]); err != nil {
		markLatestToolReceiptUnknown(messages, transcript)
		toolResult.isError = true
		toolResult.outcome = ReceiptOutcomeUnknown
		return toolResult, err
	}
	if !dispatch.committed {
		if err := consumer.resolveTask(taskID, ExecutionResolution{RunID: intent.RunID, ActionID: intent.ActionID, Evidence: ResolutionNeverDispatched}); err != nil {
			toolResult.isError = true
			return toolResult, err
		}
		if ctx.Err() != nil {
			toolResult.isError = true
			return toolResult, ctx.Err()
		}
	} else {
		resolved, err := consumer.resolvePreparedTask(ctx, taskID, intent, execution)
		if err != nil {
			toolResult.isError = true
			return toolResult, err
		}
		if !resolved {
			toolResult.isError = true
			return toolResult, runCommitQuiescenceError{cause: ErrRunCommitQuiescenceUnproven}
		}
	}
	if outcome == ReceiptOutcomeUnknown {
		toolResult.isError = true
		return toolResult, markUnknownToolOutcome(dispatch.err)
	}
	if dispatch.committed {
		consumer.markToolCompleted(call.ID, intent.ActionID, dispatch.metadata)
	}
	return toolResult, nil
}

func (consumer *runCommitConsumer) markToolCompleted(id, actionID string, metadata map[string]any) {
	if consumer.completedTools == nil {
		consumer.completedTools = map[string]completedToolAction{}
	}
	consumer.completedTools[id] = completedToolAction{actionID: actionID, metadata: cloneStringAnyMap(metadata)}
}

func (consumer *runCommitConsumer) resolvePreparedTask(ctx context.Context, taskID uint64, intent ActionIntent, firstExecution ActionExecution) (bool, error) {
	details, ok := runCompletionDetails(ctx, taskID)
	if !ok {
		return false, nil
	}
	if details.state == runCompletionTaskResourceOpen {
		ready, err := consumer.owner.waitForCompletionTasks(ctx, []uint64{taskID})
		if !ready {
			return false, err
		}
		details, ok = runCompletionDetails(ctx, taskID)
	}
	if !ok || details.state != runCompletionTaskReady || details.pendingResourceUnproven || !validRunCompletionProof(details.proof) {
		return false, nil
	}
	// Receipt persistence can block while the owner observes the completion
	// group. Recheck proof durability after that boundary, even when the task is
	// already Ready now; otherwise Resolve could overtake the late proof row.
	if !details.proofExecutionDurable {
		lateExecution := firstExecution
		lateExecution.RunID = intent.RunID
		lateExecution.AssistantTranscriptOrdinal = intent.AssistantTranscriptOrdinal
		lateExecution.ToolCallID = intent.ToolCallID
		lateExecution.ActionID = intent.ActionID
		lateExecution.OwnerGeneration = intent.OwnerGeneration
		lateExecution.DispatchState = DispatchStateDispatched
		lateExecution.ResourceState = RunExecutionQuiescent
		lateExecution.CompletionProof = details.proof
		if err := consumer.recordTaskExecution(taskID, lateExecution); err != nil {
			return false, err
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

func runCommitReceiptOutcome(ctx context.Context, dispatch runCommitDispatchResult) string {
	if dispatch.err != nil {
		if hasUnknownToolOutcome(dispatch.err) {
			return ReceiptOutcomeUnknown
		}
		if dispatch.knownBashOwnTimeout && ctx.Err() == nil {
			return ReceiptOutcomeError
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
	streamSink tools.SynchronousStreamSink
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
	canceled map[string]string,
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
		if canceledOutput, isCanceled := canceled[call.ID]; isCanceled {
			if canceledOutput == "" {
				canceledOutput = toolInterceptorCanceledOutput
			}
			known := runCommitToolResult{output: canceledOutput, isError: true, outcome: ReceiptOutcomeInterrupted}
			action.known = &known
			batch.KnownReceipts = append(batch.KnownReceipts, consumer.knownReceipt(ordinal, call, uint64(len(*transcript)+index), known.output, known.outcome))
			actions[index] = action
			continue
		}
		invocation, streamSink, err := consumer.prepareInvocation(ctx, registry, call, stream)
		if err != nil {
			known := runCommitToolResult{output: "Error: " + err.Error(), isError: true, outcome: ReceiptOutcomeError}
			if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				known.outcome = ReceiptOutcomeInterrupted
			}
			action.known = &known
			if errors.Is(err, ErrRunCommitPreparedInvocationUnavailable) || errors.Is(err, ErrRunCommitChildUnsupported) || errors.Is(err, ErrRunCommitStreamingUnsupported) || errors.Is(err, ErrLoopDetected) {
				action.stopErr = err
			}
			batch.KnownReceipts = append(batch.KnownReceipts, consumer.knownReceipt(ordinal, call, uint64(len(*transcript)+index), known.output, known.outcome))
			actions[index] = action
			continue
		}
		action.invocation = invocation
		action.streamSink = streamSink
		action.intent = consumer.actionIntent(batchID, ordinal, call, invocation)
		batch.Intents = append(batch.Intents, action.intent)
		actions[index] = action
	}
	if err := consumer.beginIntentBatch(batch, transcript, messages); err != nil {
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
			dispatchCtx := consumer.processExecutionIdentityContext(ctx, action.taskID, action.intent, action.invocation)
			action.result = dispatchPreparedRunCommitCall(dispatchCtx, action.invocation, action.streamSink, action.taskID)
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
	completedResults := make([]runCommitDispatchResult, len(actions))
	for index := range actions {
		action := &actions[index]
		if action.invocation == nil {
			continue
		}
		select {
		case <-action.done:
			action.complete = true
			// Closing action.done synchronizes the worker's result assignment.
			// Never inspect its result slot for an action that has not joined.
			completedResults[index] = action.result
		default:
			action.complete = false
		}
	}

	var runErr error
	for index := range actions {
		action := &actions[index]
		transcriptIndex := uint64(len(*transcript))
		if action.known != nil {
			appendToolReceipt(messages, transcript, action.call, action.known.output, action.known.outcome, consumer)
			entry := (*transcript)[len(*transcript)-1]
			if err := consumer.recordTranscriptReceipt(ordinal, nil, transcriptIndex, entry); err != nil {
				return err
			}
			runErr = errors.Join(runErr, action.stopErr)
			continue
		}
		dispatch := completedResults[index]
		committed := dispatch.committed
		output := dispatch.output
		if action.taskID == 0 {
			runErr = errors.Join(runErr, ErrRunCommitOwnerUnavailable)
		}
		if !action.complete {
			// A cancellation snapshot cannot safely inspect DispatchCommitted.
			// Keep the reservation fenced as dispatched until the owner joins the
			// worker and observes the exact prepared completion signal.
			committed = true
			output = "Error: tool result is unknown because dispatch did not return before cancellation"
			dispatch = runCommitDispatchResult{
				output:    output,
				isError:   true,
				err:       toolOutcomeUnknownError{cause: ctx.Err()},
				committed: true,
			}
			runErr = errors.Join(runErr, markUnknownToolOutcome(dispatch.err))
		}
		dispatchState := DispatchStateDispatched
		resourceState := RunExecutionCleanupPending
		proof := tools.PreparedCompletionProof("")
		if !committed {
			dispatchState = DispatchStateNotDispatched
			resourceState = RunExecutionQuiescent
		} else if details, ok := runCompletionDetails(ctx, action.taskID); ok && details.state == runCompletionTaskReady && !details.pendingResourceUnproven && validRunCompletionProof(details.proof) {
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
			ExecutionIdentity:          consumer.executionIdentity(action.intent.ActionID),
			CompletionProof:            proof,
		}
		if err := consumer.recordTaskExecution(action.taskID, execution); err != nil {
			return err
		}
		outcome := runCommitReceiptOutcome(ctx, dispatch)
		if !action.complete {
			outcome = ReceiptOutcomeUnknown
		}
		appendToolReceipt(messages, transcript, action.call, output, outcome, consumer)
		(*transcript)[len(*transcript)-1].SourceTruncated = cloneOptionalBool(dispatch.sourceTruncated)
		(*transcript)[len(*transcript)-1].ReceiptOutputTruncated = dispatch.receiptOutputTruncated
		entry := (*transcript)[len(*transcript)-1]
		if err := consumer.recordActionReceipt(action.taskID, ordinal, action.intent, dispatchState, transcriptIndex, entry); err != nil {
			markLatestToolReceiptUnknown(messages, transcript)
			return err
		}
		if outcome == ReceiptOutcomeUnknown {
			runErr = errors.Join(runErr, markUnknownToolOutcome(dispatch.err))
		}
		if !committed {
			if err := consumer.resolveTask(action.taskID, ExecutionResolution{RunID: action.intent.RunID, ActionID: action.intent.ActionID, Evidence: ResolutionNeverDispatched}); err != nil {
				return err
			}
		}
	}

	var waitIDs []uint64
	for index := range actions {
		action := &actions[index]
		if action.invocation == nil || action.taskID == 0 || !action.complete || !completedResults[index].committed {
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
	for index := range actions {
		action := &actions[index]
		if action.invocation == nil || action.taskID == 0 || !action.complete || !completedResults[index].committed {
			continue
		}
		details, ok := runCompletionDetails(ctx, action.taskID)
		if !ok || details.state != runCompletionTaskReady || details.pendingResourceUnproven || !validRunCompletionProof(details.proof) {
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
				ExecutionIdentity:          consumer.executionIdentity(action.intent.ActionID),
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
	if runErr == nil {
		for _, action := range actions {
			if action.invocation != nil && action.complete && action.result.committed {
				consumer.markToolCompleted(action.call.ID, action.intent.ActionID, action.result.metadata)
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
	}, transcript, messages); err != nil {
		return err
	}
	appendToolReceipt(messages, transcript, call, output, outcome, consumer)
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

func terminalOutcome(runCtx context.Context, err error) RunOutcome {
	if err == nil {
		return RunOutcomeSuccess
	}
	if hasUnknownToolOutcome(err) {
		return RunOutcomeUnknown
	}
	if errors.Is(err, ErrLoopDetected) {
		return RunOutcomeError
	}
	if runCtx != nil {
		if runContextErr := runCtx.Err(); runContextErr != nil && errors.Is(err, runContextErr) {
			return RunOutcomeCancelled
		}
	}
	return RunOutcomeError
}

func normalizeLoopDetectedCause(ctx context.Context, err error) error {
	if !errors.Is(context.Cause(ctx), ErrLoopDetected) {
		return err
	}
	if err == nil {
		return ErrLoopDetected
	}
	if errors.Is(err, ErrLoopDetected) {
		return err
	}
	return errors.Join(ErrLoopDetected, err)
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
	return tools.IsPositiveCompletionProof(proof)
}
