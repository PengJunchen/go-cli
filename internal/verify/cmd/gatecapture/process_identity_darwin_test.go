//go:build darwin

package gatecapture

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

func readNativeProcessIdentity(pid int) (nativeProcessIdentity, error) {
	if pid <= 0 {
		return nativeProcessIdentity{}, errors.New("process ID must be positive")
	}
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.EIO) {
			probeErr := unix.Kill(pid, 0)
			if errors.Is(probeErr, unix.ESRCH) {
				return nativeProcessIdentity{}, fmt.Errorf("read Darwin process %d identity: %w", pid, os.ErrNotExist)
			}
			if probeErr != nil && !errors.Is(probeErr, unix.EPERM) {
				return nativeProcessIdentity{}, fmt.Errorf("read Darwin process %d identity: %w", pid, errors.Join(err, probeErr))
			}
		}
		if errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT) {
			return nativeProcessIdentity{}, fmt.Errorf("read Darwin process %d identity: %w", pid, os.ErrNotExist)
		}
		return nativeProcessIdentity{}, fmt.Errorf("read Darwin process %d identity: %w", pid, err)
	}
	start := process.Proc.P_starttime
	ppid := int(process.Eproc.Ppid)
	pgid := int(process.Eproc.Pgid)
	if int(process.Proc.P_pid) != pid || ppid < 0 || pgid <= 0 || start.Sec <= 0 || start.Usec < 0 || start.Usec >= 1_000_000 {
		return nativeProcessIdentity{}, fmt.Errorf("read Darwin process %d identity: incomplete kernel identity", pid)
	}
	token := "darwin:" + strconv.FormatInt(start.Sec, 10) + "." + strconv.FormatInt(int64(start.Usec), 10)
	return nativeProcessIdentity{PID: pid, PPID: ppid, PGID: pgid, BirthToken: token}, nil
}

func nativeProcessIsAbsent(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
