//go:build !darwin && !linux

package tools

import "os/exec"

func configureCommandProcessGroup(_ *exec.Cmd) error {
	return errProcessContainmentUnsupported
}

func signalCommandProcessGroup(_ int, _ bool) error {
	return errProcessContainmentUnsupported
}

func commandProcessGroupAlive(_ int) (bool, error) {
	return false, errProcessContainmentUnsupported
}
