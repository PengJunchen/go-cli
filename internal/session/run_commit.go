package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// SessionRunCommitSink persists one run through an already locked prepared
// session handle. It borrows that handle's writer and never closes the shared
// session; release returns only this run's lease to its owner.
type SessionRunCommitSink struct {
	mu                       sync.Mutex
	prepared                 PreparedSession
	writer                   DurableSessionWriter
	index                    *WorkspaceQuarantineIndex
	sessionID                string
	workspaceID              string
	branchID                 string
	leafID                   string
	baseView                 SessionView
	allRows                  []SessionEntry
	branchRows               []SessionEntry
	runID                    string
	ownerGen                 string
	terminalID               string
	inspectExecutionIdentity func(core.ExecutionIdentity) core.ProcessGroupExitInspection
	release                  func()
	releaseOnce              sync.Once
	closed                   bool
	rejectedBatch            *closedRejectedBatch
}

var _ core.RunCommitSink = (*SessionRunCommitSink)(nil)
var _ core.RunCommitCompactionSource = (*SessionRunCommitSink)(nil)

// NewSessionRunCommitSink captures the active branch and run-start view from
// prepared. workspaceID must already be canonical and initialized in index
// before an intent can be committed. release is the idempotent per-run lease
// return callback; it must not close the shared PreparedSession itself.
func NewSessionRunCommitSink(
	prepared PreparedSession,
	index *WorkspaceQuarantineIndex,
	workspaceID string,
	release func(),
) (*SessionRunCommitSink, error) {
	if prepared == nil {
		return nil, errors.New("session: run-commit sink requires a prepared session")
	}
	if index == nil {
		return nil, errors.New("session: run-commit sink requires a workspace index")
	}
	if workspaceID == "" || !validWorkspaceID(workspaceID) {
		return nil, errors.New("session: run-commit sink requires a valid canonical workspace ID")
	}
	snapshot, err := prepared.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("session: snapshot prepared run-commit session: %w", err)
	}
	if err := snapshot.PublishedView.Validate(); err != nil {
		return nil, fmt.Errorf("session: invalid prepared run-commit view: %w", err)
	}
	if snapshot.SessionID == "" || snapshot.PublishedView.SessionID != snapshot.SessionID || snapshot.ActiveBranchID == "" || snapshot.PublishedView.BranchID != snapshot.ActiveBranchID {
		return nil, errors.New("session: prepared run-commit snapshot identity is inconsistent")
	}
	allRows, branchRows, err := validateRunCommitSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	if err := validateAllRunTranscriptPhysicalPrefixes(branchRows); err != nil {
		return nil, fmt.Errorf("session: prepared run-commit transcript is invalid: %w", err)
	}
	if _, err := analyzeDurableActionJournal(allRows, snapshot.SessionID); err != nil {
		return nil, fmt.Errorf("session: prepared run-commit action journal is invalid: %w", err)
	}
	writer := prepared.DurableWriter()
	if writer == nil {
		return nil, errors.New("session: prepared run-commit session has no durable writer")
	}
	return &SessionRunCommitSink{
		prepared: prepared, writer: writer, index: index,
		sessionID: snapshot.SessionID, workspaceID: workspaceID,
		branchID: snapshot.ActiveBranchID, leafID: snapshot.ActiveLeafEntryID,
		baseView: snapshot.PublishedView.Clone(),
		allRows:  allRows, branchRows: branchRows, release: release,
		inspectExecutionIdentity: core.InspectProcessGroupExit,
	}, nil
}

// CompactionSourceSnapshot returns a defensive copy of the exact PublishedView
// captured when this sink was created. It intentionally does not refresh from
// a separately loaded session view.
func (s *SessionRunCommitSink) CompactionSourceSnapshot() (core.CompactionSourceSnapshot, error) {
	if s == nil {
		return core.CompactionSourceSnapshot{}, errors.New("session: compaction source snapshot requires a run sink")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return core.CompactionSourceSnapshot{}, errors.New("session: compaction source snapshot is unavailable after run sink close")
	}
	return core.CompactionSourceSnapshot{
		Revision: s.baseView.Revision, BranchID: s.baseView.BranchID,
		Messages:       core.CloneAgentMessages(s.baseView.Messages),
		SourceEntryIDs: append([]string(nil), s.baseView.SourceEntryIDs...),
	}, nil
}

// CloseRunCommit returns this sink's run lease once. The prepared handle stays
// open until its session manager receives a close request and its final lease.
func (s *SessionRunCommitSink) CloseRunCommit() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseOnce.Do(func() {
		s.closed = true
		if s.release != nil {
			s.release()
		}
	})
}

func (s *SessionRunCommitSink) CommitInput(ctx context.Context, input core.RunCommitInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateCallContextLocked(ctx, input.RunID); err != nil {
		return err
	}
	if err := s.rejectAfterTerminalLocked(); err != nil {
		return err
	}
	if input.InputKind != core.InputKindUser && input.InputKind != core.InputKindSteering && input.InputKind != core.InputKindFollowUp {
		return fmt.Errorf("session: unsupported run input kind %q", input.InputKind)
	}
	message := cloneRunCommitMessage(input.Message)
	if message.Role != "user" {
		return fmt.Errorf("session: run input transcript row has role %q, want user", message.Role)
	}
	if len(message.ToolCalls) != 0 || message.ToolCallID != "" || message.ToolName != "" || message.ReceiptOutcome != "" {
		return errors.New("session: run input message contains assistant or tool receipt fields")
	}
	if message.InputKind != "" && message.InputKind != input.InputKind {
		return errors.New("session: run input kind conflicts with its canonical message")
	}
	message.InputKind = input.InputKind
	rowsByIndex, err := transcriptRowsByIndex(s.branchRows, input.RunID)
	if err != nil {
		return err
	}
	if existing, exists := rowsByIndex[input.TranscriptIndex]; exists {
		if existing.Type != EntryTypeUser || existing.InputKind != input.InputKind || !durableRowMatchesMessage(existing, message) {
			return fmt.Errorf("session: run %q transcript index %d conflicts with its durable input", input.RunID, input.TranscriptIndex)
		}
		return nil
	}
	nextIndex, err := nextTranscriptIndex(rowsByIndex)
	if err != nil {
		return err
	}
	if input.TranscriptIndex != nextIndex {
		return fmt.Errorf("session: run %q input transcript index %d is not the next index %d", input.RunID, input.TranscriptIndex, nextIndex)
	}
	entryID, err := stableRunCommitEntryID("input", struct {
		RunID string
		Index uint64
	}{input.RunID, input.TranscriptIndex})
	if err != nil {
		return err
	}
	index := input.TranscriptIndex
	row := sessionEntryFromRunCommitMessage(message, EntryTypeUser)
	row.ID = entryID
	row.RunID = input.RunID
	row.WorkspaceID = s.workspaceID
	row.InputKind = input.InputKind
	if metadata, ok := core.RunGenerationMetadataFromContext(ctx); ok {
		row.Generation = &metadata
	} else if input.Generation != nil {
		copy := *input.Generation
		row.Generation = &copy
	}
	row.TranscriptIndex = &index
	row.Timestamp = time.Now().UTC()
	return s.appendAndRefreshLocked(ctx, row)
}

func (s *SessionRunCommitSink) BeginIntentBatch(ctx context.Context, batch core.RunIntentBatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateCallContextLocked(ctx, batch.RunID); err != nil {
		return err
	}
	if err := s.rejectAfterTerminalLocked(); err != nil {
		return err
	}
	if s.rejectedBatch != nil {
		return errors.New("session: this run already closed a denied batch")
	}
	if batch.AssistantMessage.Role != "assistant" || len(batch.AssistantMessage.ToolCalls) == 0 {
		return errors.New("session: intent batch requires an assistant message with tool calls")
	}
	if batch.AssistantMessage.InputKind != "" || batch.AssistantMessage.ToolCallID != "" || batch.AssistantMessage.ToolName != "" || batch.AssistantMessage.ReceiptOutcome != "" {
		return errors.New("session: assistant intent message contains input or receipt fields")
	}
	if batch.ActionBatchID == "" && len(batch.Intents) > 0 {
		return errors.New("session: reserved intent batch requires an action batch ID")
	}
	if err := validateRunCommitToolCalls(batch.AssistantMessage.ToolCalls); err != nil {
		return err
	}
	toolCalls := make(map[string]llm.ToolCall, len(batch.AssistantMessage.ToolCalls))
	callPosition := make(map[string]int, len(batch.AssistantMessage.ToolCalls))
	for index, call := range batch.AssistantMessage.ToolCalls {
		toolCalls[call.ID] = call
		callPosition[call.ID] = index
	}
	if err := validateKnownPreDispatchReceipts(batch, toolCalls); err != nil {
		return err
	}
	for _, receipt := range batch.KnownReceipts {
		for _, intent := range batch.Intents {
			if intent.ToolCallID == receipt.ToolCallID {
				return fmt.Errorf("session: tool call %q cannot be both reserved and a known pre-dispatch denial", receipt.ToolCallID)
			}
		}
	}
	assistant, exists, err := s.assistantOccurrenceLocked(batch.RunID, batch.AssistantTranscriptOrdinal)
	if err != nil {
		return err
	}
	message := cloneRunCommitMessage(batch.AssistantMessage)
	if exists {
		if assistant.WorkspaceID != s.workspaceID || !durableRowMatchesMessage(assistant, message) {
			return fmt.Errorf("session: assistant occurrence %d conflicts with its durable row", batch.AssistantTranscriptOrdinal)
		}
	} else {
		transcriptRows, err := transcriptRowsByIndex(s.branchRows, batch.RunID)
		if err != nil {
			return err
		}
		nextIndex, err := nextTranscriptIndex(transcriptRows)
		if err != nil {
			return err
		}
		entryID, err := stableRunCommitEntryID("assistant", struct {
			RunID   string
			Ordinal uint64
		}{batch.RunID, batch.AssistantTranscriptOrdinal})
		if err != nil {
			return err
		}
		index, ordinal := nextIndex, batch.AssistantTranscriptOrdinal
		assistant = sessionEntryFromRunCommitMessage(message, EntryTypeAssistant)
		assistant.ID = entryID
		assistant.RunID = batch.RunID
		assistant.WorkspaceID = s.workspaceID
		assistant.AssistantTranscriptOrdinal = &ordinal
		assistant.TranscriptIndex = &index
		assistant.Timestamp = time.Now().UTC()
	}

	intentRows := make([]SessionEntry, 0, len(batch.Intents))
	dispatchActions := make([]WorkspaceDispatchAction, 0, len(batch.Intents))
	seenCalls := make(map[string]struct{}, len(batch.Intents))
	seenActionIDs := make(map[string]struct{}, len(batch.Intents))
	lastCallPosition := -1
	existingIntentCount := 0
	for _, intent := range batch.Intents {
		if intent.SessionID != s.sessionID || intent.WorkspaceID != s.workspaceID || intent.RunID != batch.RunID || intent.ActionBatchID != batch.ActionBatchID || intent.AssistantTranscriptOrdinal != batch.AssistantTranscriptOrdinal || intent.OwnerGeneration != s.ownerGen {
			return errors.New("session: intent identity does not match its run, session, workspace, batch, occurrence, or owner")
		}
		if intent.ToolCallID == "" || intent.ActionID == "" {
			return errors.New("session: reserved intent requires tool-call and action IDs")
		}
		call, ok := toolCalls[intent.ToolCallID]
		if !ok || call.Name != intent.ToolIdentity.ToolName() {
			return fmt.Errorf("session: intent %q does not match a tool call in its assistant message", intent.ToolCallID)
		}
		position := callPosition[intent.ToolCallID]
		if position <= lastCallPosition {
			return errors.New("session: reserved intents are not in canonical assistant tool-call order")
		}
		lastCallPosition = position
		if _, exists := seenCalls[intent.ToolCallID]; exists {
			return fmt.Errorf("session: intent batch repeats tool call %q", intent.ToolCallID)
		}
		if _, exists := seenActionIDs[intent.ActionID]; exists {
			return fmt.Errorf("session: intent batch repeats action ID %q", intent.ActionID)
		}
		seenCalls[intent.ToolCallID] = struct{}{}
		seenActionIDs[intent.ActionID] = struct{}{}
		identity := intent.ToolIdentity.Snapshot()
		if err := validateToolIdentitySnapshot(&identity, call.Name); err != nil {
			return fmt.Errorf("session: invalid trusted identity for intent %q: %w", intent.ToolCallID, err)
		}
		argumentDigest := hex.EncodeToString(intent.ArgumentDigest[:])
		schemaDigest := hex.EncodeToString(intent.SchemaDigest[:])
		if !isHexDigest(argumentDigest) || !isHexDigest(schemaDigest) {
			return errors.New("session: intent has malformed argument or schema digest")
		}
		effects, err := normalizeEffectFacts(intent.EffectSet.Values())
		if err != nil {
			return fmt.Errorf("session: invalid trusted effects for intent %q: %w", intent.ToolCallID, err)
		}
		if intent.ScopedReadProof != nil {
			workspaceID, rootErr := WorkspaceIDForCanonicalRoot(batch.CanonicalWorkspaceRoot)
			if rootErr != nil || workspaceID != s.workspaceID || !intent.ScopedReadProof.MatchesAction(
				intent.ToolIdentity, intent.EffectSet, intent.SchemaDigest, intent.ArgumentDigest,
				intent.ToolCallID, batch.CanonicalWorkspaceRoot,
			) {
				return fmt.Errorf("session: scoped read proof does not match prepared intent %q", intent.ToolCallID)
			}
		}
		if intent.ManagedDelegationProof != nil && !intent.ManagedDelegationProof.MatchesAction(intent.ToolIdentity, intent.EffectSet, intent.SchemaDigest, intent.ArgumentDigest, intent.ToolCallID, batch.CanonicalWorkspaceRoot) {
			return errors.New("session: managed delegation proof differs from prepared intent")
		}
		entryID, err := stableRunCommitEntryID("intent", struct {
			SessionID string
			Workspace string
			RunID     string
			Ordinal   uint64
			CallID    string
			BatchID   string
			ActionID  string
			Owner     string
		}{intent.SessionID, intent.WorkspaceID, intent.RunID, intent.AssistantTranscriptOrdinal, intent.ToolCallID, intent.ActionBatchID, intent.ActionID, intent.OwnerGeneration})
		if err != nil {
			return err
		}
		ordinal := intent.AssistantTranscriptOrdinal
		row := SessionEntry{
			ID: entryID, Type: EntryTypeTool, RunID: intent.RunID, WorkspaceID: intent.WorkspaceID,
			ActionBatchID: intent.ActionBatchID, ActionID: intent.ActionID, OwnerGeneration: intent.OwnerGeneration,
			ActionState: ActionStateIntent, AssistantTranscriptOrdinal: &ordinal,
			ToolCallID: intent.ToolCallID, ToolName: call.Name, ArgumentDigest: argumentDigest,
			SchemaDigest: schemaDigest, ToolIdentity: &identity, EffectSet: effects,
			Timestamp: time.Now().UTC(),
		}
		existing, exists, err := s.actionRowLocked(row)
		if err != nil {
			return err
		}
		if exists {
			if !sameIntentPayload(existing, row) {
				return fmt.Errorf("session: intent %q conflicts with its durable row", intent.ActionID)
			}
			existingIntentCount++
		} else {
			intentRows = append(intentRows, row)
		}
		dispatchActions = append(dispatchActions, WorkspaceDispatchAction{
			Action: PendingActionRef{
				SessionID: intent.SessionID, WorkspaceID: intent.WorkspaceID, RunID: intent.RunID,
				ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: intent.AssistantTranscriptOrdinal,
				ToolCallID: intent.ToolCallID, ActionID: intent.ActionID,
			},
			ToolIdentity: &identity, EffectSet: append([]tools.TrustedToolEffect(nil), effects...),
			ScopedReadProof:        intent.ScopedReadProof,
			ManagedDelegationProof: intent.ManagedDelegationProof,
		})
	}
	if existingIntentCount != 0 && existingIntentCount != len(batch.Intents) {
		return errors.New("session: intent batch is partially present; refusing to publish a dispatch marker")
	}
	if !exists {
		intentRows = append([]SessionEntry{assistant}, intentRows...)
	}
	if len(intentRows) > 0 {
		if err := s.appendAndRefreshLocked(ctx, intentRows...); err != nil {
			return err
		}
	}
	if len(dispatchActions) == 0 {
		return nil
	}
	dispatchBatch := WorkspaceDispatchBatch{
		WorkspaceID: s.workspaceID, SessionID: s.sessionID, RunID: batch.RunID,
		ActionBatchID: batch.ActionBatchID, Actions: dispatchActions,
		CanonicalWorkspaceRoot: batch.CanonicalWorkspaceRoot,
	}
	result, definiteNoIO, err := s.index.beginDispatchBatch(ctx, dispatchBatch)
	if err != nil {
		if definiteNoIO {
			if closeErr := s.closeRejectedBatchLocked(ctx, batch); closeErr != nil {
				return errors.Join(fmt.Errorf("session: commit workspace dispatch marker: %w", err), closeErr)
			}
		}
		return fmt.Errorf("session: commit workspace dispatch marker: %w", err)
	}
	if result != WorkspaceDispatchCommittedNow {
		return fmt.Errorf("session: workspace dispatch marker state %d does not grant a new dispatch permit", result)
	}
	return nil
}

func (s *SessionRunCommitSink) RecordExecution(ctx context.Context, execution core.ActionExecution) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateCallContextLocked(ctx, execution.RunID); err != nil {
		return err
	}
	// A durable terminal fences ordinary writes. Only a positive proof for an
	// already-receipted action may pass this gate for bounded owner cleanup.
	lateAfterTerminal := s.terminalID != ""
	if lateAfterTerminal && execution.CompletionProof == "" {
		return s.rejectAfterTerminalLocked()
	}
	if execution.ToolCallID == "" || execution.ActionID == "" || execution.OwnerGeneration == "" {
		return errors.New("session: execution observation requires a full reserved action identity")
	}
	if execution.OwnerGeneration != s.ownerGen {
		return errors.New("session: execution observation owner generation differs from the run owner")
	}
	if execution.DispatchState != core.DispatchStateDispatched && execution.DispatchState != core.DispatchStateNotDispatched {
		return fmt.Errorf("session: execution observation has invalid dispatch state %q", execution.DispatchState)
	}
	if !validRunExecutionState(execution.ResourceState) {
		return fmt.Errorf("session: execution observation has invalid resource state %q", execution.ResourceState)
	}
	if !validCompletionProof(execution.CompletionProof) {
		return fmt.Errorf("session: execution observation has invalid completion proof %q", execution.CompletionProof)
	}
	if execution.CompletionProof != "" && execution.DispatchState != core.DispatchStateDispatched {
		return errors.New("session: positive completion proof requires a dispatched action")
	}
	var identity *core.ExecutionIdentity
	if execution.ExecutionIdentity != nil {
		if err := execution.ExecutionIdentity.Validate(); err != nil {
			return fmt.Errorf("session: invalid execution identity: %w", err)
		}
		copy := *execution.ExecutionIdentity
		identity = &copy
	}
	intent, err := s.findActionIntentLocked(execution.RunID, execution.AssistantTranscriptOrdinal, execution.ToolCallID, execution.ActionID, execution.OwnerGeneration)
	if err != nil {
		return err
	}
	if lateAfterTerminal {
		if execution.DispatchState != core.DispatchStateDispatched || execution.ResourceState != core.RunExecutionQuiescent {
			return errors.New("session: post-terminal execution requires a positive quiescent completion observation")
		}
		ref := refFromEntry(intent, s.sessionID)
		receipt, err := s.findReceiptForIntentLocked(ref, intent)
		if err != nil {
			return fmt.Errorf("session: post-terminal completion requires its durable receipt: %w", err)
		}
		if receipt.DispatchState != core.DispatchStateDispatched || receipt.Seq == 0 {
			return errors.New("session: post-terminal completion requires a synced dispatched receipt")
		}
		if err := s.validatePostTerminalActionLocked(execution.RunID, receipt); err != nil {
			return err
		}
	}
	row := SessionEntry{
		Type: EntryTypeTool, RunID: execution.RunID, WorkspaceID: intent.WorkspaceID,
		ActionBatchID: intent.ActionBatchID, ActionID: intent.ActionID,
		OwnerGeneration: intent.OwnerGeneration, ActionState: ActionStateExecution,
		AssistantTranscriptOrdinal: cloneUint64Pointer(intent.AssistantTranscriptOrdinal),
		ToolCallID:                 intent.ToolCallID, ToolName: intent.ToolName,
		DispatchState: execution.DispatchState, ResourceState: execution.ResourceState,
		ExecutionIdentity: identity, CompletionProof: execution.CompletionProof,
	}
	row.ID, err = stableRunCommitEntryID("execution", struct {
		ActionRef       PendingActionRef
		DispatchState   core.DispatchState
		ResourceState   core.RunExecutionState
		Identity        *core.ExecutionIdentity
		CompletionProof string
	}{refFromEntry(row, s.sessionID), execution.DispatchState, execution.ResourceState, identity, string(execution.CompletionProof)})
	if err != nil {
		return err
	}
	if lateAfterTerminal && row.CompletionProof != "" {
		for _, existing := range s.branchRows {
			if existing.RecordVersion != 1 || existing.ActionState != ActionStateExecution || existing.CompletionProof == "" || !sameActionReference(refFromEntry(intent, s.sessionID), refFromEntry(existing, s.sessionID)) {
				continue
			}
			if sameExecutionObservation(existing, row) {
				return nil
			}
			return errors.New("session: post-terminal completion conflicts with an existing positive proof")
		}
	}
	for _, existing := range s.branchRows {
		if existing.ID != row.ID {
			continue
		}
		if !sameExecutionObservation(existing, row) {
			return fmt.Errorf("session: execution row ID %q conflicts with its durable payload", row.ID)
		}
		return nil
	}
	_, err = s.appendAndRefreshRowsLocked(ctx, row)
	return err
}

func (s *SessionRunCommitSink) RecordReceipt(ctx context.Context, receipt core.RunToolReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateCallContextLocked(ctx, receipt.RunID); err != nil {
		return err
	}
	return s.recordReceiptLocked(ctx, receipt)
}

func (s *SessionRunCommitSink) recordReceiptLocked(ctx context.Context, receipt core.RunToolReceipt) error {
	if err := s.rejectAfterTerminalLocked(); err != nil {
		return err
	}
	if receipt.ToolCallID == "" {
		return errors.New("session: tool receipt requires a tool-call ID")
	}
	if receipt.DispatchState != core.DispatchStateDispatched && receipt.DispatchState != core.DispatchStateNotDispatched {
		return fmt.Errorf("session: tool receipt has invalid dispatch state %q", receipt.DispatchState)
	}
	if !validRunReceiptOutcome(receipt.ReceiptOutcome) {
		return fmt.Errorf("session: tool receipt has invalid outcome %q", receipt.ReceiptOutcome)
	}
	message := cloneRunCommitMessage(receipt.Message)
	if message.Role != "tool" || message.ToolCallID != receipt.ToolCallID || message.ToolName == "" {
		return errors.New("session: tool receipt message does not match its occurrence")
	}
	if len(message.ToolCalls) != 0 || message.InputKind != "" {
		return errors.New("session: tool receipt message contains input or assistant call fields")
	}
	if message.ReceiptOutcome != "" && message.ReceiptOutcome != receipt.ReceiptOutcome {
		return errors.New("session: tool receipt outcome conflicts with its message")
	}
	message.ReceiptOutcome = receipt.ReceiptOutcome
	assistant, exists, err := s.assistantOccurrenceLocked(receipt.RunID, receipt.AssistantTranscriptOrdinal)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("session: tool receipt occurrence %q has no durable assistant row", receipt.ToolCallID)
	}
	toolName, exists := assistantToolName(assistant, receipt.ToolCallID)
	if !exists || toolName != message.ToolName {
		return errors.New("session: tool receipt does not match the durable assistant tool call")
	}
	var intent SessionEntry
	if receipt.ActionID == "" {
		if receipt.DispatchState != core.DispatchStateNotDispatched {
			return errors.New("session: unreserved tool receipt must prove it was not dispatched")
		}
		if receipt.ReceiptOutcome != core.ReceiptOutcomeError && receipt.ReceiptOutcome != core.ReceiptOutcomeInterrupted {
			return errors.New("session: unreserved tool receipt must have a known denial outcome")
		}
		if _, found, findErr := s.findOccurrenceIntentLocked(receipt.RunID, receipt.AssistantTranscriptOrdinal, receipt.ToolCallID); findErr != nil {
			return findErr
		} else if found {
			return errors.New("session: unreserved receipt conflicts with a durable reserved intent")
		}
	} else {
		if receipt.DispatchState == core.DispatchStateNotDispatched && receipt.ReceiptOutcome != core.ReceiptOutcomeError && receipt.ReceiptOutcome != core.ReceiptOutcomeInterrupted {
			return errors.New("session: not-dispatched tool receipt must have a known denial outcome")
		}
		intent, err = s.findActionIntentLocked(receipt.RunID, receipt.AssistantTranscriptOrdinal, receipt.ToolCallID, receipt.ActionID, s.ownerGen)
		if err != nil {
			return err
		}
	}
	transcriptRows, err := transcriptRowsByIndex(s.branchRows, receipt.RunID)
	if err != nil {
		return err
	}
	if err := validateTranscriptRowsInPhysicalOrder(s.branchRows, receipt.RunID); err != nil {
		return err
	}
	if existing, found := transcriptRows[receipt.TranscriptIndex]; found {
		candidate := s.receiptEntry(receipt, message, intent)
		candidate.ID, err = stableRunCommitEntryID("receipt", struct {
			RunID string
			Index uint64
		}{receipt.RunID, receipt.TranscriptIndex})
		if err != nil {
			return err
		}
		if !sameReceiptRow(existing, candidate) {
			return fmt.Errorf("session: run %q transcript index %d conflicts with its durable receipt", receipt.RunID, receipt.TranscriptIndex)
		}
		return nil
	}
	nextIndex, err := nextTranscriptIndex(transcriptRows)
	if err != nil {
		return err
	}
	if receipt.TranscriptIndex != nextIndex {
		return fmt.Errorf("session: run %q receipt transcript index %d is not the next index %d", receipt.RunID, receipt.TranscriptIndex, nextIndex)
	}
	row := s.receiptEntry(receipt, message, intent)
	row.ID, err = stableRunCommitEntryID("receipt", struct {
		RunID string
		Index uint64
	}{receipt.RunID, receipt.TranscriptIndex})
	if err != nil {
		return err
	}
	index, ordinal := receipt.TranscriptIndex, receipt.AssistantTranscriptOrdinal
	row.TranscriptIndex = &index
	row.AssistantTranscriptOrdinal = &ordinal
	_, err = s.appendAndRefreshRowsLocked(ctx, row)
	return err
}

func (s *SessionRunCommitSink) ResolveExecution(ctx context.Context, resolution core.ExecutionResolution) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateCallContextLocked(ctx, resolution.RunID); err != nil {
		return err
	}
	if resolution.ActionID == "" {
		return errors.New("session: execution resolution requires an action ID")
	}
	intent, err := s.findIntentByActionIDLocked(resolution.RunID, resolution.ActionID, s.ownerGen)
	if err != nil {
		return err
	}
	ref := refFromEntry(intent, s.sessionID)
	receipt, err := s.findReceiptForIntentLocked(ref, intent)
	if err != nil {
		return err
	}
	return s.resolveExecutionLocked(ctx, resolution, ref, intent, receipt, false)
}

func (s *SessionRunCommitSink) resolveExecutionLocked(
	ctx context.Context,
	resolution core.ExecutionResolution,
	ref PendingActionRef,
	intent SessionEntry,
	receipt SessionEntry,
	allowOperatorReconciliation bool,
) error {
	// Keep the terminal result immutable while allowing its exact local cleanup
	// or explicitly reconciled proof to finish the matching workspace marker.
	lateAfterTerminal := s.terminalID != ""
	if lateAfterTerminal && resolution.Evidence != core.ResolutionLocalResourcesStopped &&
		!(allowOperatorReconciliation && resolution.Evidence == core.ResolutionOperatorReconciled) {
		return s.rejectAfterTerminalLocked()
	}
	if receipt.RecordVersion != 1 || receipt.Seq == 0 {
		return errors.New("session: execution resolution requires a synced receipt on the active branch")
	}
	if lateAfterTerminal {
		if receipt.DispatchState != core.DispatchStateDispatched {
			return errors.New("session: post-terminal resolution requires a dispatched receipt")
		}
		if err := s.validatePostTerminalActionLocked(resolution.RunID, receipt); err != nil {
			return err
		}
	}
	if resolution.Evidence == core.ResolutionRemoteTerminal {
		return errors.New("session: remote terminal resolution requires unsupported typed protocol evidence")
	}
	resolvedEvidence, err := s.resolveEvidenceReferenceLocked(resolution, ref, intent, receipt)
	if err != nil {
		return err
	}
	resolvedEvidence.RunID = resolution.RunID
	resolvedEvidence.ActionID = resolution.ActionID
	resolution = resolvedEvidence
	entryID, err := stableRunCommitEntryID("resolution", struct {
		ActionRef       PendingActionRef
		OwnerGeneration string
	}{ref, intent.OwnerGeneration})
	if err != nil {
		return err
	}
	row := SessionEntry{
		ID: entryID, Type: EntryTypeCheckpoint, RunID: ref.RunID, WorkspaceID: ref.WorkspaceID,
		ActionBatchID: ref.ActionBatchID, ActionID: ref.ActionID, OwnerGeneration: intent.OwnerGeneration,
		AssistantTranscriptOrdinal: cloneUint64Pointer(intent.AssistantTranscriptOrdinal),
		ToolCallID:                 ref.ToolCallID, ToolName: intent.ToolName,
		CheckpointKind: CheckpointExecutionResolution, ExecutionResolution: &resolution,
	}
	var durableResolution *SessionEntry
	for index := range s.branchRows {
		existing := &s.branchRows[index]
		if existing.RecordVersion != 1 || existing.CheckpointKind != CheckpointExecutionResolution || !sameActionReference(ref, refFromEntry(*existing, s.sessionID)) {
			continue
		}
		if existing.OwnerGeneration != intent.OwnerGeneration {
			return errors.New("session: execution resolution owner generation differs from its durable intent")
		}
		if durableResolution != nil {
			return errors.New("session: action has multiple durable execution-resolution checkpoints")
		}
		durableResolution = existing
	}
	if durableResolution == nil {
		canonical, appendErr := s.appendAndRefreshRowsLocked(ctx, row)
		if appendErr != nil {
			return appendErr
		}
		if len(canonical) != 1 || canonical[0].ID != row.ID || canonical[0].Seq == 0 {
			return errors.New("session: durable writer returned an invalid resolution checkpoint identity")
		}
		durableResolution = &canonical[0]
	} else {
		if durableResolution.ID != row.ID || durableResolution.ExecutionResolution == nil || *durableResolution.ExecutionResolution != resolution {
			return errors.New("session: execution resolution conflicts with the durable checkpoint")
		}
	}
	validated := ValidatedExecutionResolution{
		Action: ref, CheckpointEntryID: durableResolution.ID, CheckpointSeq: durableResolution.Seq,
		Resolution: resolution,
	}
	if err := s.index.ApplyDurableResolutions(ctx, []ValidatedExecutionResolution{validated}); err != nil {
		return fmt.Errorf("session: apply synced execution resolution to workspace index: %w", err)
	}
	return nil
}

func (s *SessionRunCommitSink) CommitCompaction(ctx context.Context, projection core.CompactionProjection) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateCallContextLocked(ctx, core.RunIDFromContext(ctx)); err != nil {
		return err
	}
	if err := s.rejectAfterTerminalLocked(); err != nil {
		return err
	}
	return s.commitCompactionLocked(ctx, core.RunIDFromContext(ctx), projection)
}

func (s *SessionRunCommitSink) CommitTerminal(ctx context.Context, terminal core.RunTerminal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateCallContextLocked(ctx, terminal.RunID); err != nil {
		return err
	}
	if terminal.RunID == "" || core.RunIDFromContext(ctx) != terminal.RunID {
		return errors.New("session: terminal checkpoint does not match the owned run")
	}
	terminalID := string(terminal.TerminalCheckpointID)
	if terminalID == "" || strings.ContainsRune(terminalID, '\x00') {
		return errors.New("session: terminal checkpoint requires a non-empty NUL-free ID")
	}
	if !validRunOutcome(terminal.Outcome) {
		return fmt.Errorf("session: terminal checkpoint has invalid run outcome %q", terminal.Outcome)
	}
	if len(terminal.Transcript) == 0 {
		return errors.New("session: terminal checkpoint requires the canonical run transcript")
	}
	if s.terminalID != "" && s.terminalID != terminalID {
		return errors.New("session: run already has a different terminal checkpoint")
	}
	rowsByIndex, err := transcriptRowsByIndex(s.branchRows, terminal.RunID)
	if err != nil {
		return err
	}
	if err := validateTranscriptRowsInPhysicalOrder(s.branchRows, terminal.RunID); err != nil {
		return err
	}
	existingRows := make([]SessionEntry, len(rowsByIndex))
	for index := uint64(0); index < uint64(len(rowsByIndex)); index++ {
		row, exists := rowsByIndex[index]
		if !exists {
			return fmt.Errorf("session: terminal transcript has a durable hole at index %d", index)
		}
		existingRows[index] = row
	}
	if len(existingRows) > len(terminal.Transcript) {
		return errors.New("session: durable run transcript is longer than the terminal transcript")
	}
	for index, row := range existingRows {
		message := cloneRunCommitMessage(terminal.Transcript[index].Message)
		if !durableRowMatchesMessage(row, message) {
			return fmt.Errorf("session: terminal transcript conflicts with durable row at index %d", index)
		}
	}
	terminalRow, hasTerminal, err := s.findTerminalCheckpointLocked(terminal.RunID)
	if err != nil {
		return err
	}
	if err := s.verifyTerminalProjectionsLocked(terminal.RunID, terminal.Projections); err != nil {
		return err
	}
	if hasTerminal {
		if terminalRow.ID != terminalID || terminalRow.RunOutcome != terminal.Outcome {
			return errors.New("session: terminal checkpoint conflicts with its durable row")
		}
		if len(existingRows) != len(terminal.Transcript) {
			return errors.New("session: terminal checkpoint exists before the complete transcript prefix")
		}
		s.terminalID = terminalID
		return nil
	}
	rows := make([]SessionEntry, 0, len(terminal.Transcript)-len(existingRows)+1)
	for index := len(existingRows); index < len(terminal.Transcript); index++ {
		message := cloneRunCommitMessage(terminal.Transcript[index].Message)
		if message.Role != "assistant" || len(message.ToolCalls) > 0 || message.ToolCallID != "" || message.ToolName != "" || message.InputKind != "" || message.ReceiptOutcome != "" {
			return fmt.Errorf("session: terminal transcript suffix at index %d is not a plain assistant message already supported by durable input/tool commits", index)
		}
		transcriptIndex := uint64(index)
		row := sessionEntryFromRunCommitMessage(message, EntryTypeAssistant)
		row.RunID = terminal.RunID
		row.WorkspaceID = s.workspaceID
		row.TranscriptIndex = &transcriptIndex
		row.ID, err = stableRunCommitEntryID("assistant-terminal", struct {
			RunID string
			Index uint64
		}{terminal.RunID, transcriptIndex})
		if err != nil {
			return err
		}
		rows = append(rows, row)
	}
	for _, row := range s.allRows {
		if row.ID == terminalID {
			return fmt.Errorf("session: terminal checkpoint ID %q is already used by another durable row", terminalID)
		}
	}
	for _, row := range rows {
		if row.ID == terminalID {
			return errors.New("session: terminal checkpoint ID conflicts with a terminal transcript row")
		}
	}
	checkpoint := SessionEntry{
		ID: terminalID, Type: EntryTypeCheckpoint, RunID: terminal.RunID,
		WorkspaceID: s.workspaceID, CheckpointKind: CheckpointRunTerminal,
		RunOutcome: terminal.Outcome,
		Generation: terminal.Generation,
	}
	if metadata, ok := core.RunGenerationMetadataFromContext(ctx); ok {
		checkpoint.Generation = &metadata
	} else if terminal.Generation != nil {
		metadata := *terminal.Generation
		checkpoint.Generation = &metadata
	}
	rows = append(rows, checkpoint)
	canonical, err := s.appendAndRefreshRowsLocked(ctx, rows...)
	if err != nil {
		return err
	}
	if len(canonical) != len(rows) || canonical[len(canonical)-1].ID != terminalID || canonical[len(canonical)-1].Seq == 0 {
		return errors.New("session: durable writer returned an invalid terminal checkpoint identity")
	}
	s.terminalID = terminalID
	return nil
}

func (s *SessionRunCommitSink) validateCallContextLocked(ctx context.Context, payloadRunID string) error {
	if s.closed {
		return errors.New("session: run-commit lease is closed")
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	boundSink, binding, ok := core.RunCommitFromContext(ctx)
	if !ok || boundSink != s {
		return errors.New("session: run-commit call context does not bind this sink")
	}
	if err := binding.Validate(); err != nil {
		return err
	}
	if binding.SessionID != s.sessionID || binding.WorkspaceID != s.workspaceID {
		return errors.New("session: run-commit call binding differs from the prepared session or workspace")
	}
	runID := core.RunIDFromContext(ctx)
	if runID == "" || payloadRunID == "" || runID != payloadRunID {
		return errors.New("session: run-commit payload does not match the owned run ID")
	}
	if s.runID == "" {
		s.runID = runID
		s.ownerGen = binding.OwnerGeneration
	} else if s.runID != runID || s.ownerGen != binding.OwnerGeneration {
		return errors.New("session: run-commit sink was reused by another run owner")
	}
	return nil
}

func (s *SessionRunCommitSink) rejectAfterTerminalLocked() error {
	if s.terminalID != "" {
		return errors.New("session: run already has a durable terminal checkpoint")
	}
	return nil
}

func (s *SessionRunCommitSink) validatePostTerminalActionLocked(runID string, receipt SessionEntry) error {
	terminal, exists, err := s.findTerminalCheckpointLocked(runID)
	if err != nil {
		return err
	}
	if !exists || terminal.ID != s.terminalID || terminal.Seq == 0 {
		return errors.New("session: post-terminal action proof requires this run's synced terminal checkpoint")
	}
	if receipt.RecordVersion != 1 || receipt.Type != EntryTypeTool || receipt.ActionState != "" || receipt.RunID != runID || receipt.Seq == 0 || receipt.Seq >= terminal.Seq {
		return errors.New("session: post-terminal action proof requires an earlier synced receipt")
	}
	return nil
}

func (s *SessionRunCommitSink) commitCompactionLocked(ctx context.Context, runID string, projection core.CompactionProjection) error {
	row, err := s.compactionEntryLocked(runID, projection)
	if err != nil {
		return err
	}
	for _, existing := range s.branchRows {
		if existing.ID == row.ID {
			if !sameCompactionRow(existing, row) {
				return fmt.Errorf("session: compaction row ID %q conflicts with its durable payload", row.ID)
			}
			return nil
		}
		if existing.RecordVersion == 1 && existing.Type == EntryTypeCompaction && existing.RunID == runID && existing.CompactedThroughEntryID == row.CompactedThroughEntryID {
			return errors.New("session: run already has a different compaction at this source boundary")
		}
	}
	_, err = s.appendAndRefreshRowsLocked(ctx, row)
	return err
}

func (s *SessionRunCommitSink) compactionEntryLocked(runID string, projection core.CompactionProjection) (SessionEntry, error) {
	if runID == "" || projection.ExpectedHistoryRevision != s.baseView.Revision {
		return SessionEntry{}, errors.New("session: compaction projection has a stale run-start history revision")
	}
	if strings.TrimSpace(projection.Summary) == "" {
		return SessionEntry{}, errors.New("session: compaction projection requires a non-empty summary")
	}
	priorCount := projection.Through.PriorHistoryCount
	transcriptCount := projection.Through.TranscriptCount
	var boundaryID string
	if transcriptCount == 0 {
		if priorCount == 0 || priorCount > uint64(len(s.baseView.SourceEntryIDs)) {
			return SessionEntry{}, errors.New("session: compaction cursor does not map to a run-start history source")
		}
		boundaryID = s.baseView.SourceEntryIDs[priorCount-1]
	} else {
		if priorCount != uint64(len(s.baseView.Messages)) {
			return SessionEntry{}, errors.New("session: compaction cursor does not preserve the complete prior-history prefix")
		}
		transcriptRows, err := transcriptRowsByIndex(s.branchRows, runID)
		if err != nil {
			return SessionEntry{}, err
		}
		if err := validateTranscriptRowsInPhysicalOrder(s.branchRows, runID); err != nil {
			return SessionEntry{}, err
		}
		if transcriptCount > uint64(len(transcriptRows)) {
			return SessionEntry{}, errors.New("session: compaction cursor extends beyond the durable run transcript")
		}
		boundary, exists := transcriptRows[transcriptCount-1]
		if !exists {
			return SessionEntry{}, errors.New("session: compaction cursor has no exact durable transcript boundary")
		}
		boundaryID = boundary.ID
	}
	boundaryPosition := -1
	for index, entry := range s.branchRows {
		if entry.ID == boundaryID {
			boundaryPosition = index
			break
		}
	}
	if boundaryPosition < 0 {
		return SessionEntry{}, errors.New("session: compaction boundary is not on the selected active branch")
	}
	if err := s.validateCompactionBoundaryLocked(boundaryPosition); err != nil {
		return SessionEntry{}, err
	}
	row := SessionEntry{
		Type: EntryTypeCompaction, Content: projection.Summary, Summary: projection.Summary,
		RunID: runID, WorkspaceID: s.workspaceID, CompactedThroughEntryID: boundaryID,
	}
	entryID, err := stableRunCommitEntryID("compaction", struct {
		RunID       string
		Revision    string
		PriorCount  uint64
		Transcript  uint64
		BoundaryID  string
		SummaryHash string
	}{runID, projection.ExpectedHistoryRevision, priorCount, transcriptCount, boundaryID, hashText(projection.Summary)})
	if err != nil {
		return SessionEntry{}, err
	}
	row.ID = entryID
	return row, nil
}

func (s *SessionRunCommitSink) validateCompactionBoundaryLocked(boundaryPosition int) error {
	positions := make(map[string]int, len(s.branchRows))
	for index, row := range s.branchRows {
		positions[row.ID] = index
	}
	for _, assistant := range s.branchRows {
		if assistant.RecordVersion != 1 || assistant.Type != EntryTypeAssistant || len(assistant.ToolCalls) == 0 {
			continue
		}
		assistantPosition := positions[assistant.ID]
		lastReceiptPosition := assistantPosition
		closed := true
		ordinal := *assistant.AssistantTranscriptOrdinal
		for _, call := range assistant.ToolCalls {
			found := false
			for _, candidate := range s.branchRows {
				if candidate.RecordVersion == 1 && candidate.Type == EntryTypeTool && candidate.ActionState == "" && candidate.RunID == assistant.RunID && candidate.AssistantTranscriptOrdinal != nil && *candidate.AssistantTranscriptOrdinal == ordinal && candidate.ToolCallID == call.ID {
					if !found || positions[candidate.ID] > lastReceiptPosition {
						lastReceiptPosition = positions[candidate.ID]
					}
					found = true
				}
			}
			if !found {
				closed = false
			}
		}
		if boundaryPosition >= assistantPosition && (!closed || boundaryPosition < lastReceiptPosition) {
			return fmt.Errorf("session: compaction boundary splits or includes an open assistant tool-call group at %q", assistant.ID)
		}
	}
	return nil
}

func (s *SessionRunCommitSink) verifyTerminalProjectionsLocked(runID string, projections []core.CompactionProjection) error {
	// The terminal payload is a complete snapshot, not a chance to append or
	// silently omit the run's already-durable compaction projections.
	durable := make([]SessionEntry, 0)
	for _, row := range s.branchRows {
		if row.RecordVersion == 1 && row.Type == EntryTypeCompaction && row.RunID == runID {
			durable = append(durable, row)
		}
	}
	if len(projections) != len(durable) {
		return fmt.Errorf("session: terminal projection count %d differs from %d durable run projections", len(projections), len(durable))
	}
	seen := make(map[string]struct{}, len(projections))
	seenBoundaries := make(map[string]struct{}, len(projections))
	for index, projection := range projections {
		candidate, err := s.compactionEntryLocked(runID, projection)
		if err != nil {
			return fmt.Errorf("session: validate terminal compaction projection: %w", err)
		}
		if _, duplicate := seen[candidate.ID]; duplicate {
			return errors.New("session: terminal repeats a compaction projection")
		}
		if _, duplicate := seenBoundaries[candidate.CompactedThroughEntryID]; duplicate {
			return errors.New("session: terminal repeats a compaction source boundary")
		}
		seen[candidate.ID] = struct{}{}
		seenBoundaries[candidate.CompactedThroughEntryID] = struct{}{}
		if candidate.ID != durable[index].ID || !sameCompactionRow(durable[index], candidate) {
			return fmt.Errorf("session: terminal projection at position %d does not match its durable ordered projection", index)
		}
	}
	return nil
}

func (s *SessionRunCommitSink) findTerminalCheckpointLocked(runID string) (SessionEntry, bool, error) {
	var match SessionEntry
	found := false
	for _, row := range s.branchRows {
		if row.RecordVersion != 1 || row.Type != EntryTypeCheckpoint || row.CheckpointKind != CheckpointRunTerminal || row.RunID != runID {
			continue
		}
		if found {
			return SessionEntry{}, false, fmt.Errorf("session: run %q has multiple terminal checkpoints", runID)
		}
		match = *row.clone()
		found = true
	}
	return match, found, nil
}

func sameCompactionRow(existing, candidate SessionEntry) bool {
	return existing.ID == candidate.ID && existing.Type == EntryTypeCompaction && existing.RunID == candidate.RunID &&
		existing.WorkspaceID == candidate.WorkspaceID && existing.Summary == candidate.Summary &&
		existing.Content == candidate.Content && existing.CompactedThroughEntryID == candidate.CompactedThroughEntryID
}

func hashText(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func validRunOutcome(outcome core.RunOutcome) bool {
	switch outcome {
	case core.RunOutcomeSuccess, core.RunOutcomeError, core.RunOutcomeCancelled, core.RunOutcomeUnknown:
		return true
	default:
		return false
	}
}

func (s *SessionRunCommitSink) assistantOccurrenceLocked(runID string, ordinal uint64) (SessionEntry, bool, error) {
	var match SessionEntry
	found := false
	for _, row := range s.branchRows {
		if row.RecordVersion != 1 || row.Type != EntryTypeAssistant || row.RunID != runID || row.AssistantTranscriptOrdinal == nil || *row.AssistantTranscriptOrdinal != ordinal || len(row.ToolCalls) == 0 {
			continue
		}
		if found {
			return SessionEntry{}, false, fmt.Errorf("session: run %q has multiple assistant rows at ordinal %d", runID, ordinal)
		}
		match = *row.clone()
		found = true
	}
	return match, found, nil
}

func (s *SessionRunCommitSink) actionRowLocked(candidate SessionEntry) (SessionEntry, bool, error) {
	candidateRef := refFromEntry(candidate, s.sessionID)
	var match SessionEntry
	found := false
	for _, row := range s.branchRows {
		if row.RecordVersion != 1 || row.ActionState != candidate.ActionState {
			continue
		}
		if row.ID == candidate.ID && !sameActionReference(candidateRef, refFromEntry(row, s.sessionID)) {
			return SessionEntry{}, false, fmt.Errorf("session: stable action row ID %q conflicts with its full action reference", candidate.ID)
		}
		if !sameActionReference(candidateRef, refFromEntry(row, s.sessionID)) || row.OwnerGeneration != candidate.OwnerGeneration {
			continue
		}
		if row.ID != candidate.ID {
			return SessionEntry{}, false, fmt.Errorf("session: action reference has conflicting stable row IDs %q and %q", row.ID, candidate.ID)
		}
		if found {
			return SessionEntry{}, false, fmt.Errorf("session: action reference repeats journal row %q", candidate.ActionID)
		}
		match = *row.clone()
		found = true
	}
	return match, found, nil
}

func (s *SessionRunCommitSink) findOccurrenceIntentLocked(runID string, ordinal uint64, toolCallID string) (SessionEntry, bool, error) {
	var match SessionEntry
	found := false
	for _, row := range s.branchRows {
		if row.RecordVersion != 1 || row.Type != EntryTypeTool || row.ActionState != ActionStateIntent || row.RunID != runID || row.AssistantTranscriptOrdinal == nil || *row.AssistantTranscriptOrdinal != ordinal || row.ToolCallID != toolCallID {
			continue
		}
		if found {
			return SessionEntry{}, false, fmt.Errorf("session: occurrence %q has multiple durable intents", toolCallID)
		}
		match = *row.clone()
		found = true
	}
	return match, found, nil
}

func (s *SessionRunCommitSink) findActionIntentLocked(runID string, ordinal uint64, toolCallID, actionID, ownerGeneration string) (SessionEntry, error) {
	intent, found, err := s.findOccurrenceIntentLocked(runID, ordinal, toolCallID)
	if err != nil {
		return SessionEntry{}, err
	}
	if !found {
		return SessionEntry{}, fmt.Errorf("session: action %q has no durable intent", actionID)
	}
	if intent.ActionID != actionID || intent.OwnerGeneration != ownerGeneration || intent.WorkspaceID != s.workspaceID || intent.ActionBatchID == "" {
		return SessionEntry{}, fmt.Errorf("session: action %q does not match the full durable intent reference", actionID)
	}
	return intent, nil
}

func (s *SessionRunCommitSink) findIntentByActionIDLocked(runID, actionID, ownerGeneration string) (SessionEntry, error) {
	var match SessionEntry
	found := false
	for _, row := range s.branchRows {
		if row.RecordVersion != 1 || row.Type != EntryTypeTool || row.ActionState != ActionStateIntent || row.ActionID != actionID {
			continue
		}
		if row.RunID != runID || row.OwnerGeneration != ownerGeneration || row.WorkspaceID != s.workspaceID {
			return SessionEntry{}, errors.New("session: action ID belongs to a different run owner or workspace")
		}
		if found {
			return SessionEntry{}, fmt.Errorf("session: action ID %q has multiple durable intents", actionID)
		}
		match = *row.clone()
		found = true
	}
	if !found {
		return SessionEntry{}, fmt.Errorf("session: action %q has no durable intent", actionID)
	}
	return match, nil
}

func (s *SessionRunCommitSink) findReceiptForIntentLocked(ref PendingActionRef, intent SessionEntry) (SessionEntry, error) {
	var match SessionEntry
	found := false
	for _, row := range s.branchRows {
		if row.RecordVersion != 1 || row.Type != EntryTypeTool || row.ActionState != "" || row.RunID != ref.RunID || row.AssistantTranscriptOrdinal == nil || *row.AssistantTranscriptOrdinal != ref.AssistantTranscriptOrdinal || row.ToolCallID != ref.ToolCallID {
			continue
		}
		if !sameActionReference(ref, refFromEntry(row, s.sessionID)) {
			return SessionEntry{}, errors.New("session: durable receipt does not match the full reserved action reference")
		}
		if row.OwnerGeneration != intent.OwnerGeneration {
			return SessionEntry{}, errors.New("session: durable receipt owner generation differs from its intent")
		}
		if found {
			return SessionEntry{}, errors.New("session: action has multiple durable terminal receipts")
		}
		match = *row.clone()
		found = true
	}
	if !found {
		return SessionEntry{}, errors.New("session: execution resolution requires an earlier durable terminal receipt")
	}
	return match, nil
}

func (s *SessionRunCommitSink) resolveEvidenceReferenceLocked(
	resolution core.ExecutionResolution,
	ref PendingActionRef,
	intent SessionEntry,
	receipt SessionEntry,
) (core.ExecutionResolution, error) {
	selectExact := func(candidates []SessionEntry, expectedKind string) (string, error) {
		if resolution.EvidenceRef != "" {
			for _, candidate := range candidates {
				if candidate.ID == resolution.EvidenceRef {
					return candidate.ID, nil
				}
			}
			return "", fmt.Errorf("session: evidence reference %q does not name a matching %s row", resolution.EvidenceRef, expectedKind)
		}
		if len(candidates) != 1 {
			return "", fmt.Errorf("session: expected exactly one matching %s row, found %d", expectedKind, len(candidates))
		}
		return candidates[0].ID, nil
	}
	switch resolution.Evidence {
	case core.ResolutionNeverDispatched:
		if receipt.DispatchState != core.DispatchStateNotDispatched || (receipt.ReceiptOutcome != core.ReceiptOutcomeError && receipt.ReceiptOutcome != core.ReceiptOutcomeInterrupted) || receipt.ActionID != ref.ActionID {
			return core.ExecutionResolution{}, errors.New("session: never-dispatched resolution lacks an exact not-dispatched denial receipt")
		}
		id, err := selectExact([]SessionEntry{receipt}, "not-dispatched receipt")
		if err != nil {
			return core.ExecutionResolution{}, err
		}
		resolution.EvidenceRef = id
	case core.ResolutionLocalResourcesStopped:
		if receipt.DispatchState != core.DispatchStateDispatched {
			return core.ExecutionResolution{}, errors.New("session: local quiescence resolution requires a dispatched terminal receipt")
		}
		candidates := make([]SessionEntry, 0, 1)
		for _, row := range s.branchRows {
			if row.RecordVersion != 1 || row.ActionState != ActionStateExecution || row.CompletionProof == "" || row.DispatchState != core.DispatchStateDispatched || row.Seq <= intent.Seq || row.OwnerGeneration != intent.OwnerGeneration || !sameActionReference(ref, refFromEntry(row, s.sessionID)) {
				continue
			}
			candidates = append(candidates, *row.clone())
		}
		id, err := selectExact(candidates, "completion-bearing execution")
		if err != nil {
			return core.ExecutionResolution{}, err
		}
		resolution.EvidenceRef = id
	case core.ResolutionOperatorReconciled:
		candidates := make([]SessionEntry, 0, 1)
		for _, row := range s.branchRows {
			if row.RecordVersion != 1 || row.Type != EntryTypeCheckpoint || row.CheckpointKind != CheckpointReconciliation || row.Seq <= receipt.Seq || row.OwnerGeneration != intent.OwnerGeneration || !sameActionReference(ref, refFromEntry(row, s.sessionID)) {
				continue
			}
			candidates = append(candidates, *row.clone())
		}
		id, err := selectExact(candidates, "operator reconciliation checkpoint")
		if err != nil {
			return core.ExecutionResolution{}, err
		}
		resolution.EvidenceRef = id
	case core.ResolutionRemoteTerminal:
		return core.ExecutionResolution{}, errors.New("session: remote terminal resolution requires unsupported typed protocol evidence")
	default:
		return core.ExecutionResolution{}, fmt.Errorf("session: unsupported execution resolution evidence %q", resolution.Evidence)
	}
	return resolution, nil
}

func (s *SessionRunCommitSink) receiptEntry(receipt core.RunToolReceipt, message core.AgentMessage, intent SessionEntry) SessionEntry {
	row := sessionEntryFromRunCommitMessage(message, EntryTypeTool)
	row.RunID = receipt.RunID
	row.WorkspaceID = s.workspaceID
	row.ToolCallID = receipt.ToolCallID
	row.ToolName = message.ToolName
	row.ReceiptOutcome = receipt.ReceiptOutcome
	row.DispatchState = receipt.DispatchState
	ordinal, index := receipt.AssistantTranscriptOrdinal, receipt.TranscriptIndex
	row.AssistantTranscriptOrdinal = &ordinal
	row.TranscriptIndex = &index
	if receipt.ActionID != "" {
		row.ActionBatchID = intent.ActionBatchID
		row.ActionID = intent.ActionID
		row.OwnerGeneration = intent.OwnerGeneration
	}
	return row
}

func sameReceiptRow(existing, candidate SessionEntry) bool {
	return existing.ID == candidate.ID && existing.Type == EntryTypeTool && existing.ActionState == "" &&
		existing.RunID == candidate.RunID && existing.WorkspaceID == candidate.WorkspaceID &&
		existing.ActionBatchID == candidate.ActionBatchID && existing.ActionID == candidate.ActionID &&
		existing.OwnerGeneration == candidate.OwnerGeneration && existing.AssistantTranscriptOrdinal != nil &&
		candidate.AssistantTranscriptOrdinal != nil && *existing.AssistantTranscriptOrdinal == *candidate.AssistantTranscriptOrdinal &&
		existing.TranscriptIndex != nil && candidate.TranscriptIndex != nil && *existing.TranscriptIndex == *candidate.TranscriptIndex &&
		existing.ToolCallID == candidate.ToolCallID && existing.ToolName == candidate.ToolName &&
		existing.ReceiptOutcome == candidate.ReceiptOutcome && existing.DispatchState == candidate.DispatchState &&
		durableRowMatchesMessage(existing, core.AgentMessage{
			Role: "tool", Content: candidate.Content, ContentBlocks: candidate.ContentBlocks, Usage: candidate.Usage,
			ToolCallID: candidate.ToolCallID, ToolName: candidate.ToolName, ReceiptOutcome: candidate.ReceiptOutcome,
			MaterializedArtifactDigest: candidate.MaterializedArtifactDigest,
		})
}

func sameExecutionObservation(existing, candidate SessionEntry) bool {
	return existing.ID == candidate.ID && existing.Type == EntryTypeTool && existing.ActionState == ActionStateExecution &&
		existing.RunID == candidate.RunID && existing.WorkspaceID == candidate.WorkspaceID &&
		existing.ActionBatchID == candidate.ActionBatchID && existing.ActionID == candidate.ActionID &&
		existing.OwnerGeneration == candidate.OwnerGeneration &&
		sameUint64Pointer(existing.AssistantTranscriptOrdinal, candidate.AssistantTranscriptOrdinal) &&
		existing.ToolCallID == candidate.ToolCallID && existing.ToolName == candidate.ToolName &&
		existing.DispatchState == candidate.DispatchState && existing.ResourceState == candidate.ResourceState &&
		existing.CompletionProof == candidate.CompletionProof && reflect.DeepEqual(existing.ExecutionIdentity, candidate.ExecutionIdentity)
}

func assistantToolName(assistant SessionEntry, toolCallID string) (string, bool) {
	for _, call := range assistant.ToolCalls {
		if call.ID == toolCallID {
			return call.Name, true
		}
	}
	return "", false
}

func cloneUint64Pointer(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func validRunReceiptOutcome(outcome string) bool {
	switch outcome {
	case core.ReceiptOutcomeSuccess, core.ReceiptOutcomeError, core.ReceiptOutcomeInterrupted, core.ReceiptOutcomeUnknown:
		return true
	default:
		return false
	}
}

func validateTranscriptRowsInPhysicalOrder(rows []SessionEntry, runID string) error {
	var next uint64
	for _, row := range rows {
		if row.RecordVersion != 1 || row.RunID != runID || row.TranscriptIndex == nil || row.ActionState != "" || row.Type == EntryTypeCheckpoint || row.Type == EntryTypeCompaction {
			continue
		}
		if row.Type != EntryTypeUser && row.Type != EntryTypeAssistant && row.Type != EntryTypeTool {
			return fmt.Errorf("session: run %q row %q has a non-canonical transcript type %q", runID, row.ID, row.Type)
		}
		if *row.TranscriptIndex != next {
			return fmt.Errorf("session: run %q durable transcript is not a physical canonical prefix at row %q: index %d, want %d", runID, row.ID, *row.TranscriptIndex, next)
		}
		if next == ^uint64(0) {
			return fmt.Errorf("session: run %q transcript index is exhausted", runID)
		}
		next++
	}
	return nil
}

func validateAllRunTranscriptPhysicalPrefixes(rows []SessionEntry) error {
	runs := make(map[string]struct{})
	for _, row := range rows {
		if row.RecordVersion == 1 && row.RunID != "" && row.TranscriptIndex != nil && row.ActionState == "" && row.Type != EntryTypeCheckpoint && row.Type != EntryTypeCompaction {
			runs[row.RunID] = struct{}{}
		}
	}
	for runID := range runs {
		if err := validateTranscriptRowsInPhysicalOrder(rows, runID); err != nil {
			return err
		}
	}
	return nil
}

func sameIntentPayload(existing, candidate SessionEntry) bool {
	return existing.ID == candidate.ID && existing.Type == candidate.Type && existing.RunID == candidate.RunID &&
		existing.WorkspaceID == candidate.WorkspaceID && existing.ActionBatchID == candidate.ActionBatchID &&
		existing.ActionID == candidate.ActionID && existing.OwnerGeneration == candidate.OwnerGeneration &&
		existing.ActionState == candidate.ActionState && sameUint64Pointer(existing.AssistantTranscriptOrdinal, candidate.AssistantTranscriptOrdinal) &&
		existing.ToolCallID == candidate.ToolCallID && existing.ToolName == candidate.ToolName &&
		existing.ArgumentDigest == candidate.ArgumentDigest && existing.SchemaDigest == candidate.SchemaDigest &&
		reflect.DeepEqual(existing.ToolIdentity, candidate.ToolIdentity) && reflect.DeepEqual(existing.EffectSet, candidate.EffectSet)
}

func sameUint64Pointer(left, right *uint64) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func validateRunCommitToolCalls(calls []llm.ToolCall) error {
	seen := make(map[string]struct{}, len(calls))
	for index, call := range calls {
		if call.ID == "" || call.Name == "" {
			return fmt.Errorf("session: assistant tool call at index %d has incomplete identity", index)
		}
		if _, exists := seen[call.ID]; exists {
			return fmt.Errorf("session: assistant message repeats tool call ID %q", call.ID)
		}
		seen[call.ID] = struct{}{}
		if err := validateProtocolJSONArgument(call.Args); err != nil {
			return fmt.Errorf("session: assistant tool call %q has non-JSON arguments: %w", call.ID, err)
		}
	}
	return nil
}

func validateKnownPreDispatchReceipts(batch core.RunIntentBatch, calls map[string]llm.ToolCall) error {
	seen := make(map[string]struct{}, len(batch.KnownReceipts))
	for index, receipt := range batch.KnownReceipts {
		if receipt.RunID != batch.RunID || receipt.AssistantTranscriptOrdinal != batch.AssistantTranscriptOrdinal || receipt.ActionID != "" || receipt.DispatchState != core.DispatchStateNotDispatched {
			return fmt.Errorf("session: known pre-dispatch receipt at index %d has reserved or mismatched identity", index)
		}
		call, exists := calls[receipt.ToolCallID]
		if !exists || receipt.Message.Role != "tool" || receipt.Message.ToolCallID != receipt.ToolCallID || receipt.Message.ToolName != call.Name || len(receipt.Message.ToolCalls) != 0 || receipt.Message.InputKind != "" {
			return fmt.Errorf("session: known pre-dispatch receipt at index %d does not match an assistant tool call", index)
		}
		if receipt.ReceiptOutcome != core.ReceiptOutcomeError && receipt.ReceiptOutcome != core.ReceiptOutcomeInterrupted {
			return fmt.Errorf("session: known pre-dispatch receipt at index %d has non-denial outcome %q", index, receipt.ReceiptOutcome)
		}
		if receipt.Message.ReceiptOutcome != "" && receipt.Message.ReceiptOutcome != receipt.ReceiptOutcome {
			return fmt.Errorf("session: known pre-dispatch receipt at index %d conflicts with its message outcome", index)
		}
		if _, exists := seen[receipt.ToolCallID]; exists {
			return fmt.Errorf("session: known pre-dispatch receipts repeat tool call %q", receipt.ToolCallID)
		}
		seen[receipt.ToolCallID] = struct{}{}
	}
	return nil
}

func (s *SessionRunCommitSink) appendAndRefreshLocked(ctx context.Context, rows ...SessionEntry) error {
	_, err := s.appendAndRefreshRowsLocked(ctx, rows...)
	return err
}

func (s *SessionRunCommitSink) appendAndRefreshRowsLocked(ctx context.Context, rows ...SessionEntry) ([]SessionEntry, error) {
	if len(rows) == 0 {
		return []SessionEntry{}, nil
	}
	parentID := s.leafID
	for index := range rows {
		row := &rows[index]
		if row.ID == "" || row.Type == "" {
			return nil, errors.New("session: run-commit row requires an ID and type")
		}
		row.RecordVersion = 1
		row.SessionID = s.sessionID
		row.BranchID = s.branchID
		row.ParentID = parentID
		if row.Timestamp.IsZero() {
			row.Timestamp = time.Now().UTC()
		}
		parentID = row.ID
	}
	canonical, err := s.writer.AppendAndSync(ctx, rows...)
	if err != nil {
		return nil, fmt.Errorf("session: persist run-commit rows: %w", err)
	}
	if err := s.refreshLocked(); err != nil {
		return nil, fmt.Errorf("session: refresh run-commit snapshot after sync: %w", err)
	}
	return canonical, nil
}

func (s *SessionRunCommitSink) refreshLocked() error {
	snapshot, err := s.prepared.Snapshot()
	if err != nil {
		return err
	}
	if snapshot.SessionID != s.sessionID || snapshot.ActiveBranchID != s.branchID {
		return errors.New("session: prepared run-commit session changed physical identity or branch")
	}
	allRows, branchRows, err := validateRunCommitSnapshot(snapshot)
	if err != nil {
		return err
	}
	if _, err := analyzeDurableActionJournal(allRows, s.sessionID); err != nil {
		return fmt.Errorf("session: updated run-commit action journal is invalid: %w", err)
	}
	s.allRows = allRows
	s.branchRows = branchRows
	s.leafID = snapshot.ActiveLeafEntryID
	return nil
}

func validateRunCommitSnapshot(snapshot PreparedSessionSnapshot) ([]SessionEntry, []SessionEntry, error) {
	allRows := cloneSessionEntries(snapshot.DurableEntries)
	entries := make(map[string]*SessionEntry, len(allRows))
	var maxSeq uint64
	for index := range allRows {
		row := &allRows[index]
		if row.ID == "" {
			return nil, nil, fmt.Errorf("session: snapshot durable row %d has no ID", index)
		}
		if _, exists := entries[row.ID]; exists {
			return nil, nil, fmt.Errorf("session: snapshot repeats durable row ID %q", row.ID)
		}
		if row.RecordVersion == 1 && row.Seq == 0 {
			return nil, nil, fmt.Errorf("session: snapshot version 1 row %q has no sequence", row.ID)
		}
		if row.Seq > maxSeq {
			maxSeq = row.Seq
		}
		entries[row.ID] = row
	}
	if maxSeq > snapshot.MaxSeq {
		return nil, nil, errors.New("session: snapshot max sequence precedes a durable row")
	}
	if snapshot.ActiveLeafEntryID == "" {
		if len(entries) != 0 {
			return nil, nil, errors.New("session: non-empty snapshot has no active leaf")
		}
		return allRows, nil, nil
	}
	branchRows, err := orderedDurableBranch(snapshot.ActiveLeafEntryID, entries)
	if err != nil {
		return nil, nil, fmt.Errorf("session: select prepared active branch: %w", err)
	}
	if len(branchRows) == 0 {
		return nil, nil, errors.New("session: active branch has no durable path")
	}
	return allRows, branchRows, nil
}

func transcriptRowsByIndex(rows []SessionEntry, runID string) (map[uint64]SessionEntry, error) {
	indexed := make(map[uint64]SessionEntry)
	for _, row := range rows {
		if row.RecordVersion != 1 || row.RunID != runID || row.TranscriptIndex == nil || row.ActionState != "" || row.Type == EntryTypeCheckpoint || row.Type == EntryTypeCompaction {
			continue
		}
		if row.Type != EntryTypeUser && row.Type != EntryTypeAssistant && row.Type != EntryTypeTool {
			return nil, fmt.Errorf("session: run %q row %q has a non-canonical transcript type %q", runID, row.ID, row.Type)
		}
		if previous, exists := indexed[*row.TranscriptIndex]; exists {
			return nil, fmt.Errorf("session: run %q has duplicate transcript index %d in rows %q and %q", runID, *row.TranscriptIndex, previous.ID, row.ID)
		}
		indexed[*row.TranscriptIndex] = *row.clone()
	}
	return indexed, nil
}

func nextTranscriptIndex(rows map[uint64]SessionEntry) (uint64, error) {
	for index := uint64(0); index < uint64(len(rows)); index++ {
		if _, exists := rows[index]; !exists {
			return 0, fmt.Errorf("session: canonical transcript has a hole at index %d", index)
		}
	}
	if _, exists := rows[uint64(len(rows))]; exists {
		return 0, errors.New("session: canonical transcript index range is not contiguous")
	}
	return uint64(len(rows)), nil
}

func durableRowMatchesMessage(row SessionEntry, expected core.AgentMessage) bool {
	actual, ok := sessionEntryToAgentMessage(row)
	if !ok {
		return false
	}
	// Runtime source coordinates intentionally stay outside the durable row
	// schema. Compare only the message fields the sink actually persists.
	expected.Runtime = llm.RuntimeMetadata{}
	return reflect.DeepEqual(actual, expected)
}

func cloneRunCommitMessage(message core.AgentMessage) core.AgentMessage {
	return core.CloneAgentMessages([]core.AgentMessage{message})[0]
}

func sessionEntryFromRunCommitMessage(message core.AgentMessage, entryType EntryType) SessionEntry {
	message = cloneRunCommitMessage(message)
	return SessionEntry{
		Type: entryType, Content: message.Content, ContentBlocks: message.ContentBlocks,
		Usage: message.Usage, ToolCalls: message.ToolCalls, ToolCallID: message.ToolCallID,
		ToolName: message.ToolName, InputKind: message.InputKind, ReceiptOutcome: message.ReceiptOutcome,
		MaterializedArtifactDigest: message.MaterializedArtifactDigest,
	}
}

func stableRunCommitEntryID(domain string, key any) (string, error) {
	encoded, err := json.Marshal(key)
	if err != nil {
		return "", fmt.Errorf("session: encode stable run-commit entry identity: %w", err)
	}
	payload := append([]byte(domain+"\x00"), encoded...)
	digest := sha256.Sum256(payload)
	return "runcommit_" + hex.EncodeToString(digest[:]), nil
}
