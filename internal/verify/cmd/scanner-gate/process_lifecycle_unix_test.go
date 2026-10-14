//go:build darwin || linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func processGroupsSupported() bool { return true }

func assertDescendantStoppedFromFile(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read descendant pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse descendant pid %q: %v", data, err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !descendantRunning(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("owned descendant pid %d remained active after gate returned", pid)
}

func cleanupDescendantFromFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return
	}
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid > 1 {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return
	}
	process, err := os.FindProcess(pid)
	if err == nil {
		_ = process.Kill()
	}
}

func descendantRunning(pid int) bool {
	if data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat")); err == nil {
		if closeParen := strings.LastIndexByte(string(data), ')'); closeParen >= 0 {
			fields := strings.Fields(string(data)[closeParen+1:])
			if len(fields) > 0 && (fields[0] == "Z" || fields[0] == "X") {
				return false
			}
		}
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
