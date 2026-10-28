package session

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/pengjunchen/go-cli/internal/core"
)

type closedRejectedBatch struct {
	batch    core.RunIntentBatch
	receipts []core.RunToolReceipt
	consumed bool
}

var _ core.RunCommitRejectedBatchSource = (*SessionRunCommitSink)(nil)

func cloneRejectedBatch(batch core.RunIntentBatch) core.RunIntentBatch {
	batch.AssistantMessage = cloneRunCommitMessage(batch.AssistantMessage)
	batch.Intents = append([]core.ActionIntent(nil), batch.Intents...)
	batch.KnownReceipts = cloneRejectedReceipts(batch.KnownReceipts)
	return batch
}

func cloneRejectedReceipts(receipts []core.RunToolReceipt) []core.RunToolReceipt {
	result := append([]core.RunToolReceipt(nil), receipts...)
	for index := range result {
		result[index].Message = cloneRunCommitMessage(result[index].Message)
	}
	return result
}

// TakeRejectedIntentBatch exposes only this trusted sink's already-synced,
// same-owner facts. It is not a tool/provider error self-attestation interface.
// Mismatches never consume the legitimate one-shot result.
func (s *SessionRunCommitSink) TakeRejectedIntentBatch(ctx context.Context, batch core.RunIntentBatch) ([]core.RunToolReceipt, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateCallContextLocked(ctx, batch.RunID); err != nil {
		return nil, false, err
	}
	closed := s.rejectedBatch
	if closed == nil || closed.consumed {
		return nil, false, nil
	}
	if !reflect.DeepEqual(closed.batch, batch) {
		return nil, false, errors.New("session: rejected-batch lookup does not match its exact captured batch")
	}
	closed.consumed = true
	return cloneRejectedReceipts(closed.receipts), true, nil
}

func (s *SessionRunCommitSink) closeRejectedBatchLocked(ctx context.Context, batch core.RunIntentBatch) error {
	assistant, exists, err := s.assistantOccurrenceLocked(batch.RunID, batch.AssistantTranscriptOrdinal)
	if err != nil {
		return err
	}
	if !exists || assistant.TranscriptIndex == nil {
		return errors.New("session: denied batch has no canonical assistant")
	}
	rows, err := transcriptRowsByIndex(s.branchRows, batch.RunID)
	if err != nil {
		return err
	}
	next, err := nextTranscriptIndex(rows)
	if err != nil {
		return err
	}
	plan, err := analyzeDurableActionJournal(s.allRows, s.sessionID)
	if err != nil {
		return err
	}
	resolved := make(map[string]bool)
	for _, occurrence := range plan.occurrences {
		if occurrence.intent != nil && occurrence.resolution != nil {
			resolved[occurrence.intent.ActionID] = true
		}
	}
	prefix := uint64(0)
	for index := *assistant.TranscriptIndex + 1; index < next; index++ {
		offset := index - *assistant.TranscriptIndex - 1
		if offset >= uint64(len(assistant.ToolCalls)) {
			return errors.New("session: denied batch has rows beyond its open tool group")
		}
		row := rows[index]
		call := assistant.ToolCalls[offset]
		if row.Type != EntryTypeTool || row.ToolCallID != call.ID || row.ToolName != call.Name || row.ActionState != "" {
			return errors.New("session: denied batch existing receipts are not its closed canonical prefix")
		}
		if row.ReceiptOutcome == core.ReceiptOutcomeUnknown || (row.ActionID != "" && !resolved[row.ActionID]) {
			return errors.New("session: denied batch cannot pass a pending prior receipt")
		}
		prefix++
	}
	intents := make(map[string]core.ActionIntent, len(batch.Intents))
	known := make(map[string]core.RunToolReceipt, len(batch.KnownReceipts))
	for _, intent := range batch.Intents {
		intents[intent.ToolCallID] = intent
	}
	for _, receipt := range batch.KnownReceipts {
		known[receipt.ToolCallID] = receipt
	}
	closed := make([]core.RunToolReceipt, 0, len(assistant.ToolCalls)-int(prefix))
	for _, call := range assistant.ToolCalls[prefix:] {
		intent, reserved := intents[call.ID]
		existing, hasIntent, findErr := s.findOccurrenceIntentLocked(batch.RunID, batch.AssistantTranscriptOrdinal, call.ID)
		if findErr != nil {
			return findErr
		}
		if hasIntent && (!reserved || existing.ActionID != intent.ActionID || existing.ActionBatchID != batch.ActionBatchID || existing.OwnerGeneration != s.ownerGen) {
			return errors.New("session: denied batch cannot close an unrelated pending intent")
		}
		if reserved && !hasIntent {
			return errors.New("session: denied batch lost its reserved intent")
		}
		receipt := core.RunToolReceipt{RunID: batch.RunID, AssistantTranscriptOrdinal: batch.AssistantTranscriptOrdinal, ToolCallID: call.ID, TranscriptIndex: next, DispatchState: core.DispatchStateNotDispatched, ReceiptOutcome: core.ReceiptOutcomeInterrupted, Message: core.AgentMessage{Role: "tool", ToolCallID: call.ID, ToolName: call.Name, Content: core.RejectedBatchSuffixOutput, ReceiptOutcome: core.ReceiptOutcomeInterrupted}}
		if reserved {
			receipt.ActionID = intent.ActionID
			receipt.ReceiptOutcome = core.ReceiptOutcomeError
			receipt.Message.ReceiptOutcome = core.ReceiptOutcomeError
			receipt.Message.Content = core.RejectedBatchReservedOutput
		}
		if preKnown, ok := known[call.ID]; ok {
			if reserved || preKnown.TranscriptIndex != next {
				return errors.New("session: denied batch pre-known receipt coordinates conflict")
			}
			receipt = preKnown
		}
		if err := s.recordReceiptLocked(ctx, receipt); err != nil {
			return fmt.Errorf("session: sync denied batch receipt: %w", err)
		}
		if reserved {
			durableReceipt, err := s.findReceiptForIntentLocked(refFromEntry(existing, s.sessionID), existing)
			if err != nil {
				return err
			}
			resolution := core.ExecutionResolution{RunID: batch.RunID, ActionID: intent.ActionID, Evidence: core.ResolutionNeverDispatched, EvidenceRef: durableReceipt.ID}
			if err := s.resolveExecutionLocked(ctx, resolution, refFromEntry(existing, s.sessionID), existing, durableReceipt, false); err != nil {
				return err
			}
		}
		closed = append(closed, receipt)
		next++
	}
	if len(closed) == 0 {
		return errors.New("session: denied batch had no unreceipted calls")
	}
	s.rejectedBatch = &closedRejectedBatch{batch: cloneRejectedBatch(batch), receipts: cloneRejectedReceipts(closed)}
	return nil
}
