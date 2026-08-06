package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// defaultAskTimeout is the default wait for a user answer when none is
// configured on the tool.
const defaultAskTimeout = 30 * time.Second

// HITLQuestionEvent is emitted when the agent needs user input. This is the
// tools-package consumer-side contract; it mirrors core.HITLQuestionEvent. A
// local copy is required because the tools package cannot import core (core
// already depends on tools), so the tool depends on its own contract and the
// core package provides an adapter (core.AdaptHITLEmitter).
type HITLQuestionEvent struct {
	// QuestionID uniquely identifies the question so the answer can be
	// matched back.
	QuestionID string
	// Question is the prompt presented to the user.
	Question string
	// Options holds the choices for a multiple-choice question. It is empty
	// for free-text questions.
	Options []string
	// ResponseCh is where the caller sends the user's answer. It must be
	// buffered so a single answer never blocks the sender.
	ResponseCh chan HITLAnswer
}

// HITLAnswer is the user's response to a HITL question.
type HITLAnswer struct {
	// QuestionID links the answer back to the originating question.
	QuestionID string
	// Answer is the user's textual response (a chosen option or free text).
	Answer string
	// Error carries any error the HITL layer produced (e.g. user dismissed).
	Error error
}

// HITLQuestionEmitter is the sink a tool uses to surface a HITL question. The
// emitter is responsible for eventually delivering exactly one HITLAnswer on
// event.ResponseCh.
//
//exempt:scan012 // consumer-side interface; default impl in core package
type HITLQuestionEmitter interface {
	// Emit delivers the question event to the HITL layer.
	Emit(ctx context.Context, event HITLQuestionEvent) error
}

// AskUserQuestionTool is a ToolDefinition that asks the user a question and
// waits for the answer. It supports both multiple-choice (options provided)
// and free-text (no options) modes.
type AskUserQuestionTool struct {
	emitter HITLQuestionEmitter
	timeout time.Duration
}

var _ ToolDefinition = (*AskUserQuestionTool)(nil)

// NewAskUserQuestionTool builds an AskUserQuestionTool backed by emitter. A
// zero timeout falls back to defaultAskTimeout at execution time.
func NewAskUserQuestionTool(emitter HITLQuestionEmitter, timeout time.Duration) *AskUserQuestionTool {
	return &AskUserQuestionTool{emitter: emitter, timeout: timeout}
}

// Name returns the tool name.
func (t *AskUserQuestionTool) Name() string { return "ask_user" }

// Description returns a brief description of the tool.
func (t *AskUserQuestionTool) Description() string {
	return "ask_user: asks the user a question and waits for the answer. Args: question (string, required), options ([]string, optional for multiple-choice)."
}

// Execute extracts the question and options from call.Args, emits a
// HITLQuestionEvent, and waits for the answer with a timeout. The timeout
// context is derived from the incoming context so the emitter's work is
// canceled when the wait gives up.
func (t *AskUserQuestionTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	question, ok := call.Args["question"].(string)
	if !ok || question == "" {
		return nil, errors.New("ask_user: missing string argument 'question'")
	}
	options := toStringSlice(call.Args["options"])

	timeout := t.timeout
	if timeout <= 0 {
		timeout = defaultAskTimeout
	}

	askCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	event := HITLQuestionEvent{
		QuestionID: nextRequestID("q"),
		Question:   question,
		Options:    options,
		ResponseCh: make(chan HITLAnswer, 1),
	}

	slog.Debug("tools.ask_user.execute",
		"question_id", event.QuestionID,
		"options", len(event.Options),
		"timeout", timeout.String(),
	)

	if err := t.emitter.Emit(askCtx, event); err != nil {
		return nil, fmt.Errorf("ask_user: %w", err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case ans := <-event.ResponseCh:
		if ans.Error != nil {
			return nil, fmt.Errorf("ask_user: %w", ans.Error)
		}
		return &ToolResult{
			Output:   ans.Answer,
			Metadata: map[string]any{"question_id": event.QuestionID},
		}, nil
	case <-timer.C:
		return nil, fmt.Errorf("ask_user: timed out after %s", timeout)
	case <-askCtx.Done():
		return nil, askCtx.Err()
	}
}
