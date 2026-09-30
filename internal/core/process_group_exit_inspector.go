package core

import (
	"errors"
	"time"
)

var errPIDNamespaceTokenUnsupported = errors.New("PID namespace tokens are supported only on Linux")

// ProcessGroupExitStatus is the result of a read-only check of a persisted
// process-group identity. Unknown is the only safe result when the inspector
// cannot verify the same host and process namespace.
type ProcessGroupExitStatus string

const (
	ProcessGroupExitUnknown ProcessGroupExitStatus = "unknown"
	ProcessGroupExitPresent ProcessGroupExitStatus = "present"
	ProcessGroupExitAbsent  ProcessGroupExitStatus = "absent"
)

// ProcessGroupExitInspection records the status observed for one local process
// group. Absent means the kernel reported that the group did not exist at
// ObservedAt; it does not prove that a process which escaped the group stopped.
type ProcessGroupExitInspection struct {
	Status     ProcessGroupExitStatus
	Reason     string
	ObservedAt time.Time
}

// InspectProcessGroupExit checks whether the persisted local process group is
// still present. It never sends a signal to the saved leader or process group.
// Unsupported, stale, inaccessible, or incomplete identities remain unknown.
func InspectProcessGroupExit(identity ExecutionIdentity) ProcessGroupExitInspection {
	if err := identity.Validate(); err != nil {
		return unknownProcessGroupInspection("invalid execution identity")
	}
	if identity.Kind != ExecutionIdentityProcessGroup {
		return unknownProcessGroupInspection("identity is not a Unix process group")
	}
	return inspectPlatformProcessGroupExit(identity)
}

func unknownProcessGroupInspection(reason string) ProcessGroupExitInspection {
	return ProcessGroupExitInspection{Status: ProcessGroupExitUnknown, Reason: reason}
}

func processGroupInspection(status ProcessGroupExitStatus, reason string) ProcessGroupExitInspection {
	return ProcessGroupExitInspection{Status: status, Reason: reason, ObservedAt: time.Now().UTC()}
}
