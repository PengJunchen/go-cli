package tools

import (
	"context"
	"errors"
)

// PendingExecutionError reports an execution error while the associated
// process or resource may still be active. Outcome certainty and resource
// quiescence are independent: OutcomeUnknown reports whether the action's
// result is unresolved, while ExecutionDone is the resource-quiescence
// receipt. ExecutionDone must close only after the resource is positively
// known to have stopped and its readers have joined. A nil or never-closing
// channel does not prove quiescence. Unwrap preserves the underlying error
// chain for errors.Is and errors.As.
type PendingExecutionError interface {
	error
	OutcomeUnknown() bool
	ExecutionDone() <-chan struct{}
	Unwrap() error
}

// unresolvedExecutionError reports failures that must stop a helper fallback
// chain or a surrounding mutation. A pending resource remains unresolved even
// when the action outcome is known; unknown outcome and unconfirmed local
// containment are independently fail-closed. Context cancellation also stops
// best-effort helper fallback rather than starting additional work.
func unresolvedExecutionError(err error) bool {
	if err == nil {
		return false
	}
	var pending PendingExecutionError
	if errors.As(err, &pending) {
		return true
	}
	var outcome interface{ OutcomeUnknown() bool }
	if errors.As(err, &outcome) && outcome.OutcomeUnknown() {
		return true
	}
	return errors.Is(err, errProcessContainmentUnconfirmed) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}
