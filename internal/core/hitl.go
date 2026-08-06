package core

import (
	"context"
	"log/slog"
	"time"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// HITLQuestionEvent is emitted when the agent needs user input. The caller
// (UI / HITL layer) receives the event and delivers a HITLAnswer on
// ResponseCh.
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
//exempt:scan012 // consumer-side interface; bridge adapter provided below
type HITLQuestionEmitter interface {
	// Emit delivers the question event to the HITL layer.
	Emit(ctx context.Context, event HITLQuestionEvent) error
}

// hitlEmitterAdapter bridges a core.HITLQuestionEmitter to the
// tools.HITLQuestionEmitter contract. It exists because the tools package
// cannot import core (core already depends on tools), so the tool defines its
// own consumer-side contract that this adapter satisfies.
type hitlEmitterAdapter struct {
	e HITLQuestionEmitter
}

var _ tools.HITLQuestionEmitter = (*hitlEmitterAdapter)(nil)

// AdaptHITLEmitter wraps a core.HITLQuestionEmitter so it can back a
// tools.AskUserQuestionTool.
func AdaptHITLEmitter(e HITLQuestionEmitter) tools.HITLQuestionEmitter {
	return &hitlEmitterAdapter{e: e}
}

// Emit converts the tools-level event into a core event, delegates to the core
// emitter, and forwards the single answer back to the tools event's response
// channel. The forwarding goroutine honors ctx cancellation so it never leaks
// when the caller stops waiting.
func (a *hitlEmitterAdapter) Emit(ctx context.Context, ev tools.HITLQuestionEvent) error {
	coreEv := HITLQuestionEvent{
		QuestionID: ev.QuestionID,
		Question:   ev.Question,
		Options:    ev.Options,
		ResponseCh: make(chan HITLAnswer, 1),
	}
	if err := a.e.Emit(ctx, coreEv); err != nil {
		return err
	}
	go func() {
		select {
		case ans := <-coreEv.ResponseCh:
			select {
			case ev.ResponseCh <- tools.HITLAnswer{
				QuestionID: ans.QuestionID,
				Answer:     ans.Answer,
				Error:      ans.Error,
			}:
			case <-ctx.Done():
			}
		case <-ctx.Done():
		}
		slog.Debug("core.hitl_adapter.forward_done", "question_id", ev.QuestionID)
	}()
	return nil
}

// NewAskUserQuestionTool builds a tools.AskUserQuestionTool backed by a core
// HITLQuestionEmitter. This is the integration seam for callers that have a
// core emitter and want a registered tool.
func NewAskUserQuestionTool(e HITLQuestionEmitter, timeout time.Duration) *tools.AskUserQuestionTool {
	return tools.NewAskUserQuestionTool(AdaptHITLEmitter(e), timeout)
}
