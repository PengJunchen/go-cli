package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// SynthesizedMessage is a system message produced from a recoverable error. It
// can be injected into the conversation context so the agent can continue after
// a failure.
type SynthesizedMessage struct {
	// Role is always "system".
	Role string
	// Content explains the error and suggests a recovery strategy.
	Content string
	// OriginalError is the text of the error that was synthesized.
	OriginalError string
}

// FailureTurnSynthesizer converts recoverable errors into system messages that
// can be injected into the conversation context, allowing the agent to continue
// after errors instead of aborting the turn.
type FailureTurnSynthesizer interface {
	// Synthesize creates a system message from the given error.
	Synthesize(ctx context.Context, err error) (SynthesizedMessage, error)
	// IsRecoverable reports whether the error is one the agent can reasonably
	// retry or work around.
	IsRecoverable(err error) bool
}

// DefaultFailureTurnSynthesizer is the default implementation of
// FailureTurnSynthesizer. It classifies network timeouts, context deadlines,
// connection failures, and temporary errors as recoverable.
type DefaultFailureTurnSynthesizer struct{}

var _ FailureTurnSynthesizer = (*DefaultFailureTurnSynthesizer)(nil)

// NewDefaultFailureTurnSynthesizer returns a DefaultFailureTurnSynthesizer.
func NewDefaultFailureTurnSynthesizer() *DefaultFailureTurnSynthesizer {
	return &DefaultFailureTurnSynthesizer{}
}

// IsRecoverable reports whether the error represents a transient or retryable
// condition. It returns true for context deadlines/cancellation, network
// timeouts, connection-refused errors, and errors whose message contains
// "timeout", "timed out", "connection refused", or "temporary".
func (s *DefaultFailureTurnSynthesizer) IsRecoverable(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, context.Canceled) {
		return true
	}

	// net.Error covers network-level timeouts.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "connection refused") {
		return true
	}
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "timed out") {
		return true
	}
	if strings.Contains(msg, "temporary") {
		return true
	}

	return false
}

// Synthesize creates a SynthesizedMessage from the given error. The message
// explains the error and suggests a recovery strategy. It returns an error if
// err is nil.
func (s *DefaultFailureTurnSynthesizer) Synthesize(_ context.Context, err error) (SynthesizedMessage, error) {
	if err == nil {
		return SynthesizedMessage{}, fmt.Errorf("failure_synthesis: cannot synthesize a nil error")
	}

	recoverable := s.IsRecoverable(err)
	original := err.Error()

	var content string
	if recoverable {
		content = fmt.Sprintf(
			"A recoverable error occurred: %s. The previous action failed transiently. "+
				"You may retry the action, adjust your approach, or continue with an alternative strategy.",
			original,
		)
	} else {
		content = fmt.Sprintf(
			"An error occurred: %s. This error may not be automatically recoverable. "+
				"Assess the situation and inform the user if the task cannot be completed.",
			original,
		)
	}

	slog.Debug("core.failure_synthesis.synthesize", "recoverable", recoverable, "error", original)

	return SynthesizedMessage{
		Role:          "system",
		Content:       content,
		OriginalError: original,
	}, nil
}

// FailureSynthesisMiddleware is an agent-level Middleware that intercepts
// recoverable errors from the wrapped loop. When the loop returns a
// recoverable error, the middleware synthesizes a recovery message, injects
// it into the submission history as a system message, and retries the run
// once. Non-recoverable errors are passed through unchanged.
type FailureSynthesisMiddleware struct {
	synthesizer FailureTurnSynthesizer
	name        string
}

var _ Middleware = (*FailureSynthesisMiddleware)(nil)

// NewFailureSynthesisMiddleware builds a middleware backed by the given
// synthesizer.
func NewFailureSynthesisMiddleware(s FailureTurnSynthesizer) *FailureSynthesisMiddleware {
	return &FailureSynthesisMiddleware{synthesizer: s, name: "failure-synthesis"}
}

// Name returns the middleware identifier.
func (m *FailureSynthesisMiddleware) Name() string {
	if m.name == "" {
		return "failure-synthesis"
	}
	return m.name
}

// Wrap returns a loop-view that retries recoverable errors once with a
// synthesized recovery message.
func (m *FailureSynthesisMiddleware) Wrap(next AgentLoop) AgentLoop {
	return &failureSynthesisLoop{synthesizer: m.synthesizer, next: next}
}

// failureSynthesisLoop is the concrete wrapped loop produced by
// FailureSynthesisMiddleware.
type failureSynthesisLoop struct {
	synthesizer FailureTurnSynthesizer
	next        AgentLoop
}

// Run delegates to the wrapped loop. On a recoverable error it synthesizes a
// recovery message, injects it into the submission history, and retries once.
func (l *failureSynthesisLoop) Run(ctx context.Context, submission Submission, stream ...EventStream) ([]AgentEvent, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "middleware.failure-synthesis", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, nil)

	events, err := l.next.Run(spanCtx, submission, stream...)
	if err == nil {
		return events, err
	}

	// Non-recoverable errors pass through unchanged.
	if l.synthesizer == nil || !l.synthesizer.IsRecoverable(err) {
		logger.Info("failure_synthesis.skip", "recoverable", false, "error", err.Error())
		return events, err
	}

	// Synthesize a recovery message and inject it into the history so the
	// LLM receives context about the failure on the retry.
	msg, synErr := l.synthesizer.Synthesize(spanCtx, err)
	if synErr != nil {
		slog.Warn("core.failure_synthesis.synthesize_failed", "err", synErr)
		return events, err
	}

	logger.Info("failure_synthesis.retry", "original_error", msg.OriginalError, "recoverable", true)
	slog.Info("core.failure_synthesis.retry", "error", msg.OriginalError)

	// Prepend the synthesized system message to the history so the LLM sees
	// the recovery strategy before the original conversation.
	retrySubmission := submission
	retrySubmission.History = append(
		[]AgentMessage{{Role: msg.Role, Content: msg.Content}},
		submission.History...,
	)

	return l.next.Run(spanCtx, retrySubmission, stream...)
}
