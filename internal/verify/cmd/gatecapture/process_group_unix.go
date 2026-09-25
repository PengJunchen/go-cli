//go:build darwin || linux

package gatecapture

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const (
	processTerminateGrace = 100 * time.Millisecond
	processCleanupBudget  = 500 * time.Millisecond
	processPollInterval   = 10 * time.Millisecond
)

type unixProcessGroup struct {
	cmd *exec.Cmd
}

func newOwnedProcessGroup(cmd *exec.Cmd) (processGroup, error) {
	if cmd.Process != nil {
		return nil, ErrProcessAlreadyStarted
	}
	attrs := cmd.SysProcAttr
	if attrs == nil {
		attrs = &syscall.SysProcAttr{}
		cmd.SysProcAttr = attrs
	}
	if attrs.Pgid != 0 {
		return nil, ErrForeignProcessGroup
	}
	if attrs.Setsid && attrs.Setpgid {
		return nil, ErrProcessGroupConfiguration
	}
	if !attrs.Setsid {
		attrs.Setpgid = true
	}
	return &unixProcessGroup{cmd: cmd}, nil
}

func (group *unixProcessGroup) processGroupID() int {
	if group == nil || group.cmd == nil || group.cmd.Process == nil {
		return 0
	}
	return group.cmd.Process.Pid
}

func (group *unixProcessGroup) terminate() error {
	pgid := group.processGroupID()
	if pgid <= 0 {
		return os.ErrProcessDone
	}
	return signalOwnedProcessGroup(pgid, syscall.SIGTERM)
}

func (group *unixProcessGroup) cleanup() (bool, error) {
	pgid := group.processGroupID()
	if pgid <= 0 {
		return false, nil
	}
	alive, checkErr := ownedProcessGroupAlive(pgid)
	if checkErr == nil && !alive {
		return false, nil
	}
	deadline := time.Now().Add(processCleanupBudget)
	terminateDeadline := time.Now().Add(processTerminateGrace)
	if terminateDeadline.After(deadline) {
		terminateDeadline = deadline
	}
	terminateErr := signalOwnedProcessGroup(pgid, syscall.SIGTERM)
	stopped, waitErr := waitOwnedProcessGroupStopped(pgid, terminateDeadline)
	if stopped {
		return true, nil
	}
	checkErr = errors.Join(checkErr, waitErr)
	killErr := signalOwnedProcessGroup(pgid, syscall.SIGKILL)
	stopped, waitErr = waitOwnedProcessGroupStopped(pgid, deadline)
	if stopped {
		return true, nil
	}
	checkErr = errors.Join(checkErr, waitErr)
	cleanupErr := fmt.Errorf("%w: owned group %d may still contain active processes: %v", ErrProcessCleanupUnconfirmed, pgid, errors.Join(checkErr, terminateErr, killErr))
	return true, cleanupErr
}

func signalOwnedProcessGroup(pgid int, signal syscall.Signal) error {
	if err := syscall.Kill(-pgid, signal); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}

func ownedProcessGroupAlive(pgid int) (bool, error) {
	if err := syscall.Kill(-pgid, 0); err != nil {
		if errors.Is(err, syscall.EPERM) {
			return true, nil
		}
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		return true, err
	}
	return true, nil
}

func waitOwnedProcessGroupStopped(pgid int, deadline time.Time) (bool, error) {
	var lastErr error
	for {
		alive, err := ownedProcessGroupAlive(pgid)
		if err == nil && !alive {
			return true, nil
		}
		if err != nil {
			lastErr = err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, lastErr
		}
		if remaining > processPollInterval {
			remaining = processPollInterval
		}
		time.Sleep(remaining)
	}
}
