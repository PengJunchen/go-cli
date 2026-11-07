package core

import (
	"context"
	"errors"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type editLoopRecoveryControlKey struct{}

type editLoopRejection struct {
	cause      error
	repetition *llm.RepeatedResponseError
}

type streamedAssistantTextError struct{ cause error }

func (e *streamedAssistantTextError) Error() string { return e.cause.Error() }
func (e *streamedAssistantTextError) Unwrap() error { return e.cause }

func (e *editLoopRejection) Error() string { return e.cause.Error() }
func (e *editLoopRejection) Unwrap() error { return e.cause }

type completedEditForRecovery struct {
	response         llm.Message
	action           completedToolAction
	callID           string
	ordinal          uint64
	inputGeneration  int
	catalogVersion   int
	catalogVersioned bool
	mismatch         *tools.PreparedEditMismatchEvidence
}

func soleRepeatedResponse(err error) *llm.RepeatedResponseError {
	for current := err; current != nil; current = errors.Unwrap(current) {
		if _, joined := current.(interface{ Unwrap() []error }); joined {
			return nil
		}
		if repeated, ok := current.(*llm.RepeatedResponseError); ok {
			return repeated
		}
	}
	return nil
}

func canRecoverEditLoop(ctx context.Context, consumer *runCommitConsumer, previous *completedEditForRecovery, rejection *editLoopRejection) bool {
	if ctx == nil || ctx.Err() != nil || CheckRunGeneration(ctx) != nil || llm.ContinuationNoReplay(ctx) ||
		consumer == nil || previous == nil || rejection == nil || rejection.repetition == nil || previous.mismatch == nil {
		return false
	}
	var streamedText *streamedAssistantTextError
	if errors.As(rejection.cause, &streamedText) {
		return false
	}
	action := previous.action
	current, present := consumer.completedTools[previous.callID]
	if action.actionID == "" || action.callID != previous.callID || action.runID != consumer.runID || action.runID != RunIDFromContext(ctx) ||
		action.ownerGeneration == "" || action.ownerGeneration != consumer.binding.OwnerGeneration || action.ordinal == 0 || action.ordinal != previous.ordinal ||
		!present || len(consumer.completedTools) != 1 || current.actionID != action.actionID || current.callID != action.callID ||
		current.runID != action.runID || current.ownerGeneration != action.ownerGeneration || current.ordinal != action.ordinal ||
		current.editMismatch != previous.mismatch ||
		previous.response.Content != "" || len(previous.response.ToolCalls) != 1 || previous.callID == "" ||
		previous.response.ToolCalls[0].ID != previous.callID || previous.response.ToolCalls[0].Name != "edit" {
		return false
	}
	if !rejection.repetition.MatchesResponse(previous.response.Content, previous.response.ToolCalls) {
		return false
	}
	return true
}

func recoveryReadDefinitions(definitions []tools.ToolDefinition) []tools.ToolDefinition {
	for _, definition := range definitions {
		if definition != nil && definition.Name() == "read" && tools.IsPreparedStockReadDefinition(definition) {
			return []tools.ToolDefinition{definition}
		}
	}
	return nil
}

func validRecoveryRead(consumer *runCommitConsumer, call llm.ToolCall, action completedToolAction, receipt TranscriptEntry, assistantOrdinal uint64, mismatch *tools.PreparedEditMismatchEvidence) bool {
	if consumer == nil || mismatch == nil || action.actionID == "" || action.callID != call.ID || call.ID == "" ||
		action.runID != consumer.runID || action.ownerGeneration == "" || action.ownerGeneration != consumer.binding.OwnerGeneration ||
		action.ordinal != assistantOrdinal || receipt.Message.Role != string(llm.RoleTool) || receipt.Message.ToolCallID != call.ID ||
		receipt.Message.ReceiptOutcome != ReceiptOutcomeSuccess || action.readEvidence == nil {
		return false
	}
	return action.readEvidence.CanonicalPath() == mismatch.CanonicalPath()
}
