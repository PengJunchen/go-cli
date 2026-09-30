//go:build darwin

package core

func inspectPlatformProcessGroupExit(identity ExecutionIdentity) ProcessGroupExitInspection {
	if identity.Platform != "darwin" {
		return unknownProcessGroupInspection("execution identity belongs to another platform")
	}
	return unknownProcessGroupInspection("Darwin process-group exit inspection is not available")
}

// CurrentPIDNamespaceToken is unsupported on Darwin because it has no Linux
// PID namespace identity.
func CurrentPIDNamespaceToken() (string, error) {
	return "", errPIDNamespaceTokenUnsupported
}
