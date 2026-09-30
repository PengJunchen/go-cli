//go:build linux

package core

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func inspectPlatformProcessGroupExit(identity ExecutionIdentity) ProcessGroupExitInspection {
	if identity.Platform != "linux" {
		return unknownProcessGroupInspection("execution identity belongs to another platform")
	}
	if identity.PIDNamespaceToken == "" {
		return unknownProcessGroupInspection("PID namespace identity is missing")
	}
	hostGeneration, err := currentLinuxHostGeneration()
	if err != nil {
		return unknownProcessGroupInspection("Linux host generation is unavailable")
	}
	pidNamespaceToken, err := currentLinuxPIDNamespaceToken()
	if err != nil {
		return unknownProcessGroupInspection("Linux PID namespace identity is unavailable")
	}
	return inspectLinuxProcessGroupExit(identity, hostGeneration, pidNamespaceToken, inspectLinuxLeader, probeLinuxProcessGroup)
}

// CurrentPIDNamespaceToken returns a stable token for the current Linux PID
// namespace. It is intended to be persisted with an ExecutionIdentity before
// that identity is used for post-restart inspection.
func CurrentPIDNamespaceToken() (string, error) {
	return currentLinuxPIDNamespaceToken()
}

func currentLinuxHostGeneration() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("read Linux boot generation: %w", err)
	}
	bootID := strings.TrimSpace(string(data))
	if bootID == "" {
		return "", errors.New("Linux boot generation is empty")
	}
	return "linux:" + bootID, nil
}

func currentLinuxPIDNamespaceToken() (string, error) {
	currentNamespace, err := os.Stat("/proc/self/ns/pid")
	if err != nil {
		return "", fmt.Errorf("inspect current PID namespace: %w", err)
	}
	initNamespace, err := os.Stat("/proc/1/ns/pid")
	if err != nil {
		return "", fmt.Errorf("inspect PID namespace init: %w", err)
	}
	if !os.SameFile(currentNamespace, initNamespace) {
		return "", errors.New("procfs view does not match the current PID namespace")
	}
	namespaceStat, ok := currentNamespace.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("current PID namespace has an unsupported stat type")
	}
	initStartToken, _, err := readLinuxProcessStat(1)
	if err != nil {
		return "", fmt.Errorf("inspect PID namespace init start token: %w", err)
	}
	return fmt.Sprintf("linux:pidns:v1:%x:%x:init:%s", uint64(namespaceStat.Dev), namespaceStat.Ino, initStartToken), nil
}

func inspectLinuxLeader(pid int) (string, int, string, error) {
	if pid <= 0 {
		return "", 0, "", errors.New("process ID must be positive")
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", 0, "", err
	}
	return parseLinuxProcessStatDetails(data)
}

func readLinuxProcessStat(pid int) (string, int, error) {
	if pid <= 0 {
		return "", 0, errors.New("process ID must be positive")
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", 0, err
	}
	return parseLinuxProcessStat(data)
}

func parseLinuxProcessStat(data []byte) (string, int, error) {
	startToken, processGroupID, _, err := parseLinuxProcessStatDetails(data)
	return startToken, processGroupID, err
}

func parseLinuxProcessStatDetails(data []byte) (string, int, string, error) {
	contents := string(data)
	close := strings.LastIndex(contents, ")")
	if close < 0 {
		return "", 0, "", errors.New("process stat is malformed")
	}
	fields := strings.Fields(contents[close+1:])
	// Fields start at proc stat field 3 (state); pgrp is field 5 and
	// starttime is field 22.
	if len(fields) <= 19 {
		return "", 0, "", errors.New("process stat is incomplete")
	}
	if len(fields[0]) != 1 {
		return "", 0, "", errors.New("process stat has an invalid state")
	}
	processGroupID, err := strconv.Atoi(fields[2])
	if err != nil || processGroupID <= 0 {
		return "", 0, "", errors.New("process stat has an invalid process group")
	}
	if fields[19] == "" {
		return "", 0, "", errors.New("process stat has an empty start token")
	}
	return "linux:" + fields[19], processGroupID, fields[0], nil
}

// probeLinuxProcessGroup performs the signal-zero existence check on the
// negative process-group ID. Signal 0 is a kernel permission/existence probe;
// it delivers no signal and never targets the saved leader PID.
func probeLinuxProcessGroup(processGroupID int) error {
	if processGroupID <= 1 {
		return errors.New("process group ID cannot be probed safely")
	}
	return unix.Kill(-processGroupID, 0)
}

func processGroupInspection(status ProcessGroupExitStatus, reason string) ProcessGroupExitInspection {
	return ProcessGroupExitInspection{Status: status, Reason: reason, ObservedAt: time.Now().UTC()}
}

func inspectLinuxProcessGroupExit(
	identity ExecutionIdentity,
	currentHostGeneration string,
	currentPIDNamespaceToken string,
	leaderProbe func(int) (string, int, string, error),
	groupProbe func(int) error,
) ProcessGroupExitInspection {
	if err := identity.Validate(); err != nil || identity.Kind != ExecutionIdentityProcessGroup || identity.Platform != "linux" {
		return unknownProcessGroupInspection("invalid Linux process-group identity")
	}
	if identity.PIDNamespaceToken == "" {
		return unknownProcessGroupInspection("PID namespace identity is missing")
	}
	if currentHostGeneration == "" {
		return unknownProcessGroupInspection("Linux host generation is unavailable")
	}
	if currentHostGeneration != identity.HostGeneration {
		return unknownProcessGroupInspection("Linux host generation does not match")
	}
	if currentPIDNamespaceToken == "" {
		return unknownProcessGroupInspection("Linux PID namespace identity is unavailable")
	}
	if currentPIDNamespaceToken != identity.PIDNamespaceToken {
		return unknownProcessGroupInspection("Linux PID namespace does not match")
	}
	if identity.ProcessGroupID <= 1 {
		return unknownProcessGroupInspection("recorded process group ID cannot be probed safely")
	}
	if leaderProbe == nil || groupProbe == nil {
		return unknownProcessGroupInspection("process identity probe is unavailable")
	}
	leaderStartToken, leaderProcessGroupID, leaderState, err := leaderProbe(identity.LeaderPID)
	leaderIdentityMatches := false
	leaderIsActive := false
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return unknownProcessGroupInspection("saved process leader is inaccessible")
	}
	if err == nil && leaderStartToken == identity.ProcessStartToken {
		if leaderProcessGroupID != identity.ProcessGroupID {
			return unknownProcessGroupInspection("saved process leader is still alive outside its recorded group")
		}
		leaderIdentityMatches = true
		leaderIsActive = activeLinuxProcessState(leaderState)
	}
	pidMax, err := currentLinuxPIDMax()
	if err != nil {
		return unknownProcessGroupInspection("Linux PID limit is unavailable")
	}
	if identity.ProcessGroupID >= pidMax {
		return unknownProcessGroupInspection("recorded process group ID is outside the allocatable Linux PID range")
	}
	groupProbeErr := groupProbe(identity.ProcessGroupID)
	if groupProbeErr == nil {
		if !leaderIdentityMatches {
			return unknownProcessGroupInspection("process group ID exists but the saved leader identity is absent or reused")
		}
		if !leaderIsActive {
			return unknownProcessGroupInspection("saved process leader is not active; zombie-only process groups cannot be resolved safely")
		}
		if !linuxLeaderStillMatches(identity, leaderProbe) {
			return unknownProcessGroupInspection("saved process leader changed during process-group inspection")
		}
		return processGroupInspection(ProcessGroupExitPresent, "kernel reports that the process group exists")
	}
	if errors.Is(groupProbeErr, syscall.ESRCH) {
		if leaderIdentityMatches {
			leaderStartToken, _, _, err := leaderProbe(identity.LeaderPID)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return unknownProcessGroupInspection("saved process leader is inaccessible after process-group inspection")
			}
			if err == nil && leaderStartToken == identity.ProcessStartToken {
				return unknownProcessGroupInspection("kernel reports no group but the saved process leader identity remains")
			}
		}
		return processGroupInspection(ProcessGroupExitAbsent, "kernel reports that the process group does not exist")
	}
	return unknownProcessGroupInspection("kernel could not confirm process-group absence")
}

func linuxLeaderStillMatches(identity ExecutionIdentity, leaderProbe func(int) (string, int, string, error)) bool {
	leaderStartToken, leaderProcessGroupID, leaderState, err := leaderProbe(identity.LeaderPID)
	if err != nil {
		return false
	}
	return leaderStartToken == identity.ProcessStartToken && leaderProcessGroupID == identity.ProcessGroupID && activeLinuxProcessState(leaderState)
}

func activeLinuxProcessState(state string) bool {
	switch state {
	case "R", "S", "D", "T", "t", "W", "K", "P", "I":
		return true
	default:
		return false
	}
}

func currentLinuxPIDMax() (int, error) {
	data, err := os.ReadFile("/proc/sys/kernel/pid_max")
	if err != nil {
		return 0, err
	}
	pidMax, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pidMax <= 0 {
		return 0, errors.New("Linux PID limit is invalid")
	}
	return pidMax, nil
}
