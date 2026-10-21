//go:build unix

package extension

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRPCArtifactRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker")
	require.NoError(t, syscall.Mkfifo(path, 0600))
	_, _, err := startRPCProcess(context.Background(), RPCProcessConfig{Command: []string{path}, ArtifactSHA256: strings.Repeat("0", 64)})
	require.ErrorContains(t, err, "regular file")
}
