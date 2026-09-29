package core

import "context"

// RunOutcome describes the logical result of a run independently from
// whether its workers and other owned resources have stopped.
type RunOutcome string

const (
	RunOutcomeSuccess   RunOutcome = "success"
	RunOutcomeError     RunOutcome = "error"
	RunOutcomeCancelled RunOutcome = "cancelled" //nolint:misspell // Persisted/API wire spelling must remain stable.
	RunOutcomeUnknown   RunOutcome = "unknown"
)

// RunExecutionState describes resource ownership and cleanup progress.
type RunExecutionState string

const (
	RunExecutionRunning         RunExecutionState = "running"
	RunExecutionCancelRequested RunExecutionState = "cancel_requested"
	RunExecutionDraining        RunExecutionState = "draining"
	RunExecutionCleanupPending  RunExecutionState = "cleanup_pending"
	RunExecutionQuiescent       RunExecutionState = "quiescent"
	RunExecutionQuarantined     RunExecutionState = "quarantined"
)

// RunStatus is an immutable snapshot of the logical result and owned
// resources for one run.
type RunStatus struct {
	RunID          string
	Outcome        RunOutcome
	ExecutionState RunExecutionState
	PendingWorkers int
	EventGapCount  uint64
}

// RunStatusHandle exposes fresh immutable snapshots as cleanup progresses.
type RunStatusHandle interface {
	Snapshot() RunStatus
}

// RunStatusWaiter waits for the same owner to finish joining its callback,
// resources, durable publication, and run-slot release. On context completion
// it returns the latest status and publication together with ctx.Err(); callers
// must not treat that snapshot as a successful join.
type RunStatusWaiter interface {
	WaitForCompletion(context.Context) (RunStatus, DurablePublication, error)
}

// RunStatusSource is optionally implemented by asynchronous result streams.
type RunStatusSource interface {
	RunStatusHandle() RunStatusHandle
}
