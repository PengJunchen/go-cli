//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
)

func TestDurableCLIBinaryKeepsQuarantineWhenEscapedDescendantOutlivesProcessGroup(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	binary := filepath.Join(t.TempDir(), "go-cli")
	build := exec.Command("go", "build", "-o", binary, "./cmd/cli")
	build.Dir = root
	buildOutput, err := build.CombinedOutput()
	require.NoError(t, err, "build real CLI: %s", buildOutput)
	t.Logf("build command: go build -o %s ./cmd/cli\nbuild exit: 0\nbuild output: %s", binary, buildOutput)

	fixture := newDurableCLIBinaryFixtureWithTools(t, binary, false, []string{"bash", "write"})
	t.Cleanup(func() { fixture.server.Close() })
	escaped := newDurableCLIEscapedDescendant(t, fixture.workDir)
	var cliCommand *exec.Cmd
	var cliWait <-chan error
	var processGroupID int
	var processLeaderPID int
	var escapedPID int
	restoreSubreaper := enableChildSubreaper(t)
	t.Cleanup(func() {
		if cliCommand != nil && cliCommand.Process != nil {
			_ = cliCommand.Process.Kill()
			if cliWait != nil {
				select {
				case <-cliWait:
				case <-time.After(3 * time.Second):
				}
			}
		}
		if processGroupID > 0 {
			_ = syscall.Kill(-processGroupID, syscall.SIGKILL)
		}
		if processLeaderPID > 0 {
			_ = waitAndReapChild(processLeaderPID, 3*time.Second)
		}
		_ = escaped.release()
		_ = escaped.waitForExit(3 * time.Second)
		if escapedPID > 0 {
			_ = waitAndReapChild(escapedPID, 3*time.Second)
		}
		restoreSubreaper()
	})

	fixture.peer.setResponsePlan(
		durableCLIToolCall("call-escaped-bash", "bash", map[string]string{"command": escaped.command()}),
		durableCLIFinal("resumed while detached descendant remains unresolved"),
	)
	command, stdout, stderr, wait := fixture.start(t,
		"prompt", "-provider=openai", "-output=text", "-approve=auto", "-no-sandbox", "run escaped descendant")
	cliCommand, cliWait = command, wait
	physicalID := waitForDurableCLIOutput(t, stdout, "Session ID: ")
	rows, action := waitForDurableBashDispatch(t, fixture, physicalID, "call-escaped-bash")
	execution := requireDurableBashExecution(t, rows, action)
	require.NotNil(t, execution.ExecutionIdentity)
	identity := *execution.ExecutionIdentity
	require.NoError(t, identity.Validate())
	require.NotEmpty(t, identity.PIDNamespaceToken, "Linux process identity is bound to the captured PID namespace")
	processGroupID = identity.ProcessGroupID
	processLeaderPID = identity.LeaderPID

	escaped.waitForStarted(t, 10*time.Second)
	escapedPID = escaped.childPID(t)
	escaped.waitForEffect(t, 10*time.Second)
	heartbeat := escaped.waitForHeartbeatAfter(t, 0, 10*time.Second)
	requireWorkspaceActionLifecycleForCall(t, fixture.readWorkspaceIndex(t, action.workspaceID), physicalID, action.actionID, action.toolCallID, 1, 0)
	require.Empty(t, receiptsForCall(rows, action.toolCallID), "Bash has no completion receipt before the CLI is killed")

	require.NoError(t, command.Process.Kill(), "SIGKILL only the owned CLI parent")
	firstErr := <-wait
	first := durableCLIChildResult{stdout: stdout.String(), stderr: stderr.String(), err: firstErr, exitCode: command.ProcessState.ExitCode(), state: command.ProcessState.String()}
	requireCLIChildExit(t, first, -1)

	// This signal is sent by the test harness to finish the original fixture
	// group. The detached child has its own session and must remain alive.
	require.NoError(t, syscall.Kill(-identity.ProcessGroupID, syscall.SIGKILL), "terminate only the original Bash process group")
	escaped.waitForHeartbeatAfter(t, heartbeat, 10*time.Second)
	require.NoError(t, waitAndReapChild(identity.LeaderPID, 10*time.Second), "reap the dead owned Bash leader so this isolated test runner can observe PGID absence")
	waitForAbsentProcessGroup(t, identity, 15*time.Second)
	t.Logf("fixture boundary physical_id=%s action_id=%s owner=%s old_pgid=%d old_pgid=absent detached_child=alive heartbeat=%d", physicalID, action.actionID, execution.OwnerGeneration, identity.ProcessGroupID, heartbeat)

	// Open the same durable session directly so the read-only restart diagnostic
	// is observable before the real CLI performs its normal resume path.
	t.Setenv("HOME", fixture.home)
	t.Setenv("XDG_CONFIG_HOME", fixture.configDir)
	index, err := session.NewWorkspaceQuarantineIndex()
	require.NoError(t, err)
	rows = fixture.readRows(t, physicalID)
	require.Empty(t, receiptsForCall(rows, action.toolCallID), "the killed CLI must leave the dispatched Bash action unreceipted")
	prepared, err := session.NewDurableJSONLSessionPreparer(fixture.storeDir, index).PrepareResume(context.Background(), physicalID)
	require.NoError(t, err, "prepare exact durable session after old PGID disappeared")
	view := prepared.View()
	require.Len(t, view.ExecutionRecoveryDiagnostics, 1)
	diagnostic := view.ExecutionRecoveryDiagnostics[0]
	require.Equal(t, action.actionID, diagnostic.Action.ActionID)
	require.Equal(t, action.toolCallID, diagnostic.Action.ToolCallID)
	require.Equal(t, execution.ID, diagnostic.ExecutionEntryID)
	require.Equal(t, execution.OwnerGeneration, diagnostic.OwnerGeneration)
	require.Equal(t, core.ProcessGroupExitAbsent, diagnostic.Status)
	require.NotEmpty(t, diagnostic.Reason)
	require.NoError(t, prepared.Close())

	rows = fixture.readRows(t, physicalID)
	require.Equal(t, "unknown", requireRecoveredUnknownBashReceipt(t, rows, physicalID, action).ReceiptOutcome)
	for _, row := range rows {
		require.NotEqual(t, session.CheckpointExecutionResolution, row.CheckpointKind, "PGID absence is not proof that an escaped descendant stopped")
	}
	requireWorkspaceActionLifecycleForCall(t, fixture.readWorkspaceIndex(t, action.workspaceID), physicalID, action.actionID, action.toolCallID, 1, 0)
	escaped.waitForHeartbeatAfter(t, heartbeat, 10*time.Second)

	resumed := fixture.run(t,
		"prompt", "-provider=openai", "-output=text", "-approve=auto", "-no-sandbox", "-resume-session", physicalID, "continue without replay")
	requireCLIChildExit(t, resumed, 0)
	require.Equal(t, physicalID, durableCLIPhysicalSessionIDFromOutput(t, resumed.stdout))
	require.Contains(t, resumed.stdout, "resumed while detached descendant remains unresolved")
	rows = fixture.readRows(t, physicalID)
	require.Equal(t, "unknown", requireRecoveredUnknownBashReceipt(t, rows, physicalID, action).ReceiptOutcome)
	require.Len(t, receiptsForCall(rows, action.toolCallID), 1, "the exact Bash occurrence retains one unknown receipt")
	requireWorkspaceActionLifecycleForCall(t, fixture.readWorkspaceIndex(t, action.workspaceID), physicalID, action.actionID, action.toolCallID, 1, 0)
	require.Len(t, fixture.assertPeerRequests(t, 2), 2, "resume does not dispatch the original Bash call again")

	require.NoError(t, escaped.release())
	escaped.waitForLateEffect(t, 10*time.Second)
	require.NoError(t, escaped.waitForExit(10*time.Second))
	require.NoError(t, waitAndReapChild(escapedPID, 10*time.Second), "reap the fixture-owned detached child after its late effect")
	requireWorkspaceActionLifecycleForCall(t, fixture.readWorkspaceIndex(t, action.workspaceID), physicalID, action.actionID, action.toolCallID, 1, 0)
	t.Logf("escaped descendant completed a late effect after recovery; the unknown receipt and workspace quarantine remain unresolved")
}

func enableChildSubreaper(t *testing.T) func() {
	t.Helper()
	var previous int32
	err := unix.Prctl(unix.PR_GET_CHILD_SUBREAPER, uintptr(unsafe.Pointer(&previous)), 0, 0, 0)
	runtime.KeepAlive(&previous)
	require.NoError(t, err, "read Linux child-subreaper state")
	require.NoError(t, unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0), "make the isolated test process reap its own orphaned fixtures")
	return func() {
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, uintptr(previous), 0, 0, 0); err != nil {
			t.Errorf("restore Linux child-subreaper state: %v", err)
		}
	}
}

func waitAndReapChild(processID int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var status unix.WaitStatus
		waited, err := unix.Wait4(processID, &status, unix.WNOHANG, nil)
		if err == nil && waited == processID {
			return nil
		}
		if err != nil && !errors.Is(err, syscall.ECHILD) && !errors.Is(err, syscall.EINTR) {
			return fmt.Errorf("wait for fixture child %d: %w", processID, err)
		}
		if errors.Is(err, syscall.ECHILD) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("fixture child %d did not exit within %s", processID, timeout)
}

func requireDurableBashExecution(t *testing.T, rows []session.SessionEntry, action durableCLIAction) session.SessionEntry {
	t.Helper()
	var matches []session.SessionEntry
	for _, row := range rows {
		if row.Type == session.EntryTypeTool && row.ActionState == session.ActionStateExecution &&
			row.ActionID == action.actionID && row.ToolCallID == action.toolCallID {
			matches = append(matches, row)
		}
	}
	require.Len(t, matches, 1, "the exact dispatched Bash action has one execution row")
	return matches[0]
}

func waitForAbsentProcessGroup(t *testing.T, identity core.ExecutionIdentity, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := core.ProcessGroupExitInspection{}
	for time.Now().Before(deadline) {
		last = core.InspectProcessGroupExit(identity)
		if last.Status == core.ProcessGroupExitAbsent {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("original process group did not become verifiably absent within %s: %#v", timeout, last)
}

type durableCLIEscapedDescendant struct {
	nonce         string
	startedPath   string
	effectPath    string
	heartbeatPath string
	releasePath   string
	latePath      string
	exitPath      string
	invocationID  string
}

func newDurableCLIEscapedDescendant(t *testing.T, workDir string) *durableCLIEscapedDescendant {
	t.Helper()
	nonce := fmt.Sprintf("escaped-%d", time.Now().UnixNano())
	prefix := filepath.Join(workDir, ".durable-escaped-"+nonce)
	return &durableCLIEscapedDescendant{
		nonce: nonce, startedPath: prefix + ".started", effectPath: prefix + ".effect",
		heartbeatPath: prefix + ".heartbeat", releasePath: prefix + ".release",
		latePath: prefix + ".late-effect", exitPath: prefix + ".exit-proof",
	}
}

func (child *durableCLIEscapedDescendant) command() string {
	childScript := strings.Join([]string{
		"invocation_id=" + shellQuote(child.nonce) + ":$$",
		"printf '%s\\n' \"$invocation_id\" >> " + shellQuote(child.startedPath) + " && sync || exit 91",
		"printf '%s\\n' \"$invocation_id\" >> " + shellQuote(child.effectPath) + " && sync || exit 92",
		"i=0",
		"while [ \"$(cat " + shellQuote(child.releasePath) + " 2>/dev/null)\" != " + shellQuote(child.nonce) + " ]; do i=$((i + 1)); printf '%s:%s\\n' \"$invocation_id\" \"$i\" > " + shellQuote(child.heartbeatPath) + "; sleep 0.02; done",
		"printf '%s\\n' \"$invocation_id\" >> " + shellQuote(child.latePath) + " && sync || exit 93",
		"printf '%s\\n' \"$invocation_id\" >> " + shellQuote(child.exitPath) + " && sync || exit 94",
		"exit 0",
	}, "; ")
	return "setsid sh -c " + shellQuote(childScript) + " </dev/null >/dev/null 2>&1 & escaped_pid=$!; wait \"$escaped_pid\""
}

func (child *durableCLIEscapedDescendant) waitForStarted(t *testing.T, timeout time.Duration) {
	t.Helper()
	entries := child.waitForLedger(t, child.startedPath, timeout)
	require.Len(t, entries, 1)
	require.True(t, strings.HasPrefix(entries[0], child.nonce+":"))
	child.invocationID = entries[0]
}

func (child *durableCLIEscapedDescendant) childPID(t *testing.T) int {
	t.Helper()
	fields := strings.Split(child.invocationID, ":")
	require.Len(t, fields, 2)
	processID, err := strconv.Atoi(fields[1])
	require.NoError(t, err)
	require.Positive(t, processID)
	return processID
}

func (child *durableCLIEscapedDescendant) waitForEffect(t *testing.T, timeout time.Duration) {
	t.Helper()
	entries := child.waitForLedger(t, child.effectPath, timeout)
	require.Equal(t, []string{child.invocationID}, entries)
}

func (child *durableCLIEscapedDescendant) waitForLateEffect(t *testing.T, timeout time.Duration) {
	t.Helper()
	entries := child.waitForLedger(t, child.latePath, timeout)
	require.Equal(t, []string{child.invocationID}, entries)
}

func (child *durableCLIEscapedDescendant) waitForHeartbeatAfter(t *testing.T, previous int, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if value, ok := child.heartbeat(); ok && value > previous {
			return value
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("detached child heartbeat did not advance beyond %d", previous)
	return 0
}

func (child *durableCLIEscapedDescendant) heartbeat() (int, bool) {
	data, err := os.ReadFile(child.heartbeatPath)
	if err != nil {
		return 0, false
	}
	line := strings.TrimSpace(string(data))
	prefix := child.invocationID + ":"
	if child.invocationID == "" || !strings.HasPrefix(line, prefix) {
		return 0, false
	}
	var value int
	if _, err := fmt.Sscanf(strings.TrimPrefix(line, prefix), "%d", &value); err != nil {
		return 0, false
	}
	return value, true
}

func (child *durableCLIEscapedDescendant) release() error {
	if err := os.WriteFile(child.releasePath, []byte(child.nonce), 0o600); err != nil {
		return fmt.Errorf("release detached fixture child: %w", err)
	}
	return nil
}

func (child *durableCLIEscapedDescendant) waitForExit(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		entries, err := readDurableCLIOrphanLedger(child.exitPath)
		if err == nil && len(entries) == 1 && entries[0] == child.invocationID {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("detached fixture child did not publish its exit proof")
}

func (child *durableCLIEscapedDescendant) waitForLedger(t *testing.T, path string, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		entries, err := readDurableCLIOrphanLedger(path)
		if err == nil && len(entries) > 0 {
			return entries
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("detached fixture child did not publish its ledger to %s", path)
	return nil
}
