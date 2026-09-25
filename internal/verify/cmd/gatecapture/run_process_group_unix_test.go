//go:build darwin || linux

package gatecapture

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunStopsOwnedDescendantsOnTimeout(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "holder.pid")
	t.Cleanup(func() { killFixtureProcess(pidFile) })
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := runFixtureWithEnv(ctx, t, "spawn-ignore-term-and-wait", "VERIFY_GATE_CAPTURE_PID_FILE="+pidFile, Limits{
		StdoutBytes: 1024,
		StderrBytes: 1024,
		WaitDelay:   150 * time.Millisecond,
	})
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("fixture context error = %v, want deadline exceeded (err=%v)", ctx.Err(), err)
	}
	if !errors.Is(err, ErrResidualDescendants) {
		t.Fatalf("Run() error = %v, want report that it cleaned a residual descendant", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Run() took %s with a 150ms timeout and WaitDelay", elapsed)
	}
	assertFixtureProcessStopped(t, pidFile)
}

func TestRunCleansDescendantsAfterLeaderExit(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "holder.pid")
	t.Cleanup(func() { killFixtureProcess(pidFile) })
	started := time.Now()
	_, err := runFixtureWithEnv(context.Background(), t, "spawn-holder", "VERIFY_GATE_CAPTURE_PID_FILE="+pidFile, Limits{
		StdoutBytes: 1024,
		StderrBytes: 1024,
		WaitDelay:   150 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("Run() returned success after the command left a pipe-holding descendant")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Run() took %s with a 150ms WaitDelay", elapsed)
	}
	assertFixtureProcessStopped(t, pidFile)
}

func TestRunStopsDescendantHoldingPipesAfterContextTimeout(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "holder.pid")
	t.Cleanup(func() { killFixtureProcess(pidFile) })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	result, err := runFixtureWithEnv(ctx, t, "spawn-holder", "VERIFY_GATE_CAPTURE_PID_FILE="+pidFile, Limits{
		StdoutBytes: 1024,
		StderrBytes: 1024,
		WaitDelay:   150 * time.Millisecond,
	})
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("fixture context error = %v, want deadline exceeded (result=%+v err=%v)", ctx.Err(), result, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Run() took %s with a 100ms timeout and 150ms WaitDelay", elapsed)
	}
	assertFixtureProcessStopped(t, pidFile)
}

func TestRunRejectsForeignProcessGroupBeforeStart(t *testing.T) {
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: 1}

	_, err := Run(cmd, Limits{StdoutBytes: 32, StderrBytes: 32})
	if !errors.Is(err, ErrForeignProcessGroup) {
		t.Fatalf("Run() error = %v, want foreign process group rejection", err)
	}
	if cmd.Process != nil {
		t.Fatal("Run() started a command that requested a foreign process group")
	}
}

func TestRunRejectsAlreadyStartedCommand(t *testing.T) {
	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fixture command: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	_, err := Run(cmd, Limits{StdoutBytes: 32, StderrBytes: 32})
	if !errors.Is(err, ErrProcessAlreadyStarted) {
		t.Fatalf("Run() error = %v, want already-started rejection", err)
	}
}

func assertFixtureProcessStopped(t *testing.T, pidFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read fixture pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse fixture pid: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !fixtureProcessRunning(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("owned descendant pid %d remained active after Run returned", pid)
}

func fixtureProcessRunning(pid int) bool {
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat")); err == nil {
			if closeParen := strings.LastIndexByte(string(data), ')'); closeParen >= 0 {
				fields := strings.Fields(string(data)[closeParen+1:])
				if len(fields) > 0 && (fields[0] == "Z" || fields[0] == "X") {
					return false
				}
			}
		}
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func killFixtureProcess(pidFile string) {
	data, err := os.ReadFile(pidFile)
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
