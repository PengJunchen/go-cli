package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
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
