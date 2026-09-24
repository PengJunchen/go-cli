package tools

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
