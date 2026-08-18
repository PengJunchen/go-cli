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

// eventsToTurnMessages reconstructs the AgentMessage slice produced during a
// turn from the events fired by the loop. It preserves assistant messages
// (with tool calls) and tool results so a retry can continue from where the
// previous attempt left off instead of replaying from the beginning.
//
// Incremental "message" events (streaming fragments) are skipped; only the
// final non-incremental "message" event carries the complete assistant
// response and tool calls.
func eventsToTurnMessages(events []AgentEvent) []AgentMessage {
	// Build a map of ToolCallID -> tool name from "tool_call" events so we
	// can populate ToolName on tool-result messages.
	toolNames := make(map[string]string)
	for _, ev := range events {
		if ev.Kind == "tool_call" {
			toolNames[ev.ToolCallID] = ev.Content
		}
	}

	var msgs []AgentMessage
	for _, ev := range events {
		switch ev.Kind {
		case "message":
			if ev.Incremental {
				continue // Skip streaming fragments
			}
			msgs = append(msgs, AgentMessage{
				Role:      "assistant",
				Content:   ev.Content,
				ToolCalls: ev.ToolCalls,
				Usage:     ev.Usage,
			})
		case "tool_result":
			msgs = append(msgs, AgentMessage{
				Role:       "tool",
				Content:    ev.Content,
				ToolCallID: ev.ToolCallID,
				ToolName:   toolNames[ev.ToolCallID],
			})
		case "tool_canceled":
			// For canceled events, Content holds the tool name.
			msgs = append(msgs, AgentMessage{
				Role:       "tool",
				Content:    "Tool call canceled by interceptor",
				ToolCallID: ev.ToolCallID,
				ToolName:   ev.Content,
			})
		}
	}
	return msgs
}

// Run delegates to the wrapped loop. On a recoverable error it synthesizes a
// recovery message, injects it at the interruption point, and retries once.
// The retry resumes from the failure point: already-completed tool calls and
// their results are preserved as context so the LLM continues instead of
// replaying the entire turn (which would re-execute tools and cause duplicate
// side effects).
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

	// Reconstruct the turn messages (assistant responses + tool results)
	// from the events produced by the failed attempt. This allows the retry
	// to continue from the failure point instead of replaying the entire
	// turn, which would re-execute already-completed tool calls.
	turnMessages := eventsToTurnMessages(events)

	// Build the retry history: original history + the original user message
	// (now part of history) + the turn messages from the failed attempt.
	// The synthesized message becomes the new submission content so it is
	// injected at the interruption point as a user message, prompting the
	// LLM to continue from where it left off.
	retryHistory := make([]AgentMessage, 0, len(submission.History)+len(turnMessages)+1)
	retryHistory = append(retryHistory, submission.History...)
	retryHistory = append(retryHistory, AgentMessage{Role: "user", Content: submission.Content})
	retryHistory = append(retryHistory, turnMessages...)

	retrySubmission := Submission{
		Type:    submission.Type,
		Content: msg.Content,
		History: retryHistory,
	}

	retryEvents, retryErr := l.next.Run(spanCtx, retrySubmission, stream...)
	return append(events, retryEvents...), retryErr
}
