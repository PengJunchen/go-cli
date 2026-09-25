package core

import (
	"errors"
	"fmt"
)

// DispatchState records whether a reserved action crossed its final dispatch
// fence.
type DispatchState string

const (
	// DispatchStateNotDispatched means the executor was positively not entered.
	DispatchStateNotDispatched DispatchState = "not_dispatched"
	// DispatchStateDispatched means the executor may have been entered.
	DispatchStateDispatched DispatchState = "dispatched"
)

// ExecutionIdentity records structured details for a locally owned process
// group or Windows job. It does not prove that the resource is stopped and
// must never be used to signal a restored process by PID alone.
type ExecutionIdentity struct {
	Kind              ExecutionIdentityKind `json:"kind"`
	Platform          string                `json:"platform"`
	HostGeneration    string                `json:"host_generation,omitempty"`
	LeaderPID         int                   `json:"leader_pid,omitempty"`
	ProcessStartToken string                `json:"process_start_token,omitempty"`
	ProcessGroupID    int                   `json:"process_group_id,omitempty"`
	JobID             string                `json:"job_id,omitempty"`
}

// ExecutionIdentityKind identifies the kind of local process containment.
type ExecutionIdentityKind string

const (
	// ExecutionIdentityProcessGroup identifies a Unix process group.
	ExecutionIdentityProcessGroup ExecutionIdentityKind = "process_group"
	// ExecutionIdentityWindowsJob identifies a Windows job object.
	ExecutionIdentityWindowsJob ExecutionIdentityKind = "windows_job"
)

// Validate checks that an execution identity has the fields required for its
// declared platform and containment kind. A valid shape is not proof that the
// resource has stopped or is safe to signal after restart.
func (i ExecutionIdentity) Validate() error {
	if i.HostGeneration == "" {
		return errors.New("core: execution identity host generation is required")
	}
	switch i.Kind {
	case ExecutionIdentityProcessGroup:
		if i.Platform != "darwin" && i.Platform != "linux" {
			return fmt.Errorf("core: process group identity is unsupported on %q", i.Platform)
		}
		if i.LeaderPID <= 0 || i.ProcessStartToken == "" || i.ProcessGroupID <= 0 {
			return errors.New("core: process group identity is incomplete")
		}
	case ExecutionIdentityWindowsJob:
		if i.Platform != "windows" {
			return fmt.Errorf("core: Windows job identity has invalid platform %q", i.Platform)
		}
		if i.LeaderPID <= 0 || i.ProcessStartToken == "" || i.JobID == "" {
			return errors.New("core: Windows job identity is incomplete")
		}
	default:
		return fmt.Errorf("core: unsupported execution identity kind %q", i.Kind)
	}
	return nil
}
