//go:build !darwin && !linux

package tools

import (
	"context"
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

func startManagedLocalBashCommandWithIdentityObserver(_ context.CancelFunc, _ *exec.Cmd, _ context.Context, _ ProcessExecutionIdentityObserver) (*managedLocalCommand, error) {
	return nil, errProcessContainmentUnsupported
}
