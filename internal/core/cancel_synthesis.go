package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// CancelSynthesizer converts cancellation errors into system messages that can
// be appended to the session log when a Turn is cancelled, ensuring the log has
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
//	"The previous turn was cancelled due to: <reason>. The conversation continues from here."
//
// where <reason> is:
//   - "The operation was cancelled by the user." when err is context.Canceled,
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
		reason = "The operation was cancelled by the user."
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
		"The previous turn was cancelled due to: %s. The conversation continues from here.",
		reason,
	)

	slog.Debug("core.cancel_synthesis.synthesize", "reason", reason, "error", original)

	return SynthesizedMessage{
		Role:          "system",
		Content:       content,
		OriginalError: original,
	}
}
