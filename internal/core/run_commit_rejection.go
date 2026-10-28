package core

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// RejectedBatchReservedOutput is the fixed known pre-dispatch denial text.
const RejectedBatchReservedOutput = "Tool call not dispatched because the workspace is quarantined"

// RejectedBatchSuffixOutput describes calls skipped after a known batch denial.
const RejectedBatchSuffixOutput = "Tool call not dispatched because a prior tool call stopped the group"

// RunCommitRejectedBatchSource extends the host-injected trusted persistence
// sink contract with one-shot already-synced rejected-batch facts. This is not
// an attestation mechanism for tool/provider errors or an untrusted DTO.
type RunCommitRejectedBatchSource interface {
	TakeRejectedIntentBatch(context.Context, RunIntentBatch) ([]RunToolReceipt, bool, error)
}

type knownRejectedBatchError struct{ cause error }

func (e knownRejectedBatchError) Error() string {
	return fmt.Sprintf("core: tool batch denied before dispatch: %v", e.cause)
}
func (e knownRejectedBatchError) Unwrap() error { return e.cause }

func (consumer *runCommitConsumer) takeRejectedBatch(batch RunIntentBatch, transcript *[]TranscriptEntry, messages *[]llm.Message) (bool, error) {
	source, ok := consumer.sink.(RunCommitRejectedBatchSource)
	if !ok {
		return false, nil
	}
	receipts, closed, err := source.TakeRejectedIntentBatch(consumer.ctx, batch)
	if err != nil {
		return false, err
	}
	if !closed {
		return false, nil
	}
	if transcript == nil || messages == nil {
		return false, errors.New("core: rejected batch has no canonical transcript")
	}
	assistantIndex := -1
	for index := len(*transcript) - 1; index >= 0; index-- {
		if len((*transcript)[index].Message.ToolCalls) > 0 {
			assistantIndex = index
			break
		}
	}
	if assistantIndex < 0 {
		return false, errors.New("core: rejected batch has no active assistant group")
	}
	assistant := (*transcript)[assistantIndex].Message
	if !reflect.DeepEqual(assistant.ToolCalls, batch.AssistantMessage.ToolCalls) {
		return false, errors.New("core: rejected batch assistant calls do not match")
	}
	prefix := len(*transcript) - assistantIndex - 1
	if prefix < 0 || prefix > len(assistant.ToolCalls) || len(receipts) != len(assistant.ToolCalls)-prefix {
		return false, errors.New("core: rejected batch does not close the complete remaining group")
	}
	for index := 0; index < prefix; index++ {
		message := (*transcript)[assistantIndex+1+index].Message
		call := assistant.ToolCalls[index]
		if message.Role != "tool" || message.ToolCallID != call.ID || message.ToolName != call.Name {
			return false, errors.New("core: rejected batch has a noncanonical existing receipt prefix")
		}
	}
	intents := make(map[string]ActionIntent, len(batch.Intents))
	known := make(map[string]RunToolReceipt, len(batch.KnownReceipts))
	for _, intent := range batch.Intents {
		intents[intent.ToolCallID] = intent
	}
	for _, receipt := range batch.KnownReceipts {
		known[receipt.ToolCallID] = receipt
	}
	for index, receipt := range receipts {
		call := assistant.ToolCalls[prefix+index]
		message := receipt.Message
		if receipt.RunID != consumer.runID || receipt.AssistantTranscriptOrdinal != batch.AssistantTranscriptOrdinal || receipt.TranscriptIndex != uint64(len(*transcript)+index) || receipt.ToolCallID != call.ID || message.Role != "tool" || message.ToolCallID != call.ID || message.ToolName != call.Name || message.ReceiptOutcome != receipt.ReceiptOutcome || receipt.DispatchState != DispatchStateNotDispatched || len(message.ToolCalls) > 0 || message.InputKind != "" {
			return false, errors.New("core: rejected batch receipt coordinates or dispatch state differ")
		}
		expectedAction := ""
		expectedOutcome := ReceiptOutcomeInterrupted
		expectedContent := RejectedBatchSuffixOutput
		if intent, ok := intents[call.ID]; ok {
			expectedAction = intent.ActionID
			expectedOutcome = ReceiptOutcomeError
			expectedContent = RejectedBatchReservedOutput
		}
		if preKnown, ok := known[call.ID]; ok {
			if receipt.ActionID != "" || !reflect.DeepEqual(receipt, preKnown) {
				return false, errors.New("core: rejected batch changed its pre-known receipt")
			}
			expectedOutcome = preKnown.ReceiptOutcome
			expectedContent = preKnown.Message.Content
		}
		expectedMessage := AgentMessage{Role: "tool", ToolCallID: call.ID, ToolName: call.Name, Content: expectedContent, ReceiptOutcome: expectedOutcome}
		if !reflect.DeepEqual(message, expectedMessage) {
			return false, errors.New("core: rejected batch changed canonical receipt content")
		}
		if receipt.ActionID != expectedAction || receipt.ReceiptOutcome != expectedOutcome || message.Content != expectedContent {
			return false, errors.New("core: rejected batch outcome or reserved action differs")
		}
	}
	for index, receipt := range receipts {
		call := assistant.ToolCalls[prefix+index]
		appendToolReceipt(messages, transcript, call, receipt.Message.Content, receipt.ReceiptOutcome, consumer)
	}
	return true, nil
}
