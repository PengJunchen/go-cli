//go:build !unix

package extension

import (
	"os"
	"os/exec"
)

func configureRPCProcess(_ *exec.Cmd) {}
func terminateRPCProcess(command *exec.Cmd) {
	if command.Process != nil {
		_ = command.Process.Kill()
	}
}

func openRPCArtifactFile(path string) (*os.File, error) { return os.Open(path) }
