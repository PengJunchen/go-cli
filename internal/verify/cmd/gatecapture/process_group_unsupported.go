//go:build !darwin && !linux

package gatecapture

import "os/exec"

func newOwnedProcessGroup(cmd *exec.Cmd) (processGroup, error) {
	if cmd != nil && cmd.Process != nil {
		return nil, ErrProcessAlreadyStarted
	}
	return nil, ErrProcessGroupUnsupported
}
