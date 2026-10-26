//go:build darwin || linux

package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const stdioEscapedHelperEnvPrefix = "GO_CLI_TEST_STDIO_ESCAPED_HELPER_"

func TestStdioProcessOwnerEscapedPipeHolderDoesNotBlockAdapterJoin(t *testing.T) {
	dir := t.TempDir()
	readyPath := filepath.Join(dir, "escaped-ready")
	releasePath := filepath.Join(dir, "escaped-release")
	finishedPath := filepath.Join(dir, "escaped-finished")
	pidPath := filepath.Join(dir, "escaped-pid")
	failedPath := filepath.Join(dir, "escaped-failed")
	var cleanupCalls atomic.Int32
	owner, err := NewStdioProcessOwner(StdioProcessConfig{
		Command: os.Args[0],
		Args:    []string{"-test.run=^TestStdioProcessOwnerEscapedPipeHolderHelper$"},
		Cleanup: func() error {
			cleanupCalls.Add(1)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	defer stdioOwnerCleanup(t, owner)
	owner.cmd.Env = stdioEscapedHelperEnv("leader", readyPath, releasePath, finishedPath, pidPath, failedPath)
	if err := owner.Start(); err != nil {
		t.Fatalf("start pipe-holder leader: %v", err)
	}
	if err := stdioOwnerWaitForFile(readyPath, 5*time.Second); err != nil {
		if _, statErr := os.Stat(failedPath); statErr == nil {
			contents, _ := os.ReadFile(failedPath)
			t.Fatalf("escaped helper failed: %s", contents)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(releasePath, []byte("release\n"), 0o600)
		if err := stdioOwnerWaitForFile(finishedPath, 5*time.Second); err != nil {
			t.Errorf("escaped pipe-holder did not exit after release: %v", err)
		}
	})

	client := NewStdioAdapter(owner.Reader(), owner.Writer(), WithStdioOwnedTransportCloser(owner))
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("Connect with an idle escaped pipe-holder: %v", err)
	}
	disconnectCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	disconnectErr := client.Disconnect(disconnectCtx)
	cancel()
	if disconnectErr != nil {
		var pending *StdioPendingError
		if errors.As(disconnectErr, &pending) {
			_ = os.WriteFile(releasePath, []byte("release\n"), 0o600)
			select {
			case <-pending.Done():
			case <-time.After(5 * time.Second):
				t.Fatalf("pending Disconnect did not finish after releasing escaped child: %v", disconnectErr)
			}
		}
		t.Fatalf("Disconnect remained pending while an escaped child held the owned pipe: %v", disconnectErr)
	}

	if got := cleanupCalls.Load(); got != 1 {
		t.Fatalf("process owner cleanup count = %d, want 1 after adapter join", got)
	}
	pidBytes, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read escaped child pid: %v", err)
	}
	escapedPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("parse escaped child pid: %v", err)
	}
	escapedProcess, err := os.FindProcess(escapedPID)
	if err != nil {
		t.Fatalf("find escaped child: %v", err)
	}
	if err := escapedProcess.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("escaped child did not remain outside the owned process group: %v", err)
	}
	owner.mu.Lock()
	ownerGroupID := owner.groupID
	groupGone := owner.groupGone
	owner.mu.Unlock()
	if !groupGone || escapedPID == ownerGroupID {
		t.Fatalf("process group result=%v, escaped pid=%d owner group=%d", groupGone, escapedPID, ownerGroupID)
	}
	stdioOwnerRequireJoinedState(t, owner)
}

// TestStdioProcessOwnerEscapedPipeHolderHelper is run only in subprocesses
// created by TestStdioProcessOwnerEscapedPipeHolderDoesNotBlockAdapterJoin.
func TestStdioProcessOwnerEscapedPipeHolderHelper(t *testing.T) {
	mode := os.Getenv(stdioEscapedHelperEnvPrefix + "MODE")
	if mode == "" {
		return
	}
	readyPath := os.Getenv(stdioEscapedHelperEnvPrefix + "READY")
	releasePath := os.Getenv(stdioEscapedHelperEnvPrefix + "RELEASE")
	finishedPath := os.Getenv(stdioEscapedHelperEnvPrefix + "FINISHED")
	pidPath := os.Getenv(stdioEscapedHelperEnvPrefix + "PID")
	failedPath := os.Getenv(stdioEscapedHelperEnvPrefix + "FAILED")

	switch mode {
	case "leader":
		cmd := exec.Command(os.Args[0], "-test.run=^TestStdioProcessOwnerEscapedPipeHolderHelper$")
		cmd.Env = stdioEscapedHelperEnv("escaped", readyPath, releasePath, finishedPath, pidPath, failedPath)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			_ = os.WriteFile(failedPath, []byte(err.Error()), 0o600)
			return
		}
		if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
			_ = cmd.Process.Kill()
			return
		}
		_ = cmd.Wait()
	case "escaped":
		if err := os.WriteFile(readyPath, []byte("ready\n"), 0o600); err != nil {
			_ = os.WriteFile(failedPath, []byte(err.Error()), 0o600)
			return
		}
		for {
			if _, err := os.Stat(releasePath); err == nil {
				_ = os.WriteFile(finishedPath, []byte("finished\n"), 0o600)
				return
			}
			time.Sleep(time.Millisecond)
		}
	default:
		_ = os.WriteFile(failedPath, []byte(fmt.Sprintf("unknown helper mode %q", mode)), 0o600)
	}
}

func stdioEscapedHelperEnv(mode, readyPath, releasePath, finishedPath, pidPath, failedPath string) []string {
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, stdioEscapedHelperEnvPrefix) {
			env = append(env, entry)
		}
	}
	return append(env,
		stdioEscapedHelperEnvPrefix+"MODE="+mode,
		stdioEscapedHelperEnvPrefix+"READY="+readyPath,
		stdioEscapedHelperEnvPrefix+"RELEASE="+releasePath,
		stdioEscapedHelperEnvPrefix+"FINISHED="+finishedPath,
		stdioEscapedHelperEnvPrefix+"PID="+pidPath,
		stdioEscapedHelperEnvPrefix+"FAILED="+failedPath,
	)
}

func TestStdioProcessOwnerCloseSettlesPermissionErrorAfterConfirmedAbsence(t *testing.T) {
	requireStdioProcessGroups(t)
	owner, err := NewStdioProcessOwner(StdioProcessConfig{
		Command: "/bin/sh",
		Args:    []string{"-c", "printf '%s\\n' early-exit-ready; exit 23", "stdio-owner-helper"},
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	defer stdioOwnerCleanup(t, owner)
	if err := owner.Start(); err != nil {
		t.Fatalf("start early-exit helper: %v", err)
	}
	if !stdioOwnerWaitChannel(owner.waitDone, 5*time.Second) {
		t.Fatal("direct child did not exit and reap")
	}
	owner.mu.Lock()
	groupID := owner.groupID
	owner.mu.Unlock()
	groupGone, probeErr := waitACPStdioProcessGroupGone(groupID, time.Second)
	if probeErr != nil || !groupGone {
		t.Fatalf("early-exit group absence = %v, err=%v", groupGone, probeErr)
	}

	var signalCalls atomic.Int32
	closeErr := owner.closeWith(stdioProcessGroupOps{
		signal: func(gotGroupID int, force bool) error {
			if gotGroupID != groupID || force {
				t.Errorf("unexpected signal request: group=%d force=%v", gotGroupID, force)
			}
			signalCalls.Add(1)
			return fmt.Errorf("injected transient permission denial: %w", syscall.EPERM)
		},
		waitGone: waitACPStdioProcessGroupGone,
	})
	if closeErr != nil {
		t.Fatalf("Close after confirmed group absence: %v", closeErr)
	}
	if got := signalCalls.Load(); got != 1 {
		t.Fatalf("signal calls = %d, want one TERM before actual group-absence confirmation", got)
	}
	joinErr := owner.Join()
	if joinErr == nil || !strings.Contains(joinErr.Error(), "exit status 23") {
		t.Fatalf("Join error = %v, want the direct child's exit status after reaping", joinErr)
	}
	if owner.cmd.ProcessState == nil || owner.cmd.ProcessState.ExitCode() != 23 {
		t.Fatalf("direct child process state = %#v, want exit code 23", owner.cmd.ProcessState)
	}
	stdioOwnerRequireJoinedState(t, owner)
}

func TestStdioProcessOwnerKeepsPermissionErrorWhileOwnedGroupLives(t *testing.T) {
	requireStdioProcessGroups(t)
	owner, err := NewStdioProcessOwner(StdioProcessConfig{Command: "/bin/sleep", Args: []string{"60"}})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := owner.Start(); err != nil {
		t.Fatalf("start live helper: %v", err)
	}
	owner.mu.Lock()
	groupID := owner.groupID
	owner.mu.Unlock()
	t.Cleanup(func() {
		if !stdioOwnerChannelClosed(owner.waitDone) {
			_ = signalACPStdioProcessGroup(groupID, true)
			_, _ = waitACPStdioProcessGroupGone(groupID, stdioProcessKillWait)
		}
		_ = owner.Join()
	})
	alive, probeErr := acpStdioProcessGroupAlive(groupID)
	if probeErr != nil || !alive {
		t.Fatalf("new helper process group alive=%v err=%v", alive, probeErr)
	}

	permissionErr := fmt.Errorf("injected permission denial: %w", syscall.EPERM)
	var observedProbes []string
	closeErr := owner.closeWith(stdioProcessGroupOps{
		signal: func(_ int, _ bool) error { return permissionErr },
		waitGone: func(id int, _ time.Duration) (bool, error) {
			alive, err := acpStdioProcessGroupAlive(id)
			observedProbes = append(observedProbes, fmt.Sprintf("alive=%v err=%v", alive, err))
			return !alive, err
		},
	})
	if !errors.Is(closeErr, syscall.EPERM) || !errors.Is(closeErr, ErrStdioProcessCleanupPending) {
		owner.mu.Lock()
		finalGroupGone := owner.groupGone
		processState := owner.cmd.ProcessState
		owner.mu.Unlock()
		finalAlive, finalErr := acpStdioProcessGroupAlive(groupID)
		t.Fatalf("Close with a still-live group error = %v, want EPERM and cleanup pending; probes=%v finalAlive=%v finalErr=%v groupGone=%v processState=%v waitDone=%v", closeErr, observedProbes, finalAlive, finalErr, finalGroupGone, processState, stdioOwnerChannelClosed(owner.waitDone))
	}
	owner.mu.Lock()
	groupGone := owner.groupGone
	owner.mu.Unlock()
	if groupGone {
		t.Fatal("permission denial with a live process group was treated as quiescent")
	}
	joinErr := owner.Join()
	if !errors.Is(joinErr, syscall.EPERM) || !errors.Is(joinErr, ErrStdioProcessCleanupPending) {
		t.Fatalf("Join with an unproven live group error = %v, want EPERM and cleanup pending", joinErr)
	}
}

func TestStopACPStdioProcessGroupRetainsPermissionAndProbeErrors(t *testing.T) {
	requireStdioProcessGroups(t)
	probeSentinel := errors.New("injected group inspection failure")
	permissionErr := fmt.Errorf("injected permission denial: %w", syscall.EPERM)
	groupGone, signalErr, probeErr := stopACPStdioProcessGroup(12345, stdioProcessGroupOps{
		signal: func(_ int, _ bool) error { return permissionErr },
		waitGone: func(_ int, _ time.Duration) (bool, error) {
			return false, probeSentinel
		},
	})
	if groupGone || !errors.Is(signalErr, syscall.EPERM) || !errors.Is(probeErr, probeSentinel) {
		t.Fatalf("group=%v signalErr=%v probeErr=%v, want all cleanup uncertainty retained", groupGone, signalErr, probeErr)
	}
}

func TestStopACPStdioProcessGroupRetainsNonPermissionSignalErrorAfterAbsence(t *testing.T) {
	requireStdioProcessGroups(t)
	signalSentinel := errors.New("injected non-permission signal failure")
	groupGone, signalErr, probeErr := stopACPStdioProcessGroup(12345, stdioProcessGroupOps{
		signal: func(_ int, _ bool) error { return signalSentinel },
		waitGone: func(_ int, _ time.Duration) (bool, error) {
			return true, nil
		},
	})
	if !groupGone || !errors.Is(signalErr, signalSentinel) || probeErr != nil {
		t.Fatalf("group=%v signalErr=%v probeErr=%v, want confirmed absence and retained non-EPERM signal failure", groupGone, signalErr, probeErr)
	}
}
