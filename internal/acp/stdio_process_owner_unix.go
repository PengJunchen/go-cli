//go:build darwin || linux

package acp

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

func stdioProcessGroupSupported() error { return nil }

func configureACPStdioProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil {
		return errors.New("acp: stdio command is nil")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

func signalACPStdioProcessGroup(groupID int, force bool) error {
	if groupID <= 0 {
		return errors.New("acp: stdio process group identity is unavailable")
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	if err := syscall.Kill(-groupID, signal); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("acp: signal stdio process group: %w", err)
	}
	return nil
}

func acpStdioProcessGroupAlive(groupID int) (bool, error) {
	if groupID <= 0 {
		return false, errors.New("acp: stdio process group identity is unavailable")
	}
	if err := syscall.Kill(-groupID, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		if errors.Is(err, syscall.EPERM) {
			return true, nil
		}
		return false, fmt.Errorf("acp: inspect stdio process group: %w", err)
	}
	return true, nil
}

func isACPStdioProcessGroupPermissionError(err error) bool {
	return errors.Is(err, syscall.EPERM)
}

func expectedACPStdioShutdown(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && (status.Signal() == syscall.SIGTERM || status.Signal() == syscall.SIGKILL)
}
