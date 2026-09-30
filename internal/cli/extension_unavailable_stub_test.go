//go:build no_plugin

package cli

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
)

func TestAssemblyRejectsHTTPPluginPathWhenLoaderDisabled(t *testing.T) {
	requests, logs, assemblyReady, err := runAssemblyWithRemoteExtensionPath(t)
	require.ErrorIs(t, err, extension.ErrUnsupportedRPC, "no_plugin disables all extension paths before scheme dispatch")
	require.Zero(t, requests)
	require.False(t, assemblyReady)
	require.NotContains(t, err.Error(), remoteExtensionAssemblyCanary)
	require.NotContains(t, logs, remoteExtensionAssemblyCanary)
}
