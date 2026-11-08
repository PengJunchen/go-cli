//go:build linux

package gatecapture

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func readNativeProcessIdentity(pid int) (nativeProcessIdentity, error) {
	if pid <= 0 {
		return nativeProcessIdentity{}, errors.New("process ID must be positive")
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nativeProcessIdentity{}, fmt.Errorf("read Linux process %d identity: %w", pid, err)
	}
	stat := string(data)
	closeParen := strings.LastIndexByte(stat, ')')
	if closeParen < 0 {
		return nativeProcessIdentity{}, fmt.Errorf("read Linux process %d identity: malformed proc stat", pid)
	}
	fields := strings.Fields(stat[closeParen+1:])
	// These fields start at proc stat field 3 (state). PPID is field 4,
	// process group is field 5, and the birth token is field 22.
	if len(fields) <= 19 {
		return nativeProcessIdentity{}, fmt.Errorf("read Linux process %d identity: incomplete proc stat", pid)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil || ppid < 0 {
		return nativeProcessIdentity{}, fmt.Errorf("read Linux process %d identity: invalid PPID", pid)
	}
	pgid, err := strconv.Atoi(fields[2])
	if err != nil || pgid <= 0 {
		return nativeProcessIdentity{}, fmt.Errorf("read Linux process %d identity: invalid process group", pid)
	}
	if fields[19] == "" {
		return nativeProcessIdentity{}, fmt.Errorf("read Linux process %d identity: empty birth token", pid)
	}
	return nativeProcessIdentity{PID: pid, PPID: ppid, PGID: pgid, BirthToken: "linux:" + fields[19]}, nil
}

func nativeProcessIsAbsent(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
