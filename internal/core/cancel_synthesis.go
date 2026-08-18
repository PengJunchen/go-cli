package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// CancelSynthesizer converts cancellation errors into system messages that can
// be appended to the session log when a Turn is canceled, ensuring the log has
// no gaps. The synthesized message explains the cancellation so the agent (and
// the user) can see why the turn ended and continue from there.
type CancelSynthesizer struct{}

// NewCancelSynthesizer returns a CancelSynthesizer.
func NewCancelSynthesizer() *CancelSynthesizer {
	return &CancelSynthesizer{}
}

// SynthesizeCancel creates a SynthesizedMessage with role "system" that
// explains the cancellation represented by err. The content follows the form:
//
//	"The previous turn was canceled due to: <reason>. The conversation continues from here."
//
// where <reason> is:
//   - "The operation was canceled by the user." when err is context.Canceled,
//   - "The operation timed out." when err is context.DeadlineExceeded,
//   - the error's text for any other non-nil error.
//
// A nil error yields a reason of "unknown".
func (s *CancelSynthesizer) SynthesizeCancel(_ context.Context, err error) SynthesizedMessage {
	var reason string
	switch {
	case err == nil:
		reason = "unknown"
	case errors.Is(err, context.Canceled):
		reason = "The operation was canceled by the user."
	case errors.Is(err, context.DeadlineExceeded):
		reason = "The operation timed out."
	default:
		reason = err.Error()
	}

	original := "unknown"
	if err != nil {
		original = err.Error()
	}

	content := fmt.Sprintf(
		"The previous turn was canceled due to: %s. The conversation continues from here.",
		reason,
	)

	slog.Debug("core.cancel_synthesis.synthesize", "reason", reason, "error", original)

	return SynthesizedMessage{
		Role:          "system",
		Content:       content,
		OriginalError: original,
	}
}

// synthesizeCanceledToolResults appends error tool_result messages for any
// tool calls in the last assistant message that don't already have a
// corresponding tool_result in messages. This keeps the message history
// complete (every tool_call has a matching tool_result) when the loop is
// canceled mid-execution, preventing subsequent LLM requests from rejecting
// the conversation due to orphaned tool calls.
//
// It returns the updated messages slice and the list of tool calls for which
// a synthetic result was produced (so the caller can emit matching events).
func synthesizeCanceledToolResults(messages []llm.Message, canceledErr error) ([]llm.Message, []llm.ToolCall) {
	if len(messages) == 0 {
		return messages, nil
	}

	// Find the last assistant message with tool calls.
	var toolCalls []llm.ToolCall
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == llm.RoleAssistant && len(messages[i].ToolCalls) > 0 {
			toolCalls = messages[i].ToolCalls
			break
		}
	}
	if len(toolCalls) == 0 {
		return messages, nil
	}

	// Build a set of ToolCallIDs that already have tool_results, so we
	// don't synthesize duplicates.
	hasResult := make(map[string]bool, len(toolCalls))
	for _, m := range messages {
		if m.Role == llm.RoleTool && m.ToolCallID != "" {
			hasResult[m.ToolCallID] = true
		}
	}

	errMsg := "tool call canceled"
	if canceledErr != nil {
		errMsg = canceledErr.Error()
	}

	var synthesized []llm.ToolCall
	for _, tc := range toolCalls {
		if hasResult[tc.ID] {
			continue
		}
		messages = append(messages, llm.Message{
			Role:       llm.RoleTool,
			ToolCallID: tc.ID,
			Name:       tc.Name,
			Content:    "Error: " + errMsg,
		})
		synthesized = append(synthesized, tc)
	}
	return messages, synthesized
}
