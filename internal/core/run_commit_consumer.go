package core

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"unicode/utf8"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

var (
	ErrRunCommitOwnerUnavailable                 = errors.New("core: durable run requires its run owner")
	ErrRunCommitPreparedInvocationUnavailable    = errors.New("core: durable run requires prepared tool dispatch")
	ErrRunCommitChildUnsupported                 = errors.New("core: durable child run requires an independent physical session")
	ErrRunCommitStreamingUnsupported             = errors.New("core: durable streaming dispatch requires an attested synchronous stream sink")
	ErrRunCommitQuiescenceUnproven               = errors.New("core: tool completion did not prove local resource quiescence")
	ErrRunCommitCompactionProjectionUnavailable  = errors.New("core: durable compaction requires a typed history projection")
	ErrRunCommitSinkPanicked                     = errors.New("core: run commit sink panicked")
	ErrRunCommitProcessSessionActive             = errors.New("core: process-session leases are still active under the invocation host")
	ErrRunCommitProcessSessionClosureUnconfirmed = errors.New("core: invocation host closed without durable process-session closure acknowledgement")
	ErrRunCommitProcessSessionUnavailable        = errors.New("core: process-session dispatch requires a durable host-lease sink")
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
	ctx                     context.Context
	owner                   *runOwner
	sink                    RunCommitSink
	source                  *CompactionSourceSnapshot
	projections             []CompactionProjection
	initialInput            *AgentMessage
	initialInputLossy       bool
	binding                 RunCommitBinding
	runID                   string
	terminalID              TerminalCheckpointID
	identityMu              sync.RWMutex
	identities              map[string]ExecutionIdentity
	completedTools          map[string]completedToolAction
	processSession          *tools.ProcessSessionInvocation
	processSessionAdmission *tools.ProcessSessionAdmissionCapability
	completionTaskIDs       map[string]uint64
}

type completedToolAction struct {
	actionID        string
	callID          string
	runID           string
	ownerGeneration string
	ordinal         uint64
	metadata        map[string]any
	readEvidence    *tools.PreparedReadEvidence
	editMismatch    *tools.PreparedEditMismatchEvidence
}

type runCommitDispatchResult struct {
	contentBlocks                 []llm.ContentBlock
	metadata                      map[string]any
	output                        string
	isError                       bool
	err                           error
	returned                      bool
	committed                     bool
	sourceTruncated               *bool
	knownBashOwnTimeout           bool
	receiptOutputTruncated        bool
	processSessionLease           *tools.ProcessSessionLeaseSnapshot
	processSessionAdmission       *tools.ProcessSessionAdmissionCapability
	processSessionLeaseInvocation *tools.ProcessSessionInvocation
	readEvidence                  *tools.PreparedReadEvidence
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
	defer func() {
		if result.err != nil || result.isError {
			result.contentBlocks = nil
		}
	}()
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
		if err == nil && !toolResult.IsError {
			result.readEvidence, _ = tools.PreparedReadEvidenceFor(invocation, toolResult)
		}
		if toolResult.ProcessSessionLease != nil {
			lease := tools.CloneProcessSessionLeaseSnapshot(*toolResult.ProcessSessionLease)
			result.processSessionLease = &lease
		}
		if toolResult.ProcessSessionAdmission != nil {
			capability := *toolResult.ProcessSessionAdmission
			result.processSessionAdmission = &capability
		}
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
	blocks, imageErr := toolResultContentBlocks(toolResult)
	if imageErr != nil {
		result.output = "Error: " + imageErr.Error()
		result.err, result.isError = imageErr, true
		return result
	}
	result.contentBlocks = blocks
	captureMaterializedArtifact(ctx, invocation.NormalizedCall(), toolResult)
	call := invocation.NormalizedCall()
	result.output, result.receiptOutputTruncated = boundedSuccessfulReceiptOutput(toolResult.Output, call.ID, call.Name)
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
	if err != nil {
		return persistenceError("input", err)
	}
	if index == 0 {
		lossy := (message.ContentBlocks != nil && len(message.ContentBlocks) == 0) || (message.ToolCalls != nil && len(message.ToolCalls) == 0)
		frozen := cloneAgentMessage(message)
		frozen.Runtime = llm.RuntimeMetadata{}
		if consumer.initialInput != nil && !reflect.DeepEqual(*consumer.initialInput, frozen) {
			return fmt.Errorf("%w: current input changed after commit", ErrDurableCompactionUnsupported)
		}
		consumer.initialInput = &frozen
		consumer.initialInputLossy = lossy
	}
	return nil
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

func (consumer *runCommitConsumer) commitCompaction(projection CompactionProjection, current ...compaction.TurnItem) error {
	if consumer == nil {
		return nil
	}
	commitCtx := consumer.commitContext()
	if projection.RetainCurrentInput {
		commitCtx = consumer.ctx
		if err := commitCtx.Err(); err != nil {
			return err
		}
		if err := CheckRunGeneration(commitCtx); err != nil {
			return err
		}
		if consumer.initialInput == nil || consumer.initialInputLossy || len(current) != 1 {
			return fmt.Errorf("%w: retained input was not captured at commit", ErrDurableCompactionUnsupported)
		}
		if err := consumer.validateCommittedInput(current[0]); err != nil {
			return err
		}
		digest, err := RetainedInputDigest(consumer.runID, *consumer.initialInput)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrDurableCompactionUnsupported, err)
		}
		if projection.RetainedInputDigest != "" && projection.RetainedInputDigest != digest {
			return fmt.Errorf("%w: retained input digest changed", ErrDurableCompactionUnsupported)
		}
		projection.RetainedInputDigest = digest
	} else if projection.RetainedInputDigest != "" {
		return fmt.Errorf("%w: unanchored projection carries an input digest", ErrDurableCompactionUnsupported)
	}
	if err := consumer.sink.CommitCompaction(commitCtx, projection); err != nil {
		return runCommitPersistenceError{operation: "compaction", cause: err}
	}
	consumer.projections = append(consumer.projections, projection)
	return nil
}

func (consumer *runCommitConsumer) validateCommittedInput(item compaction.TurnItem) error {
	if consumer.initialInput == nil || consumer.initialInputLossy {
		return fmt.Errorf("%w: retained input was not captured losslessly", ErrDurableCompactionUnsupported)
	}
	frozen := consumer.initialInput
	if item.Role != frozen.Role || item.Content != frozen.Content || !reflect.DeepEqual(item.ContentBlocks, frozen.ContentBlocks) ||
		!reflect.DeepEqual(item.Usage, frozen.Usage) || !reflect.DeepEqual(item.ToolCalls, frozen.ToolCalls) ||
		item.ToolCallID != frozen.ToolCallID || item.ToolName != frozen.ToolName || item.FinishReason != "" ||
		item.ToolResult != "" || item.ReceiptOutcome != frozen.ReceiptOutcome || item.IsCompaction ||
		(item.InputKind != "" && item.InputKind != frozen.InputKind) || item.Runtime.CompactionMarker {
		return fmt.Errorf("%w: retained input differs from committed input", ErrDurableCompactionUnsupported)
	}
	return nil
}

// activeProcessSessionRefs returns manager-validated active leases that also
// have exact durable action/receipt records. Persisted rows alone cannot
// recreate the same-manager runtime capability.
func (consumer *runCommitConsumer) activeProcessSessionRefs() ([]ActiveProcessSessionRef, error) {
	if consumer == nil || consumer.processSession == nil {
		return nil, nil
	}
	sink, ok := consumer.sink.(RunCommitProcessSessionSink)
	if !ok {
		return nil, ErrRunCommitProcessSessionUnavailable
	}
	for {
		leases, err := sink.ActiveProcessSessionLeases(consumer.commitContext(), consumer.runID)
		if err != nil {
			return nil, fmt.Errorf("core: inspect active process-session leases: %w", err)
		}
		refs := make([]ActiveProcessSessionRef, 0, len(leases))
		closing := ""
		for _, lease := range leases {
			if consumer.processSession.AllowsLease(lease) {
				refs = append(refs, ActiveProcessSessionRef{
					OwnerID: lease.OwnerID, WorkspaceID: lease.WorkspaceID, RunID: lease.RunID,
					SessionID: lease.SessionID, OwnerGeneration: lease.OwnerGeneration, StartActionID: lease.Action.ActionID,
				})
				continue
			}
			live, isClosing, lifecycleErr := consumer.processSession.LeaseLifecycle(lease)
			if lifecycleErr != nil || live {
				if lifecycleErr == nil {
					lifecycleErr = errors.New("core: process-session lease is live but failed exact physical validation")
				}
				return nil, fmt.Errorf("core: durable process-session lease does not match its live manager: %w", lifecycleErr)
			}
			if !isClosing {
				return nil, errors.New("core: durable process-session lease has no live or owned-closing manager state")
			}
			closing = lease.SessionID
			break
		}
		if closing == "" {
			sort.Slice(refs, func(left, right int) bool {
				return refs[left].SessionID < refs[right].SessionID
			})
			return refs, nil
		}
		_, err = consumer.waitForProcessSessionClosure(closing)
		if err != nil {
			return nil, err
		}
		refreshed, err := sink.ActiveProcessSessionLeases(consumer.commitContext(), consumer.runID)
		if err != nil {
			return nil, fmt.Errorf("core: refresh process-session leases after closure: %w", err)
		}
		for _, lease := range refreshed {
			if lease.SessionID == closing && lease.RunID == consumer.runID && lease.WorkspaceID == consumer.binding.WorkspaceID &&
				(lease.Phase == tools.ProcessSessionLeaseActive || lease.Phase == tools.ProcessSessionLeaseDelivered) {
				return nil, errors.New("core: process-session closure acknowledgement did not clear its active durable lease")
			}
		}
	}
}

func (consumer *runCommitConsumer) waitForProcessSessionClosure(sessionID string) (bool, error) {
	if consumer == nil || consumer.processSession == nil || sessionID == "" {
		return false, ErrRunCommitProcessSessionClosureUnconfirmed
	}
	leaseDone := consumer.processSession.SessionClosed(sessionID)
	closureAttempted := consumer.processSession.SessionClosureAttempted(sessionID)
	ownerClosed := consumer.processSession.OwnerClosed()
	if leaseDone == nil || closureAttempted == nil || ownerClosed == nil {
		return false, ErrRunCommitProcessSessionClosureUnconfirmed
	}
	select {
	case <-leaseDone:
		return true, nil
	default:
	}
	select {
	case <-leaseDone:
		return false, nil
	case <-closureAttempted:
		select {
		case <-leaseDone:
			return false, nil
		default:
			return false, ErrRunCommitProcessSessionClosureUnconfirmed
		}
	case <-ownerClosed:
		select {
		case <-leaseDone:
			return false, nil
		default:
			return false, ErrRunCommitProcessSessionClosureUnconfirmed
		}
	case <-consumer.ctx.Done():
		select {
		case <-leaseDone:
			return false, nil
		default:
			return false, consumer.ctx.Err()
		}
	}
}

func (consumer *runCommitConsumer) beginIntentBatch(batch RunIntentBatch, transcript *[]TranscriptEntry, messages *[]llm.Message) error {
	if consumer == nil {
		return nil
	}
	batch.RunID = consumer.runID
	batch.CanonicalWorkspaceRoot = consumer.binding.CanonicalWorkspaceRoot
	batch.ProcessSessionAdmission = consumer.processSessionAdmission
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

func (consumer *runCommitConsumer) completeProcessSessionAction(invocation *tools.ProcessSessionInvocation, lease *tools.ProcessSessionLeaseSnapshot, intent ActionIntent) error {
	if consumer == nil || invocation == nil || lease == nil {
		return nil
	}
	sink, ok := consumer.sink.(RunCommitProcessSessionSink)
	if !ok {
		return ErrRunCommitProcessSessionUnavailable
	}
	publish := func(snapshot tools.ProcessSessionLeaseSnapshot) error {
		*lease = tools.CloneProcessSessionLeaseSnapshot(snapshot)
		if snapshot.Phase != tools.ProcessSessionLeaseActive && snapshot.Phase != tools.ProcessSessionLeaseStopped {
			return runCommitQuiescenceError{cause: ErrRunCommitQuiescenceUnproven}
		}
		if err := sink.CompleteProcessSessionAction(consumer.commitContext(), snapshot, processSessionActionRefFromIntent(intent), invocation.Operation()); err != nil {
			return runCommitPersistenceError{operation: "process-session action", cause: err}
		}
		// The manager reports its initial Active phase from inside Start, before
		// this consumer can durably record the delivered-live execution. Publish
		// Active only after the exact execution, receipt, and action checkpoint
		// are synced; a joined Stopped snapshot must remain stopped.
		if invocation.Operation() == tools.ProcessSessionOperationStart && snapshot.Phase == tools.ProcessSessionLeaseActive {
			if err := sink.RecordProcessSessionLease(consumer.commitContext(), snapshot); err != nil {
				return runCommitPersistenceError{operation: "process-session active lease", cause: err}
			}
		}
		return nil
	}
	if invocation.Operation() == tools.ProcessSessionOperationStart {
		if err := invocation.WithStartLeasePublication(lease.SessionID, publish); err != nil {
			return err
		}
		return nil
	}
	return publish(*lease)
}

func (consumer *runCommitConsumer) resolveProcessSessionLease(lease tools.ProcessSessionLeaseSnapshot) error {
	if consumer == nil {
		return nil
	}
	if lease.Phase != tools.ProcessSessionLeaseStopped || !lease.GroupAbsent || !lease.LeaderWaited || !lease.ReadersJoined {
		return ErrRunCommitQuiescenceUnproven
	}
	sink, ok := consumer.sink.(RunCommitProcessSessionSink)
	if !ok {
		return ErrRunCommitProcessSessionUnavailable
	}
	if err := sink.ResolveProcessSessionLease(consumer.commitContext(), lease, lease.GroupAbsent, lease.LeaderWaited, lease.ReadersJoined); err != nil {
		return runCommitPersistenceError{operation: "process-session closure", cause: err}
	}
	if consumer.processSession != nil {
		consumer.processSession.MarkLeaseClosed(lease.SessionID)
	}
	if taskID := consumer.completionTaskIDs[lease.Action.ActionID]; taskID > 0 {
		markRunCompletionResolved(consumer.ctx, taskID)
	}
	return nil
}

func (consumer *runCommitConsumer) resolveProcessSessionLeaseForInvocation(
	invocation *tools.ProcessSessionInvocation,
	lease tools.ProcessSessionLeaseSnapshot,
	taskID uint64,
) error {
	if consumer == nil || invocation == nil {
		return ErrRunCommitProcessSessionUnavailable
	}
	if lease.Phase != tools.ProcessSessionLeaseStopped || !lease.GroupAbsent || !lease.LeaderWaited || !lease.ReadersJoined {
		return ErrRunCommitQuiescenceUnproven
	}
	sink, ok := consumer.sink.(RunCommitProcessSessionSink)
	if !ok {
		return ErrRunCommitProcessSessionUnavailable
	}
	if err := sink.ResolveProcessSessionLease(consumer.commitContext(), lease, lease.GroupAbsent, lease.LeaderWaited, lease.ReadersJoined); err != nil {
		return runCommitPersistenceError{operation: "process-session closure", cause: err}
	}
	invocation.MarkLeaseClosed(lease.SessionID)
	if taskID > 0 {
		markRunCompletionResolved(consumer.ctx, taskID)
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
	var deliveryBinding *tools.ProcessSessionDeliveryBinding
	if consumer.processSession != nil {
		previous := ""
		for {
			refs, err := consumer.activeProcessSessionRefs()
			if err != nil {
				return toolOutcomeUnknownError{cause: err}
			}
			binding, active, err := consumer.processSession.DeliveryBinding(consumer.runID)
			if err != nil {
				return toolOutcomeUnknownError{cause: err}
			}
			if active {
				deliveryBinding = &binding
				break
			}
			if len(refs) == 0 {
				break
			}
			var current string
			for _, ref := range refs {
				current += ref.SessionID + "\x00"
			}
			if current == previous {
				return toolOutcomeUnknownError{cause: errors.New("core: active process-session lease is absent from its manager handoff")}
			}
			previous = current
		}
	}
	terminal := RunTerminal{
		Generation:             runGenerationMetadataPointer(consumer.ctx),
		RunID:                  consumer.runID,
		Transcript:             cloneTranscriptEntries(transcript),
		Projections:            append([]CompactionProjection(nil), projections...),
		Outcome:                outcome,
		TerminalCheckpointID:   consumer.terminalID,
		ProcessSessionDelivery: deliveryBinding,
	}
	if err := consumer.sink.CommitTerminal(consumer.commitContext(), terminal); err != nil {
		return runCommitPersistenceError{operation: "terminal", cause: err}
	}
	if deliveryBinding != nil {
		delivery, err := consumer.processSession.ConfirmTerminalDelivery(*deliveryBinding)
		if err != nil {
			return toolOutcomeUnknownError{cause: fmt.Errorf("confirm process-session terminal delivery: %w", err)}
		}
		markRunCommitTerminalSyncedWithDelivery(consumer.ctx, &delivery)
	} else {
		markRunCommitTerminalSynced(consumer.ctx)
	}
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
	if _, ok := tools.ProcessSessionInvocationFor(invocation); ok {
		if _, supported := consumer.sink.(RunCommitProcessSessionSink); !supported {
			return nil, nil, ErrRunCommitProcessSessionUnavailable
		}
	}
	return invocation, nil, nil
}

func processSessionActionRefFromIntent(intent ActionIntent) tools.ProcessSessionActionRef {
	return tools.ProcessSessionActionRef{
		SessionID: intent.SessionID, WorkspaceID: intent.WorkspaceID, RunID: intent.RunID,
		ActionBatchID: intent.ActionBatchID, ActionID: intent.ActionID, OwnerGeneration: intent.OwnerGeneration,
		AssistantTranscriptOrdinal: intent.AssistantTranscriptOrdinal, ToolCallID: intent.ToolCallID,
		SchemaDigest: intent.SchemaDigest, ArgumentDigest: intent.ArgumentDigest,
	}
}

func (consumer *runCommitConsumer) processSessionContext(ctx context.Context, intent ActionIntent, invocation tools.PreparedInvocation, taskID uint64) (context.Context, *tools.ProcessSessionInvocation, error) {
	processInvocation, ok := tools.ProcessSessionInvocationFor(invocation)
	if !ok {
		return ctx, nil, nil
	}
	leaseSink, ok := consumer.sink.(RunCommitProcessSessionSink)
	if !ok {
		return ctx, nil, ErrRunCommitProcessSessionUnavailable
	}
	if processInvocation.Operation() == tools.ProcessSessionOperationStart {
		ctx = tools.WithProcessSessionAction(ctx, processSessionActionRefFromIntent(intent), func(_ context.Context, lease tools.ProcessSessionLeaseSnapshot) error {
			// The Active checkpoint is written by completeProcessSessionAction
			// after the delivered-live execution and successful start receipt are
			// durable. Recording it here would order it before the execution.
			if lease.Phase == tools.ProcessSessionLeaseActive {
				return nil
			}
			if lease.Phase == tools.ProcessSessionLeaseStopped && processInvocation.StartActionPublished(lease.SessionID) {
				return consumer.resolveProcessSessionLeaseForInvocation(&processInvocation, lease, taskID)
			}
			if err := leaseSink.RecordProcessSessionLease(consumer.commitContext(), lease); err != nil {
				return runCommitPersistenceError{operation: "process-session lease", cause: err}
			}
			return nil
		})
	}
	return ctx, &processInvocation, nil
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
	if tools.PreparedEditRetryBlockedBeforeDispatch(ctx, invocation) {
		output := "Error: " + ErrEditRecoveryStaleRetry.Error()
		outcome := ReceiptOutcomeError
		if ctx.Err() != nil {
			output = "Error: edit retry was interrupted before dispatch"
			outcome = ReceiptOutcomeInterrupted
		}
		if err := consumer.commitKnownCall(ordinal, assistant, "batch-"+newClaimID(), call, uint64(len(*transcript)), output, outcome, transcript, messages); err != nil {
			return runCommitToolResult{output: output, isError: true, outcome: outcome}, err
		}
		if ctx.Err() != nil {
			return runCommitToolResult{output: output, isError: true, outcome: outcome}, ctx.Err()
		}
		return runCommitToolResult{output: output, isError: true, outcome: outcome}, ErrEditRecoveryStaleRetry
	}

	batchID := "batch-" + newClaimID()
	intent := consumer.actionIntent(batchID, ordinal, call, invocation)
	return consumer.executePreparedSequentialAction(ctx, ordinal, assistant, call, invocation, streamSink, batchID, intent, nil, transcript, messages)
}

func (consumer *runCommitConsumer) executePreparedSequentialAction(
	ctx context.Context,
	ordinal uint64,
	assistant AgentMessage,
	call llm.ToolCall,
	invocation tools.PreparedInvocation,
	streamSink tools.SynchronousStreamSink,
	batchID string,
	intent ActionIntent,
	dispatchOut *runCommitDispatchResult,
	transcript *[]TranscriptEntry,
	messages *[]llm.Message,
) (runCommitToolResult, error) {
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
	if taskID > 0 {
		if consumer.completionTaskIDs == nil {
			consumer.completionTaskIDs = make(map[string]uint64)
		}
		consumer.completionTaskIDs[intent.ActionID] = taskID
	}
	dispatch := runCommitDispatchResult{output: "Error: run owner cannot track prepared completion", isError: true, err: ErrRunCommitOwnerUnavailable}
	if taskID > 0 {
		dispatchCtx := consumer.processExecutionIdentityContext(ctx, taskID, intent, invocation)
		var processInvocation *tools.ProcessSessionInvocation
		var processErr error
		dispatchCtx, processInvocation, processErr = consumer.processSessionContext(dispatchCtx, intent, invocation, taskID)
		if processInvocation != nil {
			consumer.processSession = processInvocation
			dispatch.processSessionLeaseInvocation = processInvocation
		}
		if processErr != nil {
			dispatch = runCommitDispatchResult{output: "Error: " + processErr.Error(), isError: true, err: processErr, returned: true}
		} else {
			dispatch = dispatchPreparedRunCommitCall(dispatchCtx, invocation, streamSink, taskID)
		}
		if processInvocation != nil {
			dispatch.processSessionLeaseInvocation = processInvocation
		}
		if dispatch.processSessionAdmission != nil {
			consumer.processSessionAdmission = dispatch.processSessionAdmission
		}
		if processInvocation != nil {
			dispatch.processSessionLeaseInvocation = processInvocation
		}
	}
	if dispatchOut != nil {
		*dispatchOut = dispatch
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
	processInvocation := dispatch.processSessionLeaseInvocation
	processStartLive := processInvocation != nil && processInvocation.Operation() == tools.ProcessSessionOperationStart &&
		dispatch.processSessionLease != nil && processInvocation.AllowsLease(*dispatch.processSessionLease)
	processStartDeliveredAtExecution := processStartLive
	if processStartLive {
		resourceState = RunExecutionDeliveredLive
		proof = ""
	} else if dispatch.committed && processInvocation != nil && processInvocation.Operation() == tools.ProcessSessionOperationStart && dispatch.processSessionLease != nil {
		// Do not claim a live transfer when the exact manager no longer proves
		// the original process identity active. The typed closure path below must
		// resolve it before the run can become quiescent.
		resourceState = RunExecutionCleanupPending
		proof = ""
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
	if processStartLive {
		lease := tools.CloneProcessSessionLeaseSnapshot(*dispatch.processSessionLease)
		execution.ProcessSessionLease = &lease
	}
	if err := consumer.recordTaskExecution(taskID, execution); err != nil {
		return runCommitToolResult{}, err
	}
	outcome := runCommitReceiptOutcome(ctx, dispatch)
	appendToolReceipt(messages, transcript, call, dispatch.output, outcome, consumer)
	attachToolContentBlocks(messages, transcript, dispatch.contentBlocks)
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
	if outcome == ReceiptOutcomeSuccess {
		if err := consumer.completeProcessSessionAction(processInvocation, dispatch.processSessionLease, intent); err != nil {
			toolResult.isError = true
			toolResult.outcome = ReceiptOutcomeUnknown
			return toolResult, err
		}
		if processInvocation != nil && processInvocation.Operation() == tools.ProcessSessionOperationStart {
			processStartLive = dispatch.processSessionLease != nil &&
				dispatch.processSessionLease.Phase == tools.ProcessSessionLeaseActive &&
				processInvocation.AllowsLease(*dispatch.processSessionLease)
			if processStartDeliveredAtExecution && !processStartLive && dispatch.processSessionLease != nil &&
				dispatch.processSessionLease.Phase == tools.ProcessSessionLeaseActive {
				_, closing, lifecycleErr := processInvocation.LeaseLifecycle(*dispatch.processSessionLease)
				if lifecycleErr != nil || !closing {
					if lifecycleErr == nil {
						lifecycleErr = ErrRunCommitQuiescenceUnproven
					}
					toolResult.isError = true
					return toolResult, runCommitQuiescenceError{cause: lifecycleErr}
				}
				if _, waitErr := consumer.waitForProcessSessionClosure(dispatch.processSessionLease.SessionID); waitErr != nil {
					toolResult.isError = true
					return toolResult, waitErr
				}
				if refreshErr := processInvocation.WithStartLeasePublication(dispatch.processSessionLease.SessionID, func(snapshot tools.ProcessSessionLeaseSnapshot) error {
					*dispatch.processSessionLease = tools.CloneProcessSessionLeaseSnapshot(snapshot)
					return nil
				}); refreshErr != nil {
					toolResult.isError = true
					return toolResult, refreshErr
				}
				processStartLive = dispatch.processSessionLease.Phase == tools.ProcessSessionLeaseActive &&
					processInvocation.AllowsLease(*dispatch.processSessionLease)
			}
		}
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
	} else if processStartLive {
		leaseDone := processInvocation.SessionClosed(dispatch.processSessionLease.SessionID)
		ownerClosed := processInvocation.OwnerClosed()
		if !transferRunCompletionTaskToHost(ctx, taskID, leaseDone, ownerClosed) {
			toolResult.isError = true
			return toolResult, errors.Join(ErrRunCommitQuiescenceUnproven, errors.New("process-session lease could not transfer to its live host owner"))
		}
		if dispatch.processSessionAdmission != nil {
			consumer.processSessionAdmission = dispatch.processSessionAdmission
		}
	} else if processInvocation != nil && processInvocation.Operation() == tools.ProcessSessionOperationStart {
		lease := dispatch.processSessionLease
		if lease == nil || lease.Phase != tools.ProcessSessionLeaseStopped || !lease.GroupAbsent || !lease.LeaderWaited || !lease.ReadersJoined {
			toolResult.isError = true
			return toolResult, runCommitQuiescenceError{cause: ErrRunCommitQuiescenceUnproven}
		}
		if err := consumer.resolveProcessSessionLease(*lease); err != nil {
			toolResult.isError = true
			return toolResult, err
		}
		if runOwnerFromContext(ctx) == nil {
			toolResult.isError = true
			return toolResult, ErrRunCommitOwnerUnavailable
		}
		markRunCompletionResolved(ctx, taskID)
	} else {
		if dispatch.processSessionLease != nil && dispatch.processSessionLease.Phase == tools.ProcessSessionLeaseStopped {
			if err := consumer.resolveProcessSessionLease(*dispatch.processSessionLease); err != nil {
				toolResult.isError = true
				return toolResult, err
			}
		}
		resolved, err := consumer.resolvePreparedTask(ctx, taskID, intent, execution)
		if err != nil {
			toolResult.isError = true
			return toolResult, err
		}
		if !resolved {
			toolResult.isError = true
			return toolResult, runCommitQuiescenceError{cause: dispatch.err}
		}
	}
	if outcome == ReceiptOutcomeUnknown {
		toolResult.isError = true
		return toolResult, markUnknownToolOutcome(dispatch.err)
	}
	if dispatch.committed {
		consumer.markToolCompleted(call, intent, invocation, dispatch, outcome)
		if tools.PreparedEditRetryBlockedKnown(invocation, dispatch.err) {
			return toolResult, ErrEditRecoveryStaleRetry
		}
	}
	return toolResult, nil
}

func (consumer *runCommitConsumer) markToolCompleted(call llm.ToolCall, intent ActionIntent, invocation tools.PreparedInvocation, dispatch runCommitDispatchResult, outcome string) {
	if consumer.completedTools == nil {
		consumer.completedTools = map[string]completedToolAction{}
	}
	completed := completedToolAction{
		actionID: intent.ActionID, callID: call.ID, runID: intent.RunID,
		ownerGeneration: intent.OwnerGeneration, ordinal: intent.AssistantTranscriptOrdinal,
		metadata: cloneStringAnyMap(dispatch.metadata),
	}
	if outcome == ReceiptOutcomeSuccess && !dispatch.isError && dispatch.err == nil {
		completed.readEvidence = dispatch.readEvidence
	}
	if outcome == ReceiptOutcomeError && dispatch.committed && dispatch.err != nil {
		completed.editMismatch, _ = tools.PreparedEditMismatchEvidenceFor(invocation, dispatch.err)
	}
	consumer.completedTools[call.ID] = completed
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
	prepareErr error
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
			action.prepareErr = err
			if errors.Is(err, ErrRunCommitPreparedInvocationUnavailable) || errors.Is(err, ErrRunCommitChildUnsupported) || errors.Is(err, ErrRunCommitStreamingUnsupported) || errors.Is(err, ErrLoopDetected) {
				action.stopErr = err
			}
			batch.KnownReceipts = append(batch.KnownReceipts, consumer.knownReceipt(ordinal, call, uint64(len(*transcript)+index), known.output, known.outcome))
			actions[index] = action
			continue
		}
		if tools.PreparedEditRetryBlockedBeforeDispatch(ctx, invocation) {
			known := runCommitToolResult{output: "Error: " + ErrEditRecoveryStaleRetry.Error(), isError: true, outcome: ReceiptOutcomeError}
			if ctx.Err() != nil {
				known.output = "Error: edit retry was interrupted before dispatch"
				known.outcome = ReceiptOutcomeInterrupted
				action.stopErr = ctx.Err()
			} else {
				action.stopErr = ErrEditRecoveryStaleRetry
			}
			action.known = &known
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
	segments := delegationSegments(actions, consumer.binding.CanonicalWorkspaceRoot)
	if len(segments) == 0 {
		return consumer.executePreparedParallelGroup(ctx, batch, actions, transcript, messages)
	}
	for segmentIndex, segment := range segments {
		if err := ctx.Err(); err != nil {
			return consumer.finishUnreservedStagedCalls(ordinal, assistant, actions[segment.start:], transcript, messages, err)
		}
		phaseID := batchID
		if segmentIndex > 0 {
			phaseID = "batch-" + newClaimID()
		}
		phase := RunIntentBatch{ActionBatchID: phaseID, AssistantTranscriptOrdinal: ordinal, AssistantMessage: assistant}
		for i := segment.start; i < segment.end; i++ {
			action := &actions[i]
			if action.known != nil {
				phase.KnownReceipts = append(phase.KnownReceipts, consumer.knownReceipt(
					ordinal, action.call, uint64(len(*transcript)+i-segment.start), action.known.output, action.known.outcome,
				))
				continue
			}
			action.intent.ActionBatchID = phaseID
			phase.Intents = append(phase.Intents, action.intent)
		}
		if err := consumer.executePreparedParallelGroup(ctx, phase, actions[segment.start:segment.end], transcript, messages); err != nil {
			// Close only the unreserved suffix for a known cancellation. Unknown or
			// persistence failures keep their uncertain durable state fenced.
			if !hasUnknownToolOutcome(err) && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				return consumer.finishUnreservedStagedCalls(ordinal, assistant, actions[segment.end:], transcript, messages, err)
			}
			return err
		}
		for i := segment.start; i < segment.end; i++ {
			// A closed business error is delivered to the model and permits later
			// segments. A pre-dispatch guard/fence failure is a barrier because its
			// executor never ran.
			if actions[i].known == nil && !actions[i].result.committed {
				cause := actions[i].result.err
				if cause == nil {
					cause = ErrRunCommitPreparedInvocationUnavailable
				}
				return consumer.finishUnreservedStagedCalls(ordinal, assistant, actions[segment.end:], transcript, messages, cause)
			}
		}
	}
	return nil
}

type runCommitActionSegment struct{ start, end int }

// delegationSegments preserves ordinary parallel phases while isolating exact
// host-owned delegation and process-session calls. Prepared capabilities are
// not reissued, and only the typed process-session path may transfer a live
// lease to the invocation host.
func delegationSegments(actions []runCommitParallelAction, root string) []runCommitActionSegment {
	isolated := make([]bool, len(actions))
	hasIsolated := false
	for i := range actions {
		action := &actions[i]
		if action.invocation != nil && action.known == nil {
			if _, ok := tools.ProcessSessionInvocationFor(action.invocation); ok {
				isolated[i], hasIsolated = true, true
				continue
			}
		}
		intent := action.intent
		if action.invocation == nil || action.known != nil || intent.ManagedDelegationProof == nil ||
			!intent.ManagedDelegationProof.MatchesAction(intent.ToolIdentity, intent.EffectSet, intent.SchemaDigest, intent.ArgumentDigest, intent.ToolCallID, root) {
			continue
		}
		isolated[i], hasIsolated = true, true
	}
	if !hasIsolated {
		return nil
	}
	var segments []runCommitActionSegment
	ordinaryStart := 0
	for i := range actions {
		action := &actions[i]
		if action.known != nil && action.stopErr == nil && stagedPreparationStopsRun(action.prepareErr) {
			action.stopErr = action.prepareErr
		}
		// A known authority failure is a barrier too: later ordinary calls must
		// not run concurrently with that rejection.
		if !isolated[i] && (action.known == nil || action.stopErr == nil) {
			continue
		}
		if ordinaryStart < i {
			segments = append(segments, runCommitActionSegment{start: ordinaryStart, end: i})
		}
		segments = append(segments, runCommitActionSegment{start: i, end: i + 1})
		ordinaryStart = i + 1
	}
	if ordinaryStart < len(actions) {
		segments = append(segments, runCommitActionSegment{start: ordinaryStart, end: len(actions)})
	}
	return segments
}

func stagedPreparationStopsRun(err error) bool {
	if err == nil {
		return false
	}
	for _, failure := range []error{
		context.Canceled, context.DeadlineExceeded,
		tools.ErrDispatchFenceChanged, tools.ErrDispatchFenceUnavailable,
		tools.ErrInvalidDispatchFenceDomain, tools.ErrPreparedInvocationConsumed,
		tools.ErrPreparedInvocationUnavailable, tools.ErrPreparedDispatchLeaseUnavailable,
		tools.ErrPreparedToolPolicyUnavailable, tools.ErrPreparedCallMutation,
		tools.ErrPreparedProxyRegistrationStale,
	} {
		if errors.Is(err, failure) {
			return true
		}
	}
	return hasUnknownToolOutcome(err)
}

func (consumer *runCommitConsumer) finishUnreservedStagedCalls(
	ordinal uint64,
	assistant AgentMessage,
	actions []runCommitParallelAction,
	transcript *[]TranscriptEntry,
	messages *[]llm.Message,
	cause error,
) error {
	for _, action := range actions {
		if err := consumer.commitKnownCall(ordinal, assistant, "batch-"+newClaimID(), action.call, uint64(len(*transcript)), RejectedBatchSuffixOutput, ReceiptOutcomeInterrupted, transcript, messages); err != nil {
			return errors.Join(cause, err)
		}
	}
	return cause
}

func (consumer *runCommitConsumer) executePreparedParallelGroup(
	ctx context.Context,
	batch RunIntentBatch,
	actions []runCommitParallelAction,
	transcript *[]TranscriptEntry,
	messages *[]llm.Message,
) error {
	ordinal := batch.AssistantTranscriptOrdinal
	if len(actions) == 1 && actions[0].invocation != nil {
		if _, isProcessSession := tools.ProcessSessionInvocationFor(actions[0].invocation); isProcessSession {
			action := &actions[0]
			_, err := consumer.executePreparedSequentialAction(
				ctx, ordinal, batch.AssistantMessage, action.call, action.invocation, action.streamSink,
				batch.ActionBatchID, action.intent, &action.result, transcript, messages,
			)
			action.complete = err == nil
			return err
		}
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
		attachToolContentBlocks(messages, transcript, dispatch.contentBlocks)
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
				outcome := runCommitReceiptOutcome(ctx, action.result)
				consumer.markToolCompleted(action.call, action.intent, action.invocation, action.result, outcome)
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
