//go:build darwin

package tools

import (
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func currentHostGeneration() (string, error) {
	boot, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return "", fmt.Errorf("read Darwin boot generation: %w", err)
	}
	kernelUUID, err := unix.Sysctl("kern.uuid")
	if err != nil {
		return "", fmt.Errorf("read Darwin host generation: %w", err)
	}
	return fmt.Sprintf("darwin:%s:%d.%06d", strings.TrimSpace(kernelUUID), boot.Sec, boot.Usec), nil
}

func processStartToken(pid int) (string, int, error) {
	if pid <= 0 {
		return "", 0, fmt.Errorf("process ID must be positive")
	}
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", 0, fmt.Errorf("inspect process %d start token: %w", pid, err)
	}
	if int(process.Proc.P_pid) != pid || process.Proc.P_starttime.Sec <= 0 || process.Eproc.Pgid <= 0 {
		return "", 0, fmt.Errorf("inspect process %d: kernel returned incomplete identity", pid)
	}
	startToken := strconv.FormatInt(process.Proc.P_starttime.Sec, 10) + "." + strconv.FormatInt(int64(process.Proc.P_starttime.Usec), 10)
	return startToken, int(process.Eproc.Pgid), nil
}
