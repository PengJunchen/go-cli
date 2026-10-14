//go:build !linux && !darwin

package core

func inspectPlatformProcessGroupExit(ExecutionIdentity) ProcessGroupExitInspection {
	return unknownProcessGroupInspection("process-group exit inspection is unsupported on this platform")
}

func CurrentPIDNamespaceToken() (string, error) {
	return "", errPIDNamespaceTokenUnsupported
}
