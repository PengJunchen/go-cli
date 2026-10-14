//go:build linux

package tools

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func currentHostGeneration() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("read Linux boot generation: %w", err)
	}
	bootID := strings.TrimSpace(string(data))
	if bootID == "" {
		return "", errors.New("tools: boot generation on Linux is empty")
	}
	return "linux:" + bootID, nil
}

func processStartToken(pid int) (string, int, error) {
	if pid <= 0 {
		return "", 0, errors.New("process ID must be positive")
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", 0, fmt.Errorf("inspect process %d start token: %w", pid, err)
	}
	stat := string(data)
	close := strings.LastIndex(stat, ")")
	if close < 0 {
		return "", 0, fmt.Errorf("inspect process %d: malformed proc stat", pid)
	}
	fields := strings.Fields(stat[close+1:])
	// Fields start at proc stat field 3 (state); pgrp is field 5 and
	// starttime is field 22.
	if len(fields) <= 19 {
		return "", 0, fmt.Errorf("inspect process %d: incomplete proc stat", pid)
	}
	processGroupID, err := strconv.Atoi(fields[2])
	if err != nil || processGroupID <= 0 {
		return "", 0, fmt.Errorf("inspect process %d: invalid process group in proc stat", pid)
	}
	if fields[19] == "" {
		return "", 0, fmt.Errorf("inspect process %d: empty start token in proc stat", pid)
	}
	return "linux:" + fields[19], processGroupID, nil
}
