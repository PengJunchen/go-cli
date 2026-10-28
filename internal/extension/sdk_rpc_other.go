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

// The ordinary platform scope is the reaped leader and joined stdio. Request
// completion never claims a process-tree sandbox on these platforms.
func rpcProcessGroupStopped(command *exec.Cmd) bool {
	return command != nil && command.ProcessState != nil
}
