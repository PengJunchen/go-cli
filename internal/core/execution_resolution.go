package core

// ExecutionResolutionEvidence describes the durable evidence that an action
// can no longer affect the workspace.
type ExecutionResolutionEvidence string

const (
	ResolutionNeverDispatched       ExecutionResolutionEvidence = "never_dispatched"
	ResolutionLocalResourcesStopped ExecutionResolutionEvidence = "local_resources_quiescent"
	ResolutionRemoteTerminal        ExecutionResolutionEvidence = "remote_terminal"
	ResolutionOperatorReconciled    ExecutionResolutionEvidence = "operator_reconciled"
)

// ExecutionResolution is a durable checkpoint for the resource state of one
// action. EvidenceRef identifies a durable entry or a non-secret terminal
// result digest; it must not contain raw arguments, credentials, or response
// bodies.
type ExecutionResolution struct {
	RunID       string                      `json:"run_id"`
	ActionID    string                      `json:"action_id"`
	Evidence    ExecutionResolutionEvidence `json:"evidence"`
	EvidenceRef string                      `json:"evidence_ref"`
}
