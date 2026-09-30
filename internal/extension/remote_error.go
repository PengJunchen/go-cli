package extension

import (
	"errors"
	"strings"
)

// ErrRemoteExtensionUnsupported reports that a remote extension descriptor
// cannot be executed by the built-in loader.
var ErrRemoteExtensionUnsupported = errors.New("extension: remote extension loading is unavailable")

const redactedRemoteExtensionPath = "<remote-extension-path>"

func isRemoteExtensionPath(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

func safeExtensionPath(path string) string {
	if isRemoteExtensionPath(path) {
		return redactedRemoteExtensionPath
	}
	return path
}
