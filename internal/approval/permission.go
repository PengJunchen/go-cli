package approval

// PermissionMode is the 4-level operator-selectable approval posture. It is
// resolved by a PermissionModeResolver into a concrete ApprovalClassifier so
// the ApprovalMiddleware can switch policy without being re-wired.
type PermissionMode int

const (
	// PermissionDefault applies the safety-policy classifier: dangerous tools
	// are denied, everything else is allowed.
	PermissionDefault PermissionMode = iota
	// PermissionPlan holds tool execution for plan confirmation: the resolved
	// classifier returns Ask so each call needs explicit confirmation.
	PermissionPlan
	// PermissionAuto auto-allows safe tools and asks (requires confirmation)
	// for dangerous tools.
	PermissionAuto
	// PermissionAutoFull allows every tool call without prompting.
	PermissionAutoFull
)

// String returns the stable lowercase name of the permission mode, used for
// log fields and span attributes.
func (m PermissionMode) String() string {
	switch m {
	case PermissionPlan:
		return "plan"
	case PermissionAuto:
		return "auto"
	case PermissionAutoFull:
		return "auto_full"
	default:
		return "default"
	}
}
