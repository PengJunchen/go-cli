//go:build !darwin && !linux

package tools

import (
	"errors"
	"os/exec"
)

var errProcessContainmentUnsupported = errors.New("process containment is unsupported on this operating system")

func configureCommandProcessGroup(_ *exec.Cmd) error {
	return errProcessContainmentUnsupported
}

func signalCommandProcessGroup(_ int, _ bool) error {
	return errProcessContainmentUnsupported
}

func commandProcessGroupAlive(_ int) (bool, error) {
	return false, errProcessContainmentUnsupported
}
