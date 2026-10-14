package core

import "time"

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
// group. On Linux, Present is reported only while the saved leader identity is
// still observed as an active process in the recorded group; an existing
// numeric group ID without that anchor remains unknown. Absent means the kernel
// probe reported no group during inspection, but it does not prove that an
// escaped process stopped.
type ProcessGroupExitInspection struct {
	Status ProcessGroupExitStatus
	Reason string
	// ObservedAt is captured after the kernel probe and approximates the time
	// at which the status was observed.
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
