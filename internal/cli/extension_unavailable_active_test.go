//go:build !no_plugin

package cli

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
)

func TestAssemblyRejectsHTTPPluginPathAsUnavailable(t *testing.T) {
	requests, logs, assemblyReady, err := runAssemblyWithRemoteExtensionPath(t)
	require.ErrorIs(t, err, extension.ErrRemoteExtensionUnsupported)
	require.Zero(t, requests)
	require.False(t, assemblyReady, "a rejected extension path cannot yield a ready assembly")
	require.NotContains(t, err.Error(), remoteExtensionAssemblyCanary)
	require.NotContains(t, logs, remoteExtensionAssemblyCanary)
}
