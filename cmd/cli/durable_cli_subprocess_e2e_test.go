package main

import (
	"bufio"
	"bytes"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/session"
)

const durableCLIFixtureAPIKey = "durable-cli-fixture-key"

func TestDurableCLIBinaryPersistsWriteReceiptAndDoesNotReplayAfterRestart(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	binary := filepath.Join(t.TempDir(), "go-cli")
	build := exec.Command("go", "build", "-o", binary, "./cmd/cli")
	build.Dir = root
	buildOutput, err := build.CombinedOutput()
	require.NoError(t, err, "build real CLI: %s", buildOutput)
	t.Logf("build command: go build -o %s ./cmd/cli\nbuild exit: 0\nbuild output: %s", binary, buildOutput)

	t.Run("normal process exit and exact physical resume", func(t *testing.T) {
		fixture := newDurableCLIBinaryFixture(t, binary, false)
		defer fixture.server.Close()

		first := fixture.run(t, "prompt", "-provider=openai", "-output=text", "-approve=auto", "create durable marker")
		requireCLIChildExit(t, first, 0)
		physicalID := durableCLIPhysicalSessionIDFromOutput(t, first.stdout)
		require.Contains(t, first.stdout, "provider completed call")
		firstRequests := fixture.assertPeerRequests(t, 2)
		markerPath := filepath.Join(fixture.workDir, "durable-effect.txt")
		marker, err := os.ReadFile(markerPath)
		require.NoError(t, err)
		require.Equal(t, "effect-once", string(marker))

		firstRows := fixture.readRows(t, physicalID)
		firstAction := requireDurableWriteLifecycle(t, firstRows, physicalID)
		requireDurableTranscriptSequence(t, firstRows, []string{"create durable marker"}, true)
		firstTerminals := requireRunTerminals(t, firstRows, physicalID, 1)
		logDurableFileEvidence(t, "first completed child", physicalID, firstRows, firstAction, firstTerminals)
		firstIndex := fixture.readWorkspaceIndex(t, firstAction.workspaceID)
		requireWorkspaceActionLifecycle(t, firstIndex, physicalID, firstAction.actionID, 1, 1)
		require.Contains(t, string(firstRequests[1]), `"role":"tool"`)
		require.Contains(t, string(firstRequests[1]), `"tool_call_id":"call-durable-write-once"`)

		second := fixture.run(t, "prompt", "-provider=openai", "-output=text", "-approve=auto", "-resume-session", physicalID, "confirm durable marker after restart")
		requireCLIChildExit(t, second, 0)
		require.Equal(t, physicalID, durableCLIPhysicalSessionIDFromOutput(t, second.stdout), "resume must reopen the exact physical JSONL session")
		require.Contains(t, second.stdout, "provider completed call")
		marker, err = os.ReadFile(markerPath)
		require.NoError(t, err)
		require.Equal(t, "effect-once", string(marker), "resuming a completed CLI process must not replay the completed write")
		require.Equal(t, 1, strings.Count(fixture.logContents(t), "msg=write.done"), "raw production logs contain one successful managed write across both processes")

		rows := fixture.readRows(t, physicalID)
		action := requireDurableWriteLifecycle(t, rows, physicalID)
		requireDurableTranscriptSequence(t, rows, []string{"create durable marker", "confirm durable marker after restart"}, true)
		require.Equal(t, firstAction.actionID, action.actionID)
		require.Equal(t, firstAction.toolCallID, action.toolCallID)
		require.Equal(t, firstAction.workspaceID, action.workspaceID)
		terminals := requireRunTerminals(t, rows, physicalID, 2)
		logDurableFileEvidence(t, "normal exact-resume pair", physicalID, rows, action, terminals)
		require.NotEqual(t, firstTerminals[0].RunID, terminals[1].RunID, "each process invocation owns one distinct successful run")
		allRequests := fixture.assertPeerRequests(t, 3)
		restartedRequest := string(allRequests[2])
		require.Contains(t, restartedRequest, `"content":"create durable marker"`)
		require.Contains(t, restartedRequest, `"content":"confirm durable marker after restart"`)
		require.Contains(t, restartedRequest, `"tool_call_id":"call-durable-write-once"`, "the physical history contains the original call and receipt")
		requireWorkspaceActionLifecycle(t, fixture.readWorkspaceIndex(t, action.workspaceID), physicalID, action.actionID, 1, 1)
	})

	t.Run("kill after synced receipt before provider completion", func(t *testing.T) {
		fixture := newDurableCLIBinaryFixture(t, binary, true)
		defer func() {
			fixture.releaseBarrier()
			fixture.server.Close()
		}()

		command, stdout, stderr, wait := fixture.start(t, "prompt", "-provider=openai", "-output=text", "-approve=auto", "create durable marker")
		physicalID := waitForDurableCLIOutput(t, stdout, "Session ID: ")
		waitForDurableCLIBarrier(t, fixture.peer.barrierReached)
		fixture.assertPeerRequests(t, 2)
		sessionPath := filepath.Join(fixture.storeDir, "sessions", physicalID+".jsonl")
		rows := waitForDurableReceipt(t, sessionPath, physicalID)
		action := requireDurableWriteLifecycle(t, rows, physicalID)
		requireDurableTranscriptSequence(t, rows, []string{"create durable marker"}, false)
		requireWorkspaceActionLifecycle(t, fixture.readWorkspaceIndex(t, action.workspaceID), physicalID, action.actionID, 1, 1)
		require.Empty(t, requireRunTerminals(t, rows, physicalID, 0), "the provider completion barrier must precede any terminal checkpoint")
		logDurableFileEvidence(t, "killed child after synced receipt", physicalID, rows, action, nil)
		markerPath := filepath.Join(fixture.workDir, "durable-effect.txt")
		marker, err := os.ReadFile(markerPath)
		require.NoError(t, err, "the actual managed write must have happened before the blocked second provider request")
		require.Equal(t, "effect-once", string(marker))

		require.NoError(t, command.Process.Kill())
		firstErr := <-wait
		first := durableCLIChildResult{stdout: stdout.String(), stderr: stderr.String(), err: firstErr, exitCode: command.ProcessState.ExitCode(), state: command.ProcessState.String()}
		requireCLIChildExit(t, first, -1)
		t.Logf("blocked child command: %s prompt -provider=openai -output=text -approve=auto create durable marker\nblocked child exit code: %d state: %s error: %v\nblocked child stdout:\n%s\nblocked child stderr:\n%s\nraw CLI log records:\n%s", fixture.binary, first.exitCode, first.state, first.err, first.stdout, first.stderr, relevantDurableCLILogRecords(fixture.logContents(t)))

		fixture.releaseBarrier()
		second := fixture.run(t, "prompt", "-provider=openai", "-output=text", "-approve=auto", "-resume-session", physicalID, "continue after interrupted process")
		requireCLIChildExit(t, second, 0)
		require.Equal(t, physicalID, durableCLIPhysicalSessionIDFromOutput(t, second.stdout))
		marker, err = os.ReadFile(markerPath)
		require.NoError(t, err)
		require.Equal(t, "effect-once", string(marker), "recovery must not dispatch the receipted write again")
		require.Equal(t, 1, strings.Count(fixture.logContents(t), "msg=write.done"), "raw production logs contain one successful managed write across the kill/restart boundary")

		rows = fixture.readRows(t, physicalID)
		actionAfterRestart := requireDurableWriteLifecycle(t, rows, physicalID)
		requireDurableTranscriptSequence(t, rows, []string{"create durable marker", "continue after interrupted process"}, false)
		require.Equal(t, action.actionID, actionAfterRestart.actionID)
		require.Equal(t, action.toolCallID, actionAfterRestart.toolCallID)
		terminals := requireRunTerminals(t, rows, physicalID, 1)
		logDurableFileEvidence(t, "killed child exact-resume", physicalID, rows, actionAfterRestart, terminals)
		allRequests := fixture.assertPeerRequests(t, 3)
		resumedRequest := string(allRequests[2])
		require.Contains(t, resumedRequest, `"tool_call_id":"call-durable-write-once"`)
		require.Contains(t, resumedRequest, `"content":"continue after interrupted process"`)
		requireWorkspaceActionLifecycle(t, fixture.readWorkspaceIndex(t, action.workspaceID), physicalID, action.actionID, 1, 1)
	})
}

func TestDurableCLIBinaryQuarantinesLiveOrphanedBashAndNeverReplays(t *testing.T) {
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
	orphan := newDurableCLIOrphanControl(t, fixture.workDir)
	t.Cleanup(func() {
		if err := orphan.cleanup(3 * time.Second); err != nil {
			t.Errorf("release and observe the fixture-owned Bash process exit during cleanup: %v", err)
		}
	})
	fixture.peer.setResponsePlan(
		durableCLIToolCall("call-live-orphan-bash", "bash", map[string]string{"command": orphan.command()}),
		durableCLIFinal("exact resume after live orphan"),
		durableCLIToolCall("call-blocked-fresh-write", "write", map[string]string{
			"path":    "fresh-session-effect.txt",
			"content": "must-not-dispatch",
		}),
		durableCLIFinal("recovered denied fresh-session mutation"),
	)

	command, stdout, stderr, wait := fixture.start(t,
		"prompt", "-provider=openai", "-output=text", "-approve=auto", "-no-sandbox", "create durable orphan marker")
	physicalID := waitForDurableCLIOutput(t, stdout, "Session ID: ")
	rows, action := waitForDurableBashDispatch(t, fixture, physicalID, "call-live-orphan-bash")
	requireDurableSourceIDsUnique(t, rows, physicalID)
	indexRows := fixture.readWorkspaceIndex(t, action.workspaceID)
	requireWorkspaceActionLifecycleForCall(t, indexRows, physicalID, action.actionID, action.toolCallID, 1, 0)
	require.Empty(t, receiptsForCall(rows, "call-live-orphan-bash"), "no tool receipt may precede managed Bash completion")
	require.Empty(t, requireRunTerminals(t, rows, physicalID, 0), "the run cannot have a terminal while managed Bash is still running")
	require.NoFileExists(t, orphan.effectPath, "the fixture holds the actual tool child before the external effect")
	waitForOrphanStarted(t, orphan)
	requireOrphanLedgerCounts(t, orphan, 1, 0, "after managed Bash start and before its effect gate opens")
	rows, action = waitForDurableBashDispatch(t, fixture, physicalID, "call-live-orphan-bash")
	requireDurableSourceIDsUnique(t, rows, physicalID)
	indexRows = fixture.readWorkspaceIndex(t, action.workspaceID)
	requireWorkspaceActionLifecycleForCall(t, indexRows, physicalID, action.actionID, action.toolCallID, 1, 0)
	require.NoFileExists(t, orphan.effectPath, "the effect gate keeps the managed command behind the synced durable marker")
	require.NoError(t, orphan.releaseEffectGate())
	waitForOrphanEffect(t, orphan)
	requireOrphanLedgerCounts(t, orphan, 1, 1, "before parent kill")
	heartbeat := waitForOrphanHeartbeat(t, orphan, 0)
	heartbeat = waitForOrphanHeartbeatAdvance(t, orphan, heartbeat)
	requests := fixture.assertPeerRequests(t, 1)
	require.Contains(t, string(requests[0]), `"content":"create durable orphan marker"`)
	t.Logf("pre-crash durable boundary physical_id=%s action_id=%s tool_call_id=%s workspace_id=%s dispatches=1 resolutions=0 receipt=absent terminals=0 effect=%s child_nonce=%s heartbeat=%d", physicalID, action.actionID, action.toolCallID, action.workspaceID, orphan.effectPath, orphan.nonce, heartbeat)

	require.NoError(t, command.Process.Kill(), "SIGKILL only the owned CLI process; leave its managed Bash process group alone")
	firstErr := <-wait
	first := durableCLIChildResult{stdout: stdout.String(), stderr: stderr.String(), err: firstErr, exitCode: command.ProcessState.ExitCode(), state: command.ProcessState.String()}
	requireCLIChildExit(t, first, -1)
	t.Logf("killed actual CLI parent exit=%d state=%s stdout=%q stderr=%q", first.exitCode, first.state, first.stdout, first.stderr)
	rows = fixture.readRows(t, physicalID)
	requireDurableSourceIDsUnique(t, rows, physicalID)
	require.Empty(t, receiptsForCall(rows, "call-live-orphan-bash"), "SIGKILL before tool completion must leave the receipt absent")
	require.Empty(t, requireRunTerminals(t, rows, physicalID, 0), "SIGKILL must leave the original run without a terminal checkpoint")
	indexRows = fixture.readWorkspaceIndex(t, action.workspaceID)
	requireWorkspaceActionLifecycleForCall(t, indexRows, physicalID, action.actionID, action.toolCallID, 1, 0)
	heartbeat = waitForOrphanHeartbeatAdvance(t, orphan, heartbeat)
	requireOrphanLedgerCounts(t, orphan, 1, 1, "after parent kill")
	t.Logf("post-parent-kill orphan still alive physical_id=%s action_id=%s child_nonce=%s heartbeat=%d receipt=absent terminals=0 dispatches=1 resolutions=0", physicalID, action.actionID, orphan.nonce, heartbeat)

	resumed := fixture.run(t,
		"prompt", "-provider=openai", "-output=text", "-approve=auto", "-no-sandbox", "-resume-session", physicalID, "resume exact orphaned session")
	requireCLIChildExit(t, resumed, 0)
	require.Equal(t, physicalID, durableCLIPhysicalSessionIDFromOutput(t, resumed.stdout), "restart must select the same physical session")
	require.Contains(t, resumed.stdout, "exact resume after live orphan")
	requireOrphanLedgerCounts(t, orphan, 1, 1, "after exact resume")
	rows = fixture.readRows(t, physicalID)
	requireDurableSourceIDsUnique(t, rows, physicalID)
	recoveredReceipt := requireRecoveredUnknownBashReceipt(t, rows, physicalID, action)
	terminals := requireRunTerminals(t, rows, physicalID, 1)
	logDurableFileEvidence(t, "live-orphan exact resume", physicalID, rows, action, terminals)
	requests = fixture.assertPeerRequests(t, 2)
	resumedRequest := string(requests[1])
	require.Contains(t, resumedRequest, `"content":"create durable orphan marker"`)
	require.Contains(t, resumedRequest, `"tool_call_id":"call-live-orphan-bash"`)
	require.Contains(t, resumedRequest, recoveredReceipt.Content, "the recovered exact-session request must expose the durable unknown receipt")
	require.Equal(t, action.actionID, recoveredReceipt.ActionID)
	indexRows = fixture.readWorkspaceIndex(t, action.workspaceID)
	requireWorkspaceActionLifecycleForCall(t, indexRows, physicalID, action.actionID, action.toolCallID, 1, 0)
	heartbeat = waitForOrphanHeartbeatAdvance(t, orphan, heartbeat)

	blocked := fixture.run(t,
		"prompt", "-provider=openai", "-output=text", "-approve=auto", "-no-sandbox", "attempt conflicting mutation from a new session")
	requireCLIChildFailure(t, blocked, 1)
	freshPhysicalID := durableCLIPhysicalSessionIDFromOutput(t, blocked.stdout)
	require.NotEqual(t, physicalID, freshPhysicalID, "a new CLI prompt must have a distinct physical session")
	require.Contains(t, blocked.stderr, "workspace is quarantined by unresolved actions")
	require.Contains(t, blocked.stderr, "unreceipted tool calls")
	require.NoFileExists(t, filepath.Join(fixture.workDir, "fresh-session-effect.txt"), "the second physical session cannot dispatch a conflicting mutation while the orphan remains unresolved")
	requireOrphanLedgerCounts(t, orphan, 1, 1, "after distinct-session mutation rejection; that session has zero effects")
	blockedRows := fixture.readRows(t, freshPhysicalID)
	requireDurableSourceIDsUnique(t, blockedRows, freshPhysicalID)
	blockedAction := requirePendingWriteAttempt(t, blockedRows, freshPhysicalID, "call-blocked-fresh-write")
	require.NotEqual(t, action.actionID, blockedAction.actionID, "the fresh session must have its own action identity")
	require.Equal(t, action.workspaceID, blockedAction.workspaceID, "both physical sessions must share the exact captured workspace authority")
	require.Empty(t, receiptsForCall(blockedRows, "call-blocked-fresh-write"), "the failed pre-dispatch commit must not be fabricated as a durable receipt")
	require.Empty(t, requireRunTerminals(t, blockedRows, freshPhysicalID, 0), "the failing fresh-session run has no durable terminal")
	requests = fixture.assertPeerRequests(t, 3)
	require.Contains(t, string(requests[2]), `"content":"attempt conflicting mutation from a new session"`)
	requireOrphanLedgerCounts(t, orphan, 1, 1, "after distinct-session mutation rejection")
	indexRows = fixture.readWorkspaceIndex(t, action.workspaceID)
	requireWorkspaceActionLifecycleForCall(t, indexRows, physicalID, action.actionID, action.toolCallID, 1, 0)
	requireWorkspaceActionLifecycleForCall(t, indexRows, freshPhysicalID, blockedAction.actionID, blockedAction.toolCallID, 0, 0)
	heartbeat = waitForOrphanHeartbeatAdvance(t, orphan, heartbeat)
	t.Logf("fresh-session conflict denied physical_id=%s intent_action_id=%s receipt=absent terminal=absent old_action_id=%s old_dispatches=1 old_resolutions=0 heartbeat=%d provider_requests=%d exit=%d", freshPhysicalID, blockedAction.actionID, action.actionID, heartbeat, len(requests), blocked.exitCode)

	blockedResume := fixture.run(t,
		"prompt", "-provider=openai", "-output=text", "-approve=auto", "-no-sandbox", "-resume-session", freshPhysicalID, "resume after denied mutation")
	requireCLIChildExit(t, blockedResume, 0)
	require.Equal(t, freshPhysicalID, durableCLIPhysicalSessionIDFromOutput(t, blockedResume.stdout), "the failed fresh session must resume by its exact physical ID")
	require.Contains(t, blockedResume.stdout, "recovered denied fresh-session mutation")
	blockedRows = fixture.readRows(t, freshPhysicalID)
	requireDurableSourceIDsUnique(t, blockedRows, freshPhysicalID)
	interruptedReceipt := requireRecoveredInterruptedWriteReceipt(t, blockedRows, freshPhysicalID, blockedAction)
	blockedTerminals := requireRunTerminals(t, blockedRows, freshPhysicalID, 1)
	logDurableFileEvidence(t, "fresh-session exact resume after denial", freshPhysicalID, blockedRows, blockedAction, blockedTerminals)
	requests = fixture.assertPeerRequests(t, 4)
	require.Contains(t, string(requests[3]), `"tool_call_id":"call-blocked-fresh-write"`)
	require.Contains(t, string(requests[3]), interruptedReceipt.Content, "the resumed request uses real marker-absent recovery evidence")
	indexRows = fixture.readWorkspaceIndex(t, action.workspaceID)
	requireWorkspaceActionLifecycleForCall(t, indexRows, physicalID, action.actionID, action.toolCallID, 1, 0)
	requireWorkspaceActionLifecycleForCall(t, indexRows, freshPhysicalID, blockedAction.actionID, blockedAction.toolCallID, 0, 0)
	require.NoFileExists(t, filepath.Join(fixture.workDir, "fresh-session-effect.txt"), "exact recovery of the denied distinct-session mutation must not create an effect")
	requireOrphanLedgerCounts(t, orphan, 1, 1, "after denied distinct-session exact resume")

	require.NoError(t, orphan.releaseAndWait(10*time.Second), "the nonce-bound release file must let the actual orphaned Bash process exit")
	requireOrphanExitProofs(t, orphan)
	requireOrphanLedgerCounts(t, orphan, 1, 1, "after orphan exit")
	rows = fixture.readRows(t, physicalID)
	recoveredAgain := requireRecoveredUnknownBashReceipt(t, rows, physicalID, action)
	require.Equal(t, recoveredReceipt.ID, recoveredAgain.ID, "fixture child exit does not replace the durable unknown receipt")
	require.Equal(t, "unknown", recoveredAgain.ReceiptOutcome)
	indexRows = fixture.readWorkspaceIndex(t, action.workspaceID)
	requireWorkspaceActionLifecycleForCall(t, indexRows, physicalID, action.actionID, action.toolCallID, 1, 0)
	require.Equal(t, "interrupted", interruptedReceipt.ReceiptOutcome)
	require.Equal(t, "not_dispatched", string(interruptedReceipt.DispatchState))
	t.Logf("orphan released physical_id=%s action_id=%s exit_proof=%s receipt_id=%s receipt_outcome=%s dispatches=1 resolutions=0 fresh_write_receipt_id=%s fresh_write_outcome=%s fresh_write_dispatch_state=%s", physicalID, action.actionID, orphan.exitPath, recoveredAgain.ID, recoveredAgain.ReceiptOutcome, interruptedReceipt.ID, interruptedReceipt.ReceiptOutcome, interruptedReceipt.DispatchState)
}

func TestDurableCLIBinaryAllowsScopedReadersDuringLiveOrphanAndRetainsMutationQuarantine(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	binary := filepath.Join(t.TempDir(), "go-cli")
	build := exec.Command("go", "build", "-o", binary, "./cmd/cli")
	build.Dir = root
	buildOutput, err := build.CombinedOutput()
	require.NoError(t, err, "build real CLI: %s", buildOutput)
	t.Logf("build command: go build -o %s ./cmd/cli\nbuild exit: 0\nbuild output: %s", binary, buildOutput)

	fixture := newDurableCLIBinaryFixtureWithTools(t, binary, false, []string{"bash", "read", "ls", "write"})
	t.Cleanup(func() { fixture.server.Close() })
	const readMarker = "safe-read-existing-content"
	readPath := filepath.Join(fixture.workDir, "safe-read-existing.txt")
	require.NoError(t, os.WriteFile(readPath, []byte(readMarker), 0o600))
	orphan := newDurableCLIOrphanControl(t, fixture.workDir)
	t.Cleanup(func() {
		if err := orphan.cleanup(3 * time.Second); err != nil {
			t.Errorf("release and observe the fixture-owned Bash process exit during cleanup: %v", err)
		}
	})
	fixture.peer.setResponsePlan(
		durableCLIToolCall("call-live-orphan-bash", "bash", map[string]string{"command": orphan.command()}),
		durableCLIToolCall("call-quarantined-read", "read", map[string]string{"path": "safe-read-existing.txt"}),
		durableCLIFinal("scoped read completed during quarantine"),
		durableCLIToolCall("call-quarantined-ls", "ls", map[string]string{"path": "."}),
		durableCLIFinal("scoped listing completed during quarantine"),
		durableCLIToolCall("call-blocked-quarantined-write", "write", map[string]string{
			"path":    "fresh-session-effect.txt",
			"content": "must-not-dispatch",
		}),
		durableCLIFinal("exact old session resumed after safe reads"),
	)

	command, stdout, stderr, wait := fixture.start(t,
		"prompt", "-provider=openai", "-output=text", "-approve=auto", "-no-sandbox", "create unresolved mutation marker")
	orphanPhysicalID := waitForDurableCLIOutput(t, stdout, "Session ID: ")
	orphanRows, orphanAction := waitForDurableBashDispatch(t, fixture, orphanPhysicalID, "call-live-orphan-bash")
	requireDurableSourceIDsUnique(t, orphanRows, orphanPhysicalID)
	indexRows := fixture.readWorkspaceIndex(t, orphanAction.workspaceID)
	requireWorkspaceActionLifecycleForCall(t, indexRows, orphanPhysicalID, orphanAction.actionID, orphanAction.toolCallID, 1, 0)
	requireWorkspaceActionTotals(t, indexRows, 1, 0)
	require.Empty(t, receiptsForCall(orphanRows, orphanAction.toolCallID), "the original Bash must have no receipt before the CLI parent is killed")
	require.Empty(t, requireRunTerminals(t, orphanRows, orphanPhysicalID, 0), "the original run has no terminal while Bash is live")
	waitForOrphanStarted(t, orphan)
	requireOrphanLedgerCounts(t, orphan, 1, 0, "safe-read matrix before opening the Bash effect gate")
	require.NoError(t, orphan.releaseEffectGate())
	waitForOrphanEffect(t, orphan)
	requireOrphanLedgerCounts(t, orphan, 1, 1, "safe-read matrix before parent kill")
	heartbeat := waitForOrphanHeartbeat(t, orphan, 0)
	heartbeat = waitForOrphanHeartbeatAdvance(t, orphan, heartbeat)
	require.Contains(t, string(fixture.assertPeerRequests(t, 1)[0]), `"content":"create unresolved mutation marker"`)
	t.Logf("pre-crash safe-read matrix boundary physical_id=%s action_id=%s tool_call_id=%s workspace_id=%s dispatches=1 resolutions=0 receipt=absent terminals=0 effect_count=1 child_nonce=%s heartbeat=%d", orphanPhysicalID, orphanAction.actionID, orphanAction.toolCallID, orphanAction.workspaceID, orphan.nonce, heartbeat)

	require.NoError(t, command.Process.Kill(), "SIGKILL only the owned CLI process; leave its managed Bash process group alone")
	firstErr := <-wait
	first := durableCLIChildResult{stdout: stdout.String(), stderr: stderr.String(), err: firstErr, exitCode: command.ProcessState.ExitCode(), state: command.ProcessState.String()}
	requireCLIChildExit(t, first, -1)
	orphanRows = fixture.readRows(t, orphanPhysicalID)
	requireDurableSourceIDsUnique(t, orphanRows, orphanPhysicalID)
	require.Empty(t, receiptsForCall(orphanRows, orphanAction.toolCallID), "parent SIGKILL before Bash completion must leave the receipt absent")
	require.Empty(t, requireRunTerminals(t, orphanRows, orphanPhysicalID, 0), "parent SIGKILL leaves the original run without a terminal")
	indexRows = fixture.readWorkspaceIndex(t, orphanAction.workspaceID)
	requireWorkspaceActionLifecycleForCall(t, indexRows, orphanPhysicalID, orphanAction.actionID, orphanAction.toolCallID, 1, 0)
	requireWorkspaceActionTotals(t, indexRows, 1, 0)
	heartbeat = waitForOrphanHeartbeatAdvance(t, orphan, heartbeat)
	requireOrphanLedgerCounts(t, orphan, 1, 1, "after parent SIGKILL")
	t.Logf("post-parent-kill safe-read matrix boundary physical_id=%s action_id=%s heartbeat=%d receipt=absent terminals=0 dispatches=1 resolutions=0", orphanPhysicalID, orphanAction.actionID, heartbeat)

	readResult := fixture.run(t,
		"prompt", "-provider=openai", "-output=text", "-approve=auto", "-no-sandbox", "read existing workspace file during quarantine")
	requireCLIChildExit(t, readResult, 0)
	readPhysicalID := durableCLIPhysicalSessionIDFromOutput(t, readResult.stdout)
	require.NotEqual(t, orphanPhysicalID, readPhysicalID, "the safe Read executes from a distinct physical session")
	readRows := fixture.readRows(t, readPhysicalID)
	requireDurableSourceIDsUnique(t, readRows, readPhysicalID)
	readAction := requireSuccessfulScopedReader(t, readRows, readPhysicalID, "call-quarantined-read", "read")
	require.NotEqual(t, orphanAction.actionID, readAction.actionID)
	require.Equal(t, orphanAction.workspaceID, readAction.workspaceID, "the real Read must use the same captured workspace authority as the orphan")
	readTerminals := requireRunTerminals(t, readRows, readPhysicalID, 1)
	logDurableFileEvidence(t, "safe Read while old mutation is unresolved", readPhysicalID, readRows, readAction, readTerminals)
	requests := fixture.assertPeerRequests(t, 3)
	require.Contains(t, string(requests[1]), `"content":"read existing workspace file during quarantine"`)
	require.Contains(t, string(requests[2]), readMarker, "the real Read result is returned to the provider")
	indexRows = fixture.readWorkspaceIndex(t, orphanAction.workspaceID)
	requireWorkspaceActionLifecycleForCall(t, indexRows, orphanPhysicalID, orphanAction.actionID, orphanAction.toolCallID, 1, 0)
	requireWorkspaceActionLifecycleForCall(t, indexRows, readPhysicalID, readAction.actionID, readAction.toolCallID, 1, 1)
	requireWorkspaceActionTotals(t, indexRows, 2, 1)
	requireOrphanLedgerCounts(t, orphan, 1, 1, "after a separate physical session completes scoped Read")
	heartbeat = waitForOrphanHeartbeatAdvance(t, orphan, heartbeat)
	fileContents, err := os.ReadFile(readPath)
	require.NoError(t, err)
	require.Equal(t, readMarker, string(fileContents), "the existing workspace file remains unchanged by Read")

	lsResult := fixture.run(t,
		"prompt", "-provider=openai", "-output=text", "-approve=auto", "-no-sandbox", "list workspace during quarantine")
	requireCLIChildExit(t, lsResult, 0)
	lsPhysicalID := durableCLIPhysicalSessionIDFromOutput(t, lsResult.stdout)
	require.NotEqual(t, orphanPhysicalID, lsPhysicalID, "the safe LS executes from a fresh physical session")
	require.NotEqual(t, readPhysicalID, lsPhysicalID, "Read and LS use separate physical sessions")
	lsRows := fixture.readRows(t, lsPhysicalID)
	requireDurableSourceIDsUnique(t, lsRows, lsPhysicalID)
	lsAction := requireSuccessfulScopedReader(t, lsRows, lsPhysicalID, "call-quarantined-ls", "ls")
	require.NotEqual(t, orphanAction.actionID, lsAction.actionID)
	require.Equal(t, orphanAction.workspaceID, lsAction.workspaceID, "the real LS must use the same captured workspace authority as the orphan")
	lsTerminals := requireRunTerminals(t, lsRows, lsPhysicalID, 1)
	logDurableFileEvidence(t, "safe LS while old mutation is unresolved", lsPhysicalID, lsRows, lsAction, lsTerminals)
	requests = fixture.assertPeerRequests(t, 5)
	require.Contains(t, string(requests[3]), `"content":"list workspace during quarantine"`)
	require.Contains(t, string(requests[4]), "safe-read-existing.txt", "the real LS result includes the existing workspace file")
	indexRows = fixture.readWorkspaceIndex(t, orphanAction.workspaceID)
	requireWorkspaceActionLifecycleForCall(t, indexRows, orphanPhysicalID, orphanAction.actionID, orphanAction.toolCallID, 1, 0)
	requireWorkspaceActionLifecycleForCall(t, indexRows, readPhysicalID, readAction.actionID, readAction.toolCallID, 1, 1)
	requireWorkspaceActionLifecycleForCall(t, indexRows, lsPhysicalID, lsAction.actionID, lsAction.toolCallID, 1, 1)
	requireWorkspaceActionTotals(t, indexRows, 3, 2)
	requireOrphanLedgerCounts(t, orphan, 1, 1, "after a separate physical session completes scoped LS")
	heartbeat = waitForOrphanHeartbeatAdvance(t, orphan, heartbeat)

	blocked := fixture.run(t,
		"prompt", "-provider=openai", "-output=text", "-approve=auto", "-no-sandbox", "attempt write during unresolved mutation quarantine")
	requireCLIChildFailure(t, blocked, 1)
	blockedPhysicalID := durableCLIPhysicalSessionIDFromOutput(t, blocked.stdout)
	require.NotEqual(t, orphanPhysicalID, blockedPhysicalID, "the blocked Write also uses a distinct physical session")
	require.NotEqual(t, readPhysicalID, blockedPhysicalID)
	require.NotEqual(t, lsPhysicalID, blockedPhysicalID)
	require.Contains(t, blocked.stderr, "workspace is quarantined by unresolved actions")
	require.Contains(t, blocked.stderr, "unreceipted tool calls")
	require.NoFileExists(t, filepath.Join(fixture.workDir, "fresh-session-effect.txt"), "the fresh physical session's Write has no visible effect")
	blockedRows := fixture.readRows(t, blockedPhysicalID)
	requireDurableSourceIDsUnique(t, blockedRows, blockedPhysicalID)
	blockedAction := requirePendingWriteAttempt(t, blockedRows, blockedPhysicalID, "call-blocked-quarantined-write")
	require.Equal(t, orphanAction.workspaceID, blockedAction.workspaceID)
	require.Empty(t, receiptsForCall(blockedRows, blockedAction.toolCallID), "a rejected pre-dispatch Write does not receive a fabricated receipt")
	require.Empty(t, requireRunTerminals(t, blockedRows, blockedPhysicalID, 0), "the baseline CLI leaves this rejected tool-call group uncommitted")
	requests = fixture.assertPeerRequests(t, 6)
	require.Contains(t, string(requests[5]), `"content":"attempt write during unresolved mutation quarantine"`)
	indexRows = fixture.readWorkspaceIndex(t, orphanAction.workspaceID)
	requireWorkspaceActionLifecycleForCall(t, indexRows, orphanPhysicalID, orphanAction.actionID, orphanAction.toolCallID, 1, 0)
	requireWorkspaceActionLifecycleForCall(t, indexRows, readPhysicalID, readAction.actionID, readAction.toolCallID, 1, 1)
	requireWorkspaceActionLifecycleForCall(t, indexRows, lsPhysicalID, lsAction.actionID, lsAction.toolCallID, 1, 1)
	requireWorkspaceActionLifecycleForCall(t, indexRows, blockedPhysicalID, blockedAction.actionID, blockedAction.toolCallID, 0, 0)
	requireWorkspaceActionTotals(t, indexRows, 3, 2)
	requireOrphanLedgerCounts(t, orphan, 1, 1, "after the distinct-session Write is blocked with zero dispatch and effects")
	heartbeat = waitForOrphanHeartbeatAdvance(t, orphan, heartbeat)
	fileContents, err = os.ReadFile(readPath)
	require.NoError(t, err)
	require.Equal(t, readMarker, string(fileContents), "the existing file stays unchanged after the rejected Write")

	resumed := fixture.run(t,
		"prompt", "-provider=openai", "-output=text", "-approve=auto", "-no-sandbox", "-resume-session", orphanPhysicalID, "resume original session after safe reads")
	requireCLIChildExit(t, resumed, 0)
	require.Equal(t, orphanPhysicalID, durableCLIPhysicalSessionIDFromOutput(t, resumed.stdout), "only the exact old physical session is resumed")
	require.Contains(t, resumed.stdout, "exact old session resumed after safe reads")
	orphanRows = fixture.readRows(t, orphanPhysicalID)
	requireDurableSourceIDsUnique(t, orphanRows, orphanPhysicalID)
	recoveredReceipt := requireRecoveredUnknownBashReceipt(t, orphanRows, orphanPhysicalID, orphanAction)
	orphanTerminals := requireRunTerminals(t, orphanRows, orphanPhysicalID, 1)
	logDurableFileEvidence(t, "exact old-session resume after scoped reads", orphanPhysicalID, orphanRows, orphanAction, orphanTerminals)
	requests = fixture.assertPeerRequests(t, 7)
	resumedRequest := string(requests[6])
	require.Contains(t, resumedRequest, `"content":"resume original session after safe reads"`)
	require.Contains(t, resumedRequest, `"tool_call_id":"call-live-orphan-bash"`)
	require.Contains(t, resumedRequest, recoveredReceipt.Content, "the resumed exact session carries its real unknown receipt")
	indexRows = fixture.readWorkspaceIndex(t, orphanAction.workspaceID)
	requireWorkspaceActionLifecycleForCall(t, indexRows, orphanPhysicalID, orphanAction.actionID, orphanAction.toolCallID, 1, 0)
	requireWorkspaceActionLifecycleForCall(t, indexRows, readPhysicalID, readAction.actionID, readAction.toolCallID, 1, 1)
	requireWorkspaceActionLifecycleForCall(t, indexRows, lsPhysicalID, lsAction.actionID, lsAction.toolCallID, 1, 1)
	requireWorkspaceActionLifecycleForCall(t, indexRows, blockedPhysicalID, blockedAction.actionID, blockedAction.toolCallID, 0, 0)
	requireWorkspaceActionTotals(t, indexRows, 3, 2)
	requireOrphanLedgerCounts(t, orphan, 1, 1, "after exact old-session resume; the Bash mutation is not replayed")
	heartbeat = waitForOrphanHeartbeatAdvance(t, orphan, heartbeat)

	require.NoError(t, orphan.releaseAndWait(10*time.Second), "release the actual orphaned Bash child after the complete matrix")
	requireOrphanExitProofs(t, orphan)
	requireOrphanLedgerCounts(t, orphan, 1, 1, "after the orphaned Bash child exits")
	orphanRows = fixture.readRows(t, orphanPhysicalID)
	recoveredAgain := requireRecoveredUnknownBashReceipt(t, orphanRows, orphanPhysicalID, orphanAction)
	require.Equal(t, recoveredReceipt.ID, recoveredAgain.ID, "child exit does not replace the recovered unknown receipt")
	indexRows = fixture.readWorkspaceIndex(t, orphanAction.workspaceID)
	requireWorkspaceActionLifecycleForCall(t, indexRows, orphanPhysicalID, orphanAction.actionID, orphanAction.toolCallID, 1, 0)
	requireWorkspaceActionLifecycleForCall(t, indexRows, readPhysicalID, readAction.actionID, readAction.toolCallID, 1, 1)
	requireWorkspaceActionLifecycleForCall(t, indexRows, lsPhysicalID, lsAction.actionID, lsAction.toolCallID, 1, 1)
	requireWorkspaceActionTotals(t, indexRows, 3, 2)
	t.Logf("safe-read crash matrix complete old_action=%s old_receipt=%s read_action=%s ls_action=%s blocked_write_action=%s dispatches=3 resolutions=2 old_quarantine=unresolved orphan_exit=%s", orphanAction.actionID, recoveredAgain.ID, readAction.actionID, lsAction.actionID, blockedAction.actionID, orphan.exitPath)
}

type durableCLIOrphanControl struct {
	nonce          string
	invocationID   string
	startedPath    string
	effectGatePath string
	effectPath     string
	heartbeatPath  string
	releasePath    string
	exitPath       string
}

func newDurableCLIOrphanControl(t *testing.T, workDir string) *durableCLIOrphanControl {
	t.Helper()
	var nonceBytes [16]byte
	_, err := cryptorand.Read(nonceBytes[:])
	require.NoError(t, err)
	nonce := hex.EncodeToString(nonceBytes[:])
	prefix := filepath.Join(workDir, ".durable-orphan-"+nonce)
	return &durableCLIOrphanControl{
		nonce:          nonce,
		startedPath:    prefix + ".started",
		effectGatePath: prefix + ".effect-gate",
		effectPath:     filepath.Join(workDir, "durable-orphan-effect.jsonl"),
		heartbeatPath:  prefix + ".heartbeat",
		releasePath:    prefix + ".release",
		exitPath:       prefix + ".exit-proof",
	}
}

func (control *durableCLIOrphanControl) command() string {
	return strings.Join([]string{
		"invocation_id=" + shellQuote(control.nonce) + ":$$:$RANDOM",
		"printf '%s\\n' \"$invocation_id\" >> " + shellQuote(control.startedPath) + " && sync || exit 91",
		"while [ \"$(cat " + shellQuote(control.effectGatePath) + " 2>/dev/null)\" != " + shellQuote(control.nonce) + " ]; do sleep 0.02; done",
		"printf '%s\\n' \"$invocation_id\" >> " + shellQuote(control.effectPath) + " && sync || exit 92",
		"i=0",
		"while [ \"$(cat " + shellQuote(control.releasePath) + " 2>/dev/null)\" != " + shellQuote(control.nonce) + " ]; do i=$((i + 1)); printf '%s:%s\\n' \"$invocation_id\" \"$i\" > " + shellQuote(control.heartbeatPath) + "; sleep 0.02; done",
		"printf '%s\\n' \"$invocation_id\" >> " + shellQuote(control.exitPath) + " && sync || exit 93",
		"exit 0",
	}, "; ")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func (control *durableCLIOrphanControl) started() bool {
	entries, err := readDurableCLIOrphanLedger(control.startedPath)
	if err != nil || len(entries) == 0 {
		return false
	}
	if control.invocationID == "" {
		control.invocationID = entries[0]
	}
	return true
}

func (control *durableCLIOrphanControl) releaseEffectGate() error {
	return os.WriteFile(control.effectGatePath, []byte(control.nonce), 0o600)
}

func (control *durableCLIOrphanControl) releaseAndWait(timeout time.Duration) error {
	if err := control.releaseEffectGate(); err != nil {
		return fmt.Errorf("write nonce-bound effect gate: %w", err)
	}
	if err := os.WriteFile(control.releasePath, []byte(control.nonce), 0o600); err != nil {
		return fmt.Errorf("write nonce-bound release file: %w", err)
	}
	if err := waitForOrphanExitProofs(control, timeout); err != nil {
		return fmt.Errorf("wait for fixture-specific Bash exit proofs: %w", err)
	}
	if err := waitForOrphanHeartbeatStable(control, 150*time.Millisecond); err != nil {
		return err
	}
	return nil
}

func (control *durableCLIOrphanControl) cleanup(timeout time.Duration) error {
	if err := control.releaseEffectGate(); err != nil {
		return fmt.Errorf("write cleanup effect gate: %w", err)
	}
	if err := os.WriteFile(control.releasePath, []byte(control.nonce), 0o600); err != nil {
		return fmt.Errorf("write cleanup release file: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if control.started() || control.hasExitProof() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !control.started() && !control.hasExitProof() {
		return nil
	}
	if err := waitForOrphanExitProofs(control, timeout); err != nil {
		return fmt.Errorf("wait for fixture-specific Bash exit proofs during cleanup: %w", err)
	}
	if _, ok := readOrphanHeartbeat(control); ok {
		return waitForOrphanHeartbeatStable(control, 150*time.Millisecond)
	}
	return nil
}

func (control *durableCLIOrphanControl) hasExitProof() bool {
	entries, err := readDurableCLIOrphanLedger(control.exitPath)
	return err == nil && len(entries) > 0
}

func readDurableCLIOrphanLedger(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var entries []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			entries = append(entries, line)
		}
	}
	return entries, nil
}

func requireOrphanLedgerCounts(t *testing.T, control *durableCLIOrphanControl, wantStarted, wantEffects int, phase string) {
	t.Helper()
	started, err := readDurableCLIOrphanLedger(control.startedPath)
	require.NoError(t, err, "%s: read append-only Bash start ledger", phase)
	effects, err := readDurableCLIOrphanLedger(control.effectPath)
	require.NoError(t, err, "%s: read append-only external-effect ledger", phase)
	require.Len(t, started, wantStarted, "%s: actual managed Bash invocation count", phase)
	require.Len(t, effects, wantEffects, "%s: visible external effect count", phase)
	for _, invocationID := range started {
		require.True(t, strings.HasPrefix(invocationID, control.nonce+":"), "%s: start record carries this fixture nonce", phase)
	}
	for _, invocationID := range effects {
		require.Contains(t, started, invocationID, "%s: effect record belongs to a recorded managed Bash invocation", phase)
	}
	if len(started) > 0 {
		require.Equal(t, started[0], control.invocationID, "%s: fixture identity remains bound to the original managed Bash invocation", phase)
	}
}

func requireOrphanExitProofs(t *testing.T, control *durableCLIOrphanControl) {
	t.Helper()
	started, err := readDurableCLIOrphanLedger(control.startedPath)
	require.NoError(t, err)
	exits, err := readDurableCLIOrphanLedger(control.exitPath)
	require.NoError(t, err)
	require.True(t, sameLedgerRecords(started, exits), "every fixture-owned Bash invocation must publish its own exit proof")
}

func waitForOrphanExitProofs(control *durableCLIOrphanControl, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		started, startedErr := readDurableCLIOrphanLedger(control.startedPath)
		exits, exitErr := readDurableCLIOrphanLedger(control.exitPath)
		if startedErr == nil && exitErr == nil && len(started) > 0 && sameLedgerRecords(started, exits) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("not every managed Bash invocation has an exact exit proof within %s", timeout)
}

func sameLedgerRecords(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for _, entry := range left {
		found := false
		for _, other := range right {
			if entry == other {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func waitForOrphanStarted(t *testing.T, control *durableCLIOrphanControl) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := readDurableCLIOrphanLedger(control.startedPath)
		if err == nil && len(entries) > 0 {
			require.Len(t, entries, 1, "only the original managed Bash invocation may start before reopening its session")
			require.True(t, strings.HasPrefix(entries[0], control.nonce+":"), "the invocation record must carry this fixture nonce")
			control.invocationID = entries[0]
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the actual managed Bash command did not append its nonce-bound start record to %s", control.startedPath)
}

func waitForOrphanEffect(t *testing.T, control *durableCLIOrphanControl) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := readDurableCLIOrphanLedger(control.effectPath)
		if err == nil && len(entries) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the actual managed Bash command did not append its nonce-bound effect record to %s", control.effectPath)
}

func waitForOrphanHeartbeat(t *testing.T, control *durableCLIOrphanControl, after int) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if value, ok := readOrphanHeartbeat(control); ok && value > after {
			return value
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fixture-owned Bash heartbeat did not advance past %d", after)
	return 0
}

func waitForOrphanHeartbeatAdvance(t *testing.T, control *durableCLIOrphanControl, previous int) int {
	t.Helper()
	return waitForOrphanHeartbeat(t, control, previous)
}

func readOrphanHeartbeat(control *durableCLIOrphanControl) (int, bool) {
	data, err := os.ReadFile(control.heartbeatPath)
	if err != nil {
		return 0, false
	}
	line := strings.TrimSpace(string(data))
	prefix := control.invocationID + ":"
	if control.invocationID == "" || !strings.HasPrefix(line, prefix) {
		return 0, false
	}
	value, err := strconv.Atoi(strings.TrimPrefix(line, prefix))
	return value, err == nil
}

func waitForOrphanHeartbeatStable(control *durableCLIOrphanControl, duration time.Duration) error {
	value, ok := readOrphanHeartbeat(control)
	if !ok {
		return fmt.Errorf("fixture heartbeat %s is missing or has the wrong nonce", control.heartbeatPath)
	}
	time.Sleep(duration)
	latest, ok := readOrphanHeartbeat(control)
	if !ok || latest != value {
		return fmt.Errorf("fixture heartbeat advanced after its exit proof: before=%d after=%d valid=%t", value, latest, ok)
	}
	return nil
}

type durableCLIBinaryFixture struct {
	binary      string
	home        string
	workDir     string
	storeDir    string
	configDir   string
	cacheDir    string
	server      *httptest.Server
	peer        *durableCLIPeer
	childEnv    []string
	releaseOnce sync.Once
}

func newDurableCLIBinaryFixture(t *testing.T, binary string, blockAfterToolReceipt bool) *durableCLIBinaryFixture {
	return newDurableCLIBinaryFixtureWithTools(t, binary, blockAfterToolReceipt, []string{"write"})
}

func newDurableCLIBinaryFixtureWithTools(t *testing.T, binary string, blockAfterToolReceipt bool, builtins []string) *durableCLIBinaryFixture {
	t.Helper()
	peer := newDurableCLIPeer(blockAfterToolReceipt)
	server := httptest.NewServer(peer)
	fixture := &durableCLIBinaryFixture{
		binary: binary, server: server, peer: peer,
		home:      filepath.Join(t.TempDir(), "home"),
		workDir:   filepath.Join(t.TempDir(), "work"),
		storeDir:  filepath.Join(t.TempDir(), "durable-store"),
		configDir: filepath.Join(t.TempDir(), "config"),
		cacheDir:  filepath.Join(t.TempDir(), "cache"),
	}
	for _, directory := range []string{fixture.home, fixture.workDir, fixture.storeDir, fixture.configDir, fixture.cacheDir} {
		require.NoError(t, os.MkdirAll(directory, 0o700))
	}
	tmpDir := filepath.Join(t.TempDir(), "tmp")
	require.NoError(t, os.MkdirAll(tmpDir, 0o700))
	configPath := filepath.Join(fixture.home, ".go-cli", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o700))
	builtinYAML := strings.Join(builtins, "\n    - ")
	configYAML := fmt.Sprintf("provider:\n  name: openai\n  base_url: %q\n  model: durable-cli-fixture\nmodel:\n  name: durable-cli-fixture\nsession:\n  store_path: %q\ntools:\n  builtin:\n    - %s\ntracing:\n  enabled: false\n", server.URL+"/v1", fixture.storeDir, builtinYAML)
	require.NoError(t, os.WriteFile(configPath, []byte(configYAML), 0o600))
	fixture.childEnv = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + fixture.home,
		"XDG_CONFIG_HOME=" + fixture.configDir,
		"XDG_CACHE_HOME=" + fixture.cacheDir,
		"TMPDIR=" + tmpDir,
		"GO_CLI_PROVIDER_NAME=openai",
		"GO_CLI_PROVIDER_API_KEY=" + durableCLIFixtureAPIKey,
		"GO_CLI_PROVIDER_BASE_URL=" + server.URL + "/v1",
		"GO_CLI_PROVIDER_MODEL=durable-cli-fixture",
		"GO_CLI_NO_ONBOARDING=1",
		"NO_PROXY=127.0.0.1,localhost",
		"no_proxy=127.0.0.1,localhost",
		"HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=", "http_proxy=", "https_proxy=", "all_proxy=",
		"OPENAI_API_KEY=", "ANTHROPIC_API_KEY=", "GOOGLE_API_KEY=", "GEMINI_API_KEY=", "MISTRAL_API_KEY=",
		"NO_COLOR=1", "TERM=dumb", "TZ=UTC",
	}
	if runtime.GOOS == "windows" {
		fixture.childEnv = append(fixture.childEnv, "SYSTEMROOT="+os.Getenv("SYSTEMROOT"))
	}
	return fixture
}

func (fixture *durableCLIBinaryFixture) releaseBarrier() {
	fixture.releaseOnce.Do(func() { close(fixture.peer.release) })
}

func (fixture *durableCLIBinaryFixture) run(t *testing.T, args ...string) durableCLIChildResult {
	t.Helper()
	previousLog := fixture.logContents(t)
	command, stdout, stderr, wait := fixture.start(t, args...)
	var err error
	select {
	case err = <-wait:
	case <-time.After(45 * time.Second):
		_ = command.Process.Kill()
		err = <-wait
		t.Fatalf("actual CLI process exceeded 45 seconds: %s %s", fixture.binary, strings.Join(args, " "))
	}
	result := durableCLIChildResult{stdout: stdout.String(), stderr: stderr.String(), err: err, exitCode: command.ProcessState.ExitCode(), state: command.ProcessState.String()}
	currentLog := fixture.logContents(t)
	if strings.HasPrefix(currentLog, previousLog) {
		currentLog = strings.TrimPrefix(currentLog, previousLog)
	}
	t.Logf("child command: %s %s\nchild exit code: %d state: %s error: %v\nchild stdout:\n%s\nchild stderr:\n%s\nraw CLI log records:\n%s", fixture.binary, strings.Join(args, " "), result.exitCode, result.state, result.err, result.stdout, result.stderr, relevantDurableCLILogRecords(currentLog))
	return result
}

func (fixture *durableCLIBinaryFixture) start(t *testing.T, args ...string) (*exec.Cmd, *authPersistenceOutput, *authPersistenceOutput, <-chan error) {
	t.Helper()
	command := exec.Command(fixture.binary, args...)
	command.Dir = fixture.workDir
	command.Env = append([]string(nil), fixture.childEnv...)
	stdout := newAuthPersistenceOutput()
	stderr := newAuthPersistenceOutput()
	command.Stdout = stdout
	command.Stderr = stderr
	require.NoError(t, command.Start())
	wait := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		wait <- command.Wait()
		close(finished)
	}()
	t.Cleanup(func() {
		select {
		case <-finished:
			return
		default:
			_ = command.Process.Kill()
			<-finished
		}
	})
	return command, stdout, stderr, wait
}

func (fixture *durableCLIBinaryFixture) logContents(t *testing.T) string {
	t.Helper()
	logPath := filepath.Join(fixture.workDir, ".go-cli", "logs", "go-cli.log")
	data, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Logf("read CLI log: %v", err)
	}
	return string(data)
}

func relevantDurableCLILogRecords(raw string) string {
	var selected []string
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		if strings.Contains(line, "config_load_trusted") ||
			strings.Contains(line, "cli_prompt_durable_session") ||
			strings.Contains(line, "llm_stream_start") ||
			strings.Contains(line, "write.done") ||
			strings.Contains(line, "core.run_slot.released") ||
			strings.Contains(line, "core.loop.finish") ||
			strings.Contains(line, "cli_prompt_complete") ||
			strings.Contains(line, "command_end") {
			selected = append(selected, line)
		}
	}
	return strings.Join(selected, "\n")
}

func (fixture *durableCLIBinaryFixture) readRows(t *testing.T, physicalID string) []session.SessionEntry {
	t.Helper()
	path := filepath.Join(fixture.storeDir, "sessions", physicalID+".jsonl")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	rows := make([]session.SessionEntry, 0)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	for scanner.Scan() {
		var row session.SessionEntry
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &row), "decode physical JSONL row %d", len(rows))
		rows = append(rows, row)
	}
	require.NoError(t, scanner.Err())
	require.NotEmpty(t, rows, "the physical session file must contain real synced records")
	return rows
}

func (fixture *durableCLIBinaryFixture) readWorkspaceIndex(t *testing.T, workspaceID string) []durableCLIWorkspaceIndexRow {
	t.Helper()
	path := workspaceIndexPath(fixture, workspaceID)
	data, err := os.ReadFile(path)
	require.NoError(t, err, "read authoritative synced workspace index %s", path)
	rows := make([]durableCLIWorkspaceIndexRow, 0)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var row durableCLIWorkspaceIndexRow
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &row))
		rows = append(rows, row)
	}
	require.NoError(t, scanner.Err())
	return rows
}

func workspaceIndexPath(fixture *durableCLIBinaryFixture, workspaceID string) string {
	root := fixture.configDir
	if runtime.GOOS == "darwin" {
		root = filepath.Join(fixture.home, "Library", "Application Support")
	} else if runtime.GOOS == "windows" {
		root = filepath.Join(fixture.home, "AppData", "Roaming")
	}
	return filepath.Join(root, "go-cli", "workspace-quarantine", workspaceID+".jsonl")
}

type durableCLIChildResult struct {
	stdout   string
	stderr   string
	err      error
	exitCode int
	state    string
}

func requireCLIChildExit(t *testing.T, result durableCLIChildResult, want int) {
	t.Helper()
	if want == -1 {
		var exitErr *exec.ExitError
		require.ErrorAs(t, result.err, &exitErr)
		require.Equal(t, -1, result.exitCode, "a killed process has no ordinary exit code")
		return
	}
	require.NoError(t, result.err, "child exit=%d state=%s stdout=%s stderr=%s", result.exitCode, result.state, result.stdout, result.stderr)
	require.Equal(t, want, result.exitCode)
}

func requireCLIChildFailure(t *testing.T, result durableCLIChildResult, want int) {
	t.Helper()
	var exitErr *exec.ExitError
	require.ErrorAs(t, result.err, &exitErr, "the fail-closed CLI command must return a nonzero process status")
	require.Equal(t, want, result.exitCode)
}

func waitForDurableCLIOutput(t *testing.T, output *authPersistenceOutput, fragment string) string {
	t.Helper()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		output.mu.Lock()
		text := output.buffer.String()
		if strings.Contains(text, fragment) {
			output.mu.Unlock()
			return durableCLIPhysicalSessionIDFromOutput(t, text)
		}
		changed := output.changed
		output.mu.Unlock()
		select {
		case <-changed:
		case <-timer.C:
			t.Fatalf("actual CLI child did not print %q; stdout/stderr=%q", fragment, output.String())
		}
	}
}

func durableCLIPhysicalSessionIDFromOutput(t *testing.T, output string) string {
	t.Helper()
	const prefix = "Session ID: "
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			id := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			require.NotEmpty(t, id)
			return id
		}
	}
	t.Fatalf("actual CLI child did not print its physical session ID: %q", output)
	return ""
}

func waitForDurableCLIBarrier(t *testing.T, barrier <-chan struct{}) {
	t.Helper()
	select {
	case <-barrier:
	case <-time.After(20 * time.Second):
		t.Fatal("actual CLI child did not reach the deterministic provider response barrier")
	}
}

func waitForDurableReceipt(t *testing.T, path, physicalID string) []session.SessionEntry {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			var rows []session.SessionEntry
			valid := true
			scanner := bufio.NewScanner(bytes.NewReader(data))
			scanner.Buffer(make([]byte, 4096), 4*1024*1024)
			for scanner.Scan() {
				var row session.SessionEntry
				if json.Unmarshal(scanner.Bytes(), &row) != nil {
					valid = false
					break
				}
				rows = append(rows, row)
			}
			if valid && scanner.Err() == nil {
				for _, row := range rows {
					if row.SessionID == physicalID && row.Type == session.EntryTypeTool && row.ReceiptOutcome == "success" && row.ToolName == "write" {
						return rows
					}
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("actual CLI child did not sync a successful write receipt to %s", path)
	return nil
}

func waitForDurableBashDispatch(t *testing.T, fixture *durableCLIBinaryFixture, physicalID, toolCallID string) ([]session.SessionEntry, durableCLIAction) {
	t.Helper()
	sessionPath := filepath.Join(fixture.storeDir, "sessions", physicalID+".jsonl")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		rows, ok := readDurableRowsForPoll(sessionPath)
		if ok {
			if action, ready := durableBashActionForCall(rows, physicalID, toolCallID); ready {
				indexRows, indexOK := readWorkspaceIndexForPoll(fixture, action.workspaceID)
				if indexOK && hasWorkspaceDispatch(indexRows, physicalID, action.actionID, action.toolCallID) {
					return rows, action
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("actual CLI did not sync the Bash intent and its global dispatch marker for session %q / tool call %q", physicalID, toolCallID)
	return nil, durableCLIAction{}
}

func readDurableRowsForPoll(path string) ([]session.SessionEntry, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	rows := make([]session.SessionEntry, 0)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	for scanner.Scan() {
		var row session.SessionEntry
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			return nil, false
		}
		rows = append(rows, row)
	}
	return rows, scanner.Err() == nil && len(rows) > 0
}

func durableBashActionForCall(rows []session.SessionEntry, physicalID, toolCallID string) (durableCLIAction, bool) {
	return durableActionForCall(rows, physicalID, toolCallID, "bash")
}

func durableActionForCall(rows []session.SessionEntry, physicalID, toolCallID, toolName string) (durableCLIAction, bool) {
	var assistant *session.SessionEntry
	var intent *session.SessionEntry
	for i := range rows {
		row := &rows[i]
		if row.SessionID != physicalID {
			continue
		}
		if row.Type == session.EntryTypeAssistant {
			for _, call := range row.ToolCalls {
				if call.ID == toolCallID && call.Name == toolName {
					assistant = row
				}
			}
		}
		if row.Type == session.EntryTypeTool && row.ActionState == session.ActionStateIntent && row.ToolCallID == toolCallID && row.ToolName == toolName {
			intent = row
		}
	}
	if assistant == nil || intent == nil || intent.ActionID == "" || intent.WorkspaceID == "" {
		return durableCLIAction{}, false
	}
	if assistant.RunID == "" || intent.RunID != assistant.RunID || intent.WorkspaceID == "" {
		return durableCLIAction{}, false
	}
	return durableCLIAction{actionID: intent.ActionID, toolCallID: intent.ToolCallID, workspaceID: intent.WorkspaceID}, true
}

func requirePendingWriteAttempt(t *testing.T, rows []session.SessionEntry, physicalID, toolCallID string) durableCLIAction {
	t.Helper()
	action, ok := durableActionForCall(rows, physicalID, toolCallID, "write")
	require.True(t, ok, "the fresh session must durably record the write call and its intent before the workspace rejects dispatch")
	require.NotEmpty(t, action.actionID)
	require.NotEmpty(t, action.workspaceID)
	return action
}

func requireRecoveredInterruptedWriteReceipt(t *testing.T, rows []session.SessionEntry, physicalID string, action durableCLIAction) session.SessionEntry {
	t.Helper()
	receipts := receiptsForCall(rows, action.toolCallID)
	require.Len(t, receipts, 1, "exact reopen must record one receipt based on the absent global dispatch marker")
	receipt := receipts[0]
	require.Equal(t, physicalID, receipt.SessionID)
	require.NotEmpty(t, receipt.ID)
	require.Equal(t, "write", receipt.ToolName)
	require.Equal(t, "interrupted", receipt.ReceiptOutcome)
	require.Equal(t, "not_dispatched", string(receipt.DispatchState))
	require.Equal(t, action.actionID, receipt.ActionID)
	require.Equal(t, action.workspaceID, receipt.WorkspaceID)
	require.NotEmpty(t, receipt.RunID)
	return receipt
}

func hasWorkspaceDispatch(rows []durableCLIWorkspaceIndexRow, physicalID, actionID, toolCallID string) bool {
	for _, row := range rows {
		if row.Kind != "dispatch_ready" {
			continue
		}
		for _, action := range row.Actions {
			if action.Action.SessionID == physicalID && action.Action.ActionID == actionID && action.Action.ToolCallID == toolCallID {
				return true
			}
		}
	}
	return false
}

func readWorkspaceIndexForPoll(fixture *durableCLIBinaryFixture, workspaceID string) ([]durableCLIWorkspaceIndexRow, bool) {
	data, err := os.ReadFile(workspaceIndexPath(fixture, workspaceID))
	if err != nil {
		return nil, false
	}
	rows := make([]durableCLIWorkspaceIndexRow, 0)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var row durableCLIWorkspaceIndexRow
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			return nil, false
		}
		rows = append(rows, row)
	}
	return rows, scanner.Err() == nil && len(rows) > 0
}

type durableCLIAction struct {
	actionID    string
	toolCallID  string
	workspaceID string
}

func requireDurableWriteLifecycle(t *testing.T, rows []session.SessionEntry, physicalID string) durableCLIAction {
	t.Helper()
	seenIDs := make(map[string]struct{}, len(rows))
	var intents, receipts, executions, toolCalls []session.SessionEntry
	var actionIDs = make(map[string]struct{})
	for _, row := range rows {
		require.NotEmpty(t, row.ID, "every physical JSONL row has a source ID")
		if _, duplicate := seenIDs[row.ID]; duplicate {
			t.Fatalf("physical JSONL repeats source ID %q", row.ID)
		}
		seenIDs[row.ID] = struct{}{}
		require.Equal(t, uint16(1), row.RecordVersion, "all durable production rows retain explicit schema metadata: %+v", row)
		require.Equal(t, physicalID, row.SessionID, "each source row is scoped to the exact physical session")
		if len(row.ToolCalls) > 0 {
			toolCalls = append(toolCalls, row)
		}
		if row.ActionState == session.ActionStateIntent {
			intents = append(intents, row)
		}
		if row.ActionState == session.ActionStateExecution {
			executions = append(executions, row)
		}
		if row.Type == session.EntryTypeTool && row.ReceiptOutcome != "" {
			receipts = append(receipts, row)
		}
		if row.ActionID != "" {
			require.NotEmpty(t, row.ActionBatchID)
			require.NotEmpty(t, row.ToolCallID)
			require.NotEmpty(t, row.RunID)
			require.NotEmpty(t, row.WorkspaceID)
			actionIDs[row.ActionID] = struct{}{}
		}
	}
	require.Len(t, toolCalls, 1, "exactly one real provider tool call produced a canonical source row")
	require.Len(t, toolCalls[0].ToolCalls, 1)
	require.Equal(t, "write", toolCalls[0].ToolCalls[0].Name)
	require.Len(t, intents, 1, "one synced intent precedes the managed write dispatch")
	require.NotEmpty(t, executions, "the physical journal records executor lifecycle evidence")
	require.Len(t, receipts, 1, "one successful receipt records the single managed write effect")
	require.Equal(t, "success", receipts[0].ReceiptOutcome)
	require.Equal(t, "dispatched", string(receipts[0].DispatchState))
	require.Equal(t, "write", receipts[0].ToolName)
	require.Len(t, actionIDs, 1, "a restart must not mint a second action for the original tool call")
	actionID := intents[0].ActionID
	require.NotEmpty(t, actionID)
	require.Equal(t, actionID, receipts[0].ActionID)
	require.Equal(t, toolCalls[0].ToolCalls[0].ID, receipts[0].ToolCallID)
	require.Equal(t, toolCalls[0].RunID, intents[0].RunID)
	require.Equal(t, toolCalls[0].WorkspaceID, intents[0].WorkspaceID)
	return durableCLIAction{actionID: actionID, toolCallID: receipts[0].ToolCallID, workspaceID: intents[0].WorkspaceID}
}

func receiptsForCall(rows []session.SessionEntry, toolCallID string) []session.SessionEntry {
	var receipts []session.SessionEntry
	for _, row := range rows {
		if row.Type == session.EntryTypeTool && row.ActionState == "" && row.ToolCallID == toolCallID && row.ReceiptOutcome != "" {
			receipts = append(receipts, row)
		}
	}
	return receipts
}

func requireRecoveredUnknownBashReceipt(t *testing.T, rows []session.SessionEntry, physicalID string, action durableCLIAction) session.SessionEntry {
	t.Helper()
	receipts := receiptsForCall(rows, action.toolCallID)
	require.Len(t, receipts, 1, "the exact Bash occurrence has one durable receipt after recovery")
	receipt := receipts[0]
	require.Equal(t, physicalID, receipt.SessionID)
	require.NotEmpty(t, receipt.ID)
	require.Equal(t, "bash", receipt.ToolName)
	require.Equal(t, "unknown", receipt.ReceiptOutcome, "recovery must preserve the unresolved process result as unknown")
	require.Equal(t, "dispatched", string(receipt.DispatchState))
	require.Equal(t, action.actionID, receipt.ActionID)
	require.Equal(t, action.workspaceID, receipt.WorkspaceID)
	require.NotEmpty(t, receipt.RunID)
	return receipt
}

func requireDurableSourceIDsUnique(t *testing.T, rows []session.SessionEntry, physicalID string) {
	t.Helper()
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		require.NotEmpty(t, row.ID)
		require.Equal(t, uint16(1), row.RecordVersion)
		require.Equal(t, physicalID, row.SessionID)
		if _, duplicate := seen[row.ID]; duplicate {
			t.Fatalf("physical JSONL repeats source ID %q", row.ID)
		}
		seen[row.ID] = struct{}{}
	}
}

func requireRunTerminals(t *testing.T, rows []session.SessionEntry, physicalID string, want int) []session.SessionEntry {
	t.Helper()
	terminals := make([]session.SessionEntry, 0)
	seenRuns := make(map[string]struct{})
	seenIDs := make(map[string]struct{})
	for _, row := range rows {
		if row.Type != session.EntryTypeCheckpoint || row.CheckpointKind != session.CheckpointRunTerminal {
			continue
		}
		require.Equal(t, physicalID, row.SessionID)
		require.NotEmpty(t, row.ID)
		require.NotEmpty(t, row.RunID)
		require.NotEmpty(t, row.WorkspaceID)
		require.Equal(t, "success", string(row.RunOutcome))
		if _, duplicate := seenRuns[row.RunID]; duplicate {
			t.Fatalf("physical JSONL has more than one run terminal for run %q", row.RunID)
		}
		if _, duplicate := seenIDs[row.ID]; duplicate {
			t.Fatalf("physical JSONL repeats terminal source ID %q", row.ID)
		}
		seenRuns[row.RunID] = struct{}{}
		seenIDs[row.ID] = struct{}{}
		var lastTranscriptSeq uint64
		for _, message := range rows {
			if message.RunID == row.RunID && message.TranscriptIndex != nil && message.Seq > lastTranscriptSeq {
				lastTranscriptSeq = message.Seq
			}
		}
		require.Greater(t, row.Seq, lastTranscriptSeq, "terminal checkpoint follows the completed transcript for its run")
		terminals = append(terminals, row)
	}
	require.Len(t, terminals, want, "assert the actual on-disk terminal checkpoints for completed child runs")
	return terminals
}

func requireDurableTranscriptSequence(t *testing.T, rows []session.SessionEntry, prompts []string, firstRunCompleted bool) {
	t.Helper()
	transcript := make([]session.SessionEntry, 0)
	indicesByRun := make(map[string][]uint64)
	for _, row := range rows {
		if row.TranscriptIndex == nil {
			continue
		}
		transcript = append(transcript, row)
		indicesByRun[row.RunID] = append(indicesByRun[row.RunID], *row.TranscriptIndex)
	}
	sort.Slice(transcript, func(i, j int) bool { return transcript[i].Seq < transcript[j].Seq })
	for runID, indices := range indicesByRun {
		require.NotEmpty(t, runID)
		sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
		for index, transcriptIndex := range indices {
			require.Equal(t, uint64(index), transcriptIndex, "run %q transcript source indices are contiguous", runID)
		}
	}
	wantCount := 3 + len(prompts) - 1
	if firstRunCompleted {
		wantCount++
	}
	if len(prompts) > 1 {
		wantCount++
	}
	require.Len(t, transcript, wantCount, "the actual JSONL transcript has only accepted inputs, one write call/receipt, and completed assistant messages")
	index := 0
	require.Equal(t, session.EntryTypeUser, transcript[index].Type)
	require.Equal(t, prompts[0], transcript[index].Content)
	index++
	require.Equal(t, session.EntryTypeAssistant, transcript[index].Type)
	require.Len(t, transcript[index].ToolCalls, 1)
	require.Equal(t, "write", transcript[index].ToolCalls[0].Name)
	toolCallID := transcript[index].ToolCalls[0].ID
	index++
	require.Equal(t, session.EntryTypeTool, transcript[index].Type)
	require.Equal(t, toolCallID, transcript[index].ToolCallID)
	require.Equal(t, "success", transcript[index].ReceiptOutcome)
	index++
	if firstRunCompleted {
		require.Equal(t, session.EntryTypeAssistant, transcript[index].Type)
		require.Equal(t, "provider completed call", transcript[index].Content)
		index++
	}
	if len(prompts) > 1 {
		require.Equal(t, session.EntryTypeUser, transcript[index].Type)
		require.Equal(t, prompts[1], transcript[index].Content)
		index++
		require.Equal(t, session.EntryTypeAssistant, transcript[index].Type)
		require.Equal(t, "provider completed call", transcript[index].Content)
	}
}

type durableCLIWorkspaceIndexRow struct {
	Version   uint16 `json:"version"`
	Seq       uint64 `json:"seq"`
	RecordID  string `json:"record_id"`
	Kind      string `json:"kind"`
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
	Actions   []struct {
		Action struct {
			ActionID   string `json:"action_id"`
			ToolCallID string `json:"tool_call_id"`
			SessionID  string `json:"session_id"`
		} `json:"action"`
	} `json:"actions"`
	Action *struct {
		ActionID   string `json:"action_id"`
		ToolCallID string `json:"tool_call_id"`
		SessionID  string `json:"session_id"`
	} `json:"action,omitempty"`
}

func requireWorkspaceActionLifecycle(t *testing.T, rows []durableCLIWorkspaceIndexRow, physicalID, actionID string, wantDispatch, wantResolution int) {
	requireWorkspaceActionLifecycleForCall(t, rows, physicalID, actionID, "call-durable-write-once", wantDispatch, wantResolution)
}

func requireWorkspaceActionLifecycleForCall(t *testing.T, rows []durableCLIWorkspaceIndexRow, physicalID, actionID, toolCallID string, wantDispatch, wantResolution int) {
	t.Helper()
	dispatches, resolutions := 0, 0
	seenRecordIDs := make(map[string]struct{}, len(rows))
	seenSeq := make(map[uint64]struct{}, len(rows))
	for _, row := range rows {
		require.Equal(t, uint16(1), row.Version)
		require.NotEmpty(t, row.RecordID)
		require.Greater(t, row.Seq, uint64(0))
		if _, duplicate := seenRecordIDs[row.RecordID]; duplicate {
			t.Fatalf("workspace index repeats source record ID %q", row.RecordID)
		}
		if _, duplicate := seenSeq[row.Seq]; duplicate {
			t.Fatalf("workspace index repeats sequence %d", row.Seq)
		}
		seenRecordIDs[row.RecordID] = struct{}{}
		seenSeq[row.Seq] = struct{}{}
		switch row.Kind {
		case "dispatch_ready":
			for _, action := range row.Actions {
				if action.Action.ActionID == actionID {
					dispatches++
					require.Equal(t, physicalID, action.Action.SessionID)
					require.Equal(t, toolCallID, action.Action.ToolCallID)
				}
			}
		case "execution_resolution":
			if row.Action != nil && row.Action.ActionID == actionID {
				resolutions++
				require.Equal(t, physicalID, row.Action.SessionID)
				require.Equal(t, toolCallID, row.Action.ToolCallID)
			}
		}
	}
	require.Equal(t, wantDispatch, dispatches, "the authoritative workspace JSONL has one synced dispatch marker")
	require.Equal(t, wantResolution, resolutions, "the authoritative workspace JSONL records the matching effect-resolution fact")
}

func requireWorkspaceActionTotals(t *testing.T, rows []durableCLIWorkspaceIndexRow, wantDispatches, wantResolutions int) {
	t.Helper()
	dispatches, resolutions := 0, 0
	for _, row := range rows {
		switch row.Kind {
		case "dispatch_ready":
			dispatches += len(row.Actions)
		case "execution_resolution":
			resolutions++
		}
	}
	require.Equal(t, wantDispatches, dispatches, "the durable workspace index contains only expected dispatched actions")
	require.Equal(t, wantResolutions, resolutions, "the durable workspace index contains only expected action resolutions")
}

func requireSuccessfulScopedReader(t *testing.T, rows []session.SessionEntry, physicalID, toolCallID, toolName string) durableCLIAction {
	t.Helper()
	action, ok := durableActionForCall(rows, physicalID, toolCallID, toolName)
	require.True(t, ok, "the real provider call has one matching persisted %s intent", toolName)
	receipts := receiptsForCall(rows, toolCallID)
	require.Len(t, receipts, 1, "the real scoped reader has one persisted completion receipt")
	receipt := receipts[0]
	require.Equal(t, physicalID, receipt.SessionID)
	require.NotEmpty(t, receipt.ID)
	require.Equal(t, toolName, receipt.ToolName)
	require.Equal(t, "success", receipt.ReceiptOutcome)
	require.Equal(t, "dispatched", string(receipt.DispatchState))
	require.Equal(t, action.actionID, receipt.ActionID)
	require.Equal(t, action.workspaceID, receipt.WorkspaceID)
	require.NotEmpty(t, receipt.RunID)
	var executions []session.SessionEntry
	for _, row := range rows {
		if row.ActionState == session.ActionStateExecution && row.ToolCallID == toolCallID {
			executions = append(executions, row)
			require.Equal(t, action.actionID, row.ActionID)
		}
	}
	require.NotEmpty(t, executions, "the real scoped reader has persisted executor evidence")
	return action
}

func logDurableFileEvidence(t *testing.T, phase, physicalID string, rows []session.SessionEntry, action durableCLIAction, terminals []session.SessionEntry) {
	t.Helper()
	sourceIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		sourceIDs = append(sourceIDs, row.ID)
	}
	terminalEvidence := make([]string, 0, len(terminals))
	for _, terminal := range terminals {
		terminalEvidence = append(terminalEvidence, fmt.Sprintf("id=%s run=%s outcome=%s seq=%d", terminal.ID, terminal.RunID, terminal.RunOutcome, terminal.Seq))
	}
	t.Logf("%s physical_id=%s jsonl_rows=%d source_ids=%v action_id=%s tool_call_id=%s workspace_id=%s terminals=%v", phase, physicalID, len(rows), sourceIDs, action.actionID, action.toolCallID, action.workspaceID, terminalEvidence)
}

type durableCLIPeer struct {
	mu             sync.Mutex
	bodies         [][]byte
	authorizations []string
	responses      []durableCLIPeerResponse
	blockAfterTool bool
	barrierReached chan struct{}
	release        chan struct{}
	barrierOnce    sync.Once
}

type durableCLIPeerResponse struct {
	toolCallID string
	toolName   string
	arguments  map[string]string
	content    string
}

func newDurableCLIPeer(blockAfterTool bool) *durableCLIPeer {
	return &durableCLIPeer{
		blockAfterTool: blockAfterTool,
		barrierReached: make(chan struct{}),
		release:        make(chan struct{}),
	}
}

func durableCLIToolCall(toolCallID, toolName string, arguments map[string]string) durableCLIPeerResponse {
	return durableCLIPeerResponse{toolCallID: toolCallID, toolName: toolName, arguments: arguments}
}

func durableCLIFinal(content string) durableCLIPeerResponse {
	return durableCLIPeerResponse{content: content}
}

func (peer *durableCLIPeer) setResponsePlan(responses ...durableCLIPeerResponse) {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	peer.responses = append([]durableCLIPeerResponse(nil), responses...)
}

func (peer *durableCLIPeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" {
		http.Error(w, "unexpected path", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "cannot read request", http.StatusBadRequest)
		return
	}
	peer.mu.Lock()
	index := len(peer.bodies)
	peer.bodies = append(peer.bodies, append([]byte(nil), body...))
	peer.authorizations = append(peer.authorizations, r.Header.Get("Authorization"))
	var planned durableCLIPeerResponse
	plannedResponse := index < len(peer.responses)
	if plannedResponse {
		planned = peer.responses[index]
	}
	peer.mu.Unlock()

	if peer.blockAfterTool && index == 1 {
		peer.barrierOnce.Do(func() { close(peer.barrierReached) })
		select {
		case <-peer.release:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if plannedResponse {
		writeDurableCLIPeerResponse(w, index, planned)
		return
	}
	if index == 0 {
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-durable-write","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-durable-write-once","type":"function","function":{"name":"write","arguments":"{\"path\":\"durable-effect.txt\",\"content\":\"effect-once\"}"}}]},"finish_reason":"tool_calls"}]}`)
		return
	}
	_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-durable-final-%d","choices":[{"index":0,"message":{"role":"assistant","content":"provider completed call"},"finish_reason":"stop"}]}`, index)
}

func writeDurableCLIPeerResponse(w http.ResponseWriter, index int, planned durableCLIPeerResponse) {
	message := map[string]any{"role": "assistant"}
	finishReason := "stop"
	if planned.toolName != "" {
		arguments, err := json.Marshal(planned.arguments)
		if err != nil {
			http.Error(w, "cannot encode planned tool arguments", http.StatusInternalServerError)
			return
		}
		message["content"] = nil
		message["tool_calls"] = []any{map[string]any{
			"id":   planned.toolCallID,
			"type": "function",
			"function": map[string]string{
				"name":      planned.toolName,
				"arguments": string(arguments),
			},
		}}
		finishReason = "tool_calls"
	} else {
		if planned.content == "" {
			planned.content = "provider completed call"
		}
		message["content"] = planned.content
	}
	response := map[string]any{
		"id": "chatcmpl-durable-plan-" + fmt.Sprint(index),
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	body, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "cannot encode planned provider response", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(body)
}

func (peer *durableCLIPeer) requestBodies() [][]byte {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	bodies := make([][]byte, len(peer.bodies))
	for i, body := range peer.bodies {
		bodies[i] = append([]byte(nil), body...)
	}
	return bodies
}

func waitForPeerRequests(t *testing.T, peer *durableCLIPeer, count int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		peer.mu.Lock()
		if len(peer.bodies) >= count {
			authorizations := append([]string(nil), peer.authorizations...)
			peer.mu.Unlock()
			for _, authorization := range authorizations {
				require.Equal(t, "Bearer "+durableCLIFixtureAPIKey, authorization)
			}
			return
		}
		peer.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("loopback OpenAI peer received fewer than %d actual CLI requests", count)
}

func (fixture *durableCLIBinaryFixture) assertPeerRequests(t *testing.T, count int) [][]byte {
	t.Helper()
	waitForPeerRequests(t, fixture.peer, count)
	bodies := fixture.peer.requestBodies()
	require.Len(t, bodies, count)
	return bodies
}
