//go:build !darwin && !linux

package acp

import (
	"os/exec"
)

func stdioProcessGroupSupported() error { return ErrStdioProcessContainmentUnsupported }

func configureACPStdioProcessGroup(*exec.Cmd) error { return ErrStdioProcessContainmentUnsupported }

func signalACPStdioProcessGroup(int, bool) error { return ErrStdioProcessContainmentUnsupported }

func acpStdioProcessGroupAlive(int) (bool, error) {
	return false, ErrStdioProcessContainmentUnsupported
}

func expectedACPStdioShutdown(error) bool { return false }
