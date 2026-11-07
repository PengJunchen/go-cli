//go:build darwin || linux

package gatecapture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

const (
	readyFixtureModeEnv    = "VERIFY_GATE_CAPTURE_READY_MODE"
	readyFixtureReadyEnv   = "VERIFY_GATE_CAPTURE_READY_PATH"
	readyFixtureSpawnedEnv = "VERIFY_GATE_CAPTURE_SPAWNED_PATH"
	readyFixtureWatchdog   = 20 * time.Second
	readyRecordWatchdog    = 5 * time.Second
	readyRunMargin         = time.Second
	missingReadyWindow     = 250 * time.Millisecond
)

type nativeProcessIdentity struct {
	PID        int    `json:"pid"`
	PPID       int    `json:"ppid"`
	PGID       int    `json:"pgid"`
	BirthToken string `json:"birth_token"`
}

type readyHolderRecord struct {
	Parent nativeProcessIdentity `json:"parent"`
	Child  nativeProcessIdentity `json:"child"`
}

type readyRunOutcome struct {
	result          Result
	err             error
	processState    *os.ProcessState
	cleanupSnapshot runReturnCleanupSnapshot
}

type readyPipeFixture struct {
	cancel         context.CancelFunc
	ackReader      *os.File
	ackWriter      *os.File
	readyPath      string
	spawnedPath    string
	waitDelay      time.Duration
	done           chan readyRunOutcome
	expectedRecord chan readyHolderRecord
	cleanupRecord  *readyHolderRecord
}

// TestGateCaptureReadyPipeHolderFixture is the subprocess fixture for the
// parent-exit pipe-holder tests. The child writes its own native identity
// before the parent waits for the controller's separate acknowledgement pipe.
func TestGateCaptureReadyPipeHolderFixture(t *testing.T) {
	mode := os.Getenv(readyFixtureModeEnv)
	if mode == "" {
		return
	}

	switch mode {
	case "spawn-ready-parent", "spawn-missing-ready-parent":
		childMode := "hold-ready"
		if mode == "spawn-missing-ready-parent" {
			childMode = "hold-missing-ready"
		}
		child := exec.Command(os.Args[0], "-test.run=^TestGateCaptureReadyPipeHolderFixture$")
		child.Env = envWith(readyFixtureModeEnv, childMode)
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			t.Fatalf("start ready pipe-holder child: %v", err)
		}

		ack := os.NewFile(3, "ready-pipe-holder-ack")
		if ack == nil {
			t.Fatal("fixture acknowledgement descriptor is unavailable")
		}
		defer ack.Close()
		var value [1]byte
		n, err := io.ReadFull(ack, value[:])
		if err != nil {
			// Test cleanup closes the writer on early failure. Returning normally
			// lets the gate-owned process group handle the remaining child.
			return
		}
		if n != 1 || value[0] != 1 {
			t.Fatalf("invalid fixture acknowledgement: bytes=%d value=%d", n, value[0])
		}
		return

	case "hold-ready", "hold-missing-ready":
		parent, err := readNativeProcessIdentity(os.Getppid())
		if err != nil {
			t.Fatalf("capture fixture parent identity: %v", err)
		}
		child, err := readNativeProcessIdentity(os.Getpid())
		if err != nil {
			t.Fatalf("capture fixture child identity: %v", err)
		}
		if child.PPID != parent.PID || child.PGID != parent.PGID {
			t.Fatalf("fixture child is outside its expected parent group: parent=%+v child=%+v", parent, child)
		}
		record := readyHolderRecord{Parent: parent, Child: child}
		if err := writeReadyHolderRecord(os.Getenv(readyFixtureSpawnedEnv), record); err != nil {
			t.Fatalf("write child-start identity record: %v", err)
		}
		if mode == "hold-ready" {
			if err := writeReadyHolderRecord(os.Getenv(readyFixtureReadyEnv), record); err != nil {
				t.Fatalf("write child readiness record: %v", err)
			}
		}
		time.Sleep(readyFixtureWatchdog)
		return

	default:
		t.Fatalf("unknown ready pipe-holder fixture mode %q", mode)
	}
}

func TestRunStopsReadyDescendantAfterLeaderExit(t *testing.T) {
	const waitDelay = 3 * time.Second
	fixture := newReadyPipeFixture(t, "spawn-ready-parent", waitDelay)
	joined := false
	t.Cleanup(func() { fixture.cleanup(t, &joined) })

	record, err := waitReadyHolderRecord(fixture.readyPath, time.Now().Add(readyRecordWatchdog))
	if err != nil {
		t.Fatalf("child readiness record was not published: %v", err)
	}
	fixture.cleanupRecord = &record
	if err := validateReadyHolderRecord(record); err != nil {
		t.Fatalf("ready child identity is not natively verified: %v", err)
	}
	fixture.expectedRecord <- record
	if outcome, ok := fixture.takeOutcome(); ok {
		joined = true
		t.Fatalf("Run returned before the parent-exit acknowledgement: err=%v state=%v", outcome.err, outcome.processState)
	}

	acknowledgedAt := time.Now()
	if err := fixture.acknowledgeParentExit(); err != nil {
		t.Fatalf("acknowledge fixture parent: %v", err)
	}
	parentDeadline := acknowledgedAt.Add(waitDelay - processCleanupBudget)
	parentGone, err := waitForNativeIdentityGone(record.Parent, parentDeadline)
	if err != nil {
		t.Fatalf("check original parent identity after acknowledgement: %v", err)
	}
	if !parentGone {
		t.Fatalf("fixture parent identity remained live before WaitDelay: parent=%+v", record.Parent)
	}
	if elapsed := time.Since(acknowledgedAt); elapsed >= waitDelay {
		t.Fatalf("parent-exit checks completed after the %s WaitDelay window: elapsed=%s", waitDelay, elapsed)
	}
	if err := requireNativeIdentityInGroup(record.Child, record.Child.PGID); err != nil {
		t.Fatalf("ready child is not live in its recorded group after parent exit: %v", err)
	}
	groupAlive, err := nativeProcessGroupAlive(record.Child.PGID)
	if err != nil {
		t.Fatalf("inspect original process group after parent exit: %v", err)
	}
	if !groupAlive {
		t.Fatalf("original process group %d vanished while the ready child was expected to hold pipes", record.Child.PGID)
	}
	if outcome, ok := fixture.takeOutcome(); ok {
		joined = true
		t.Fatalf("Run completed before controlled cancellation and before WaitDelay: err=%v state=%v", outcome.err, outcome.processState)
	}

	runBound := waitDelay + processCleanupBudget + readyRunMargin
	cancelledAt := time.Now()
	fixture.cancel()
	outcome, ok := fixture.waitOutcome(runBound)
	if !ok {
		t.Fatalf("Run did not finish within WaitDelay + process cleanup + margin (%s)", runBound)
	}
	joined = true
	cancelElapsed := time.Since(cancelledAt)
	t.Logf("ready-holder evidence: parent=%+v child=%+v parent_gone_before_cancel=%t run_pending_after_parent_gone=true cancel_to_run_return=%s run_bound=%s run_err_type=%T run_err=%v err_wait_delay=%t err_residual_descendants=%t err_cleanup_unconfirmed=%t process_exit_code=%d", record.Parent, record.Child, parentGone, cancelElapsed, runBound, outcome.err, outcome.err, errors.Is(outcome.err, exec.ErrWaitDelay), errors.Is(outcome.err, ErrResidualDescendants), errors.Is(outcome.err, ErrProcessCleanupUnconfirmed), processExitCode(outcome.processState))
	if cancelElapsed > runBound {
		t.Errorf("Run returned after %s, exceeding WaitDelay + process cleanup + margin %s", cancelElapsed, runBound)
	}

	if outcome.processState == nil || !outcome.processState.Success() || outcome.processState.ExitCode() != 0 {
		t.Errorf("normal-exit parent state = %v, want exit 0", outcome.processState)
	}
	if outcome.err == nil {
		t.Error("Run() returned nil after cancellation with a ready descendant that held the pipes")
	}
	if errors.Is(outcome.err, ErrProcessCleanupUnconfirmed) {
		t.Errorf("Run() could not confirm cleanup of the owned process group: %v", outcome.err)
	}
	snapshot := outcome.cleanupSnapshot
	t.Logf("ready-holder Run-return snapshot: captured_at=%s record_known=%t expected_child=%+v current_child=%+v child_absent=%t child_probe_error=%v group_absent=%t group_probe_error=%v", snapshot.capturedAt.Format(time.RFC3339Nano), snapshot.recordKnown, snapshot.expectedChild, snapshot.currentChild, snapshot.childAbsent, snapshot.childProbeErr, snapshot.groupAbsent, snapshot.groupProbeErr)
	if !snapshot.recordKnown {
		t.Error("Run-return cleanup snapshot has no verified expected process identity")
	}
	if !snapshot.childAbsent || snapshot.childProbeErr != nil {
		t.Errorf("ready child identity was live or unknown in the immediate Run-return snapshot: absent=%t err=%v", snapshot.childAbsent, snapshot.childProbeErr)
	}
	if !snapshot.groupAbsent || snapshot.groupProbeErr != nil {
		t.Errorf("original process group was live or unknown in the immediate Run-return snapshot: absent=%t err=%v", snapshot.groupAbsent, snapshot.groupProbeErr)
	}
}

func TestRunRejectsMissingReadyRecord(t *testing.T) {
	const waitDelay = 3 * time.Second
	fixture := newReadyPipeFixture(t, "spawn-missing-ready-parent", waitDelay)
	joined := false
	t.Cleanup(func() { fixture.cleanup(t, &joined) })

	spawned, err := waitReadyHolderRecord(fixture.spawnedPath, time.Now().Add(readyRecordWatchdog))
	if err != nil {
		t.Fatalf("missing-ready child start identity was not published: %v", err)
	}
	fixture.cleanupRecord = &spawned
	if err := validateReadyHolderRecord(spawned); err != nil {
		t.Fatalf("started child identity is not natively verified: %v", err)
	}
	fixture.expectedRecord <- spawned
	if _, err := waitReadyHolderRecord(fixture.readyPath, time.Now().Add(missingReadyWindow)); err == nil {
		t.Fatal("missing-ready fixture was accepted as child readiness")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing-ready fixture produced an unexpected wait error: %v", err)
	} else {
		t.Logf("missing-ready negative control rejected the absent child-written readiness record: %v", err)
	}
	if outcome, ok := fixture.takeOutcome(); ok {
		joined = true
		t.Fatalf("Run completed without child readiness or controller cancellation: err=%v state=%v", outcome.err, outcome.processState)
	}

	cancelledAt := time.Now()
	fixture.cancel()
	fixture.closeAckWriter()
	runBound := waitDelay + processCleanupBudget + readyRunMargin
	outcome, ok := fixture.waitOutcome(runBound)
	if !ok {
		t.Fatal("Run did not join after missing-ready cleanup cancellation")
	}
	joined = true
	cancelElapsed := time.Since(cancelledAt)
	t.Logf("missing-ready cleanup evidence: parent=%+v child=%+v cancel_to_run_return=%s run_bound=%s run_err_type=%T run_err=%v err_cleanup_unconfirmed=%t", spawned.Parent, spawned.Child, cancelElapsed, runBound, outcome.err, outcome.err, errors.Is(outcome.err, ErrProcessCleanupUnconfirmed))
	if cancelElapsed > runBound {
		t.Errorf("missing-ready Run returned after %s, exceeding cleanup bound %s", cancelElapsed, runBound)
	}
	if errors.Is(outcome.err, ErrProcessCleanupUnconfirmed) {
		t.Errorf("missing-ready cleanup left its process group unconfirmed: %v", outcome.err)
	}
	snapshot := outcome.cleanupSnapshot
	t.Logf("missing-ready Run-return snapshot: captured_at=%s record_known=%t expected_child=%+v current_child=%+v child_absent=%t child_probe_error=%v group_absent=%t group_probe_error=%v", snapshot.capturedAt.Format(time.RFC3339Nano), snapshot.recordKnown, snapshot.expectedChild, snapshot.currentChild, snapshot.childAbsent, snapshot.childProbeErr, snapshot.groupAbsent, snapshot.groupProbeErr)
	if !snapshot.recordKnown {
		t.Error("missing-ready Run-return cleanup snapshot has no started child identity")
	}
	if !snapshot.childAbsent || snapshot.childProbeErr != nil {
		t.Errorf("missing-ready child identity was live or unknown in the immediate Run-return snapshot: absent=%t err=%v", snapshot.childAbsent, snapshot.childProbeErr)
	}
	if !snapshot.groupAbsent || snapshot.groupProbeErr != nil {
		t.Errorf("missing-ready process group was live or unknown in the immediate Run-return snapshot: absent=%t err=%v", snapshot.groupAbsent, snapshot.groupProbeErr)
	}
}

func newReadyPipeFixture(t *testing.T, mode string, waitDelay time.Duration) *readyPipeFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ackReader, ackWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("create fixture acknowledgement pipe: %v", err)
	}
	dir := t.TempDir()
	readyPath := filepath.Join(dir, "child-ready.json")
	spawnedPath := filepath.Join(dir, "child-spawned.json")
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGateCaptureReadyPipeHolderFixture$")
	cmd.Env = append(envWith("GORACE", "atexit_sleep_ms=0"),
		readyFixtureModeEnv+"="+mode,
		readyFixtureReadyEnv+"="+readyPath,
		readyFixtureSpawnedEnv+"="+spawnedPath,
	)
	cmd.ExtraFiles = []*os.File{ackReader}
	fixture := &readyPipeFixture{
		cancel:         cancel,
		ackReader:      ackReader,
		ackWriter:      ackWriter,
		readyPath:      readyPath,
		spawnedPath:    spawnedPath,
		waitDelay:      waitDelay,
		done:           make(chan readyRunOutcome, 1),
		expectedRecord: make(chan readyHolderRecord, 1),
	}
	go func() {
		result, runErr := Run(cmd, Limits{StdoutBytes: 1024, StderrBytes: 1024, WaitDelay: waitDelay})
		var record *readyHolderRecord
		select {
		case expected := <-fixture.expectedRecord:
			record = &expected
		default:
		}
		// Sample child identity and group immediately after Run returns. Do not
		// wait for the test goroutine to publish a record or for cleanup polling.
		snapshot := snapshotRunReturnCleanup(record, readNativeProcessIdentity, nativeProcessGroupAlive)
		// Run has joined cmd.Wait before returning. ProcessState is safe to read
		// here; the test never reads it concurrently with this worker.
		state := cmd.ProcessState
		fixture.done <- readyRunOutcome{result: result, err: runErr, processState: state, cleanupSnapshot: snapshot}
	}()
	return fixture
}

func (fixture *readyPipeFixture) takeOutcome() (readyRunOutcome, bool) {
	select {
	case outcome := <-fixture.done:
		_ = fixture.closeAckReader()
		return outcome, true
	default:
		return readyRunOutcome{}, false
	}
}

func (fixture *readyPipeFixture) waitOutcome(bound time.Duration) (readyRunOutcome, bool) {
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case outcome := <-fixture.done:
		_ = fixture.closeAckReader()
		return outcome, true
	case <-timer.C:
		return readyRunOutcome{}, false
	}
}

func (fixture *readyPipeFixture) acknowledgeParentExit() error {
	if fixture.ackWriter == nil {
		return errors.New("fixture acknowledgement writer is closed")
	}
	n, err := fixture.ackWriter.Write([]byte{1})
	closeErr := fixture.ackWriter.Close()
	fixture.ackWriter = nil
	if err != nil {
		return fmt.Errorf("write fixture acknowledgement: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("write fixture acknowledgement: wrote %d of 1 byte", n)
	}
	if closeErr != nil {
		return fmt.Errorf("close fixture acknowledgement writer: %w", closeErr)
	}
	return nil
}

func (fixture *readyPipeFixture) closeAckReader() error {
	if fixture.ackReader == nil {
		return nil
	}
	err := fixture.ackReader.Close()
	fixture.ackReader = nil
	return err
}

func (fixture *readyPipeFixture) closeAckWriter() {
	if fixture.ackWriter == nil {
		return
	}
	_ = fixture.ackWriter.Close()
	fixture.ackWriter = nil
}

func (fixture *readyPipeFixture) closeControl() {
	fixture.closeAckWriter()
}

func (fixture *readyPipeFixture) cleanup(t *testing.T, joined *bool) {
	t.Helper()
	fixture.cancel()
	fixture.closeControl()
	if !*joined {
		outcome, ok := fixture.waitOutcome(fixture.waitDelay + processCleanupBudget + readyRunMargin)
		if ok {
			*joined = true
			t.Logf("joined fixture Run during test cleanup: err_type=%T err=%v", outcome.err, outcome.err)
		} else {
			t.Errorf("fixture Run goroutine did not join within its cleanup bound")
		}
	}
	record := fixture.cleanupRecord
	if record == nil {
		if spawned, err := waitReadyHolderRecord(fixture.spawnedPath, time.Now().Add(10*time.Millisecond)); err == nil {
			record = &spawned
		}
	}
	if record != nil {
		if err := cleanupCapturedProcessGroup(*record); err != nil {
			t.Errorf("test-owned process-group cleanup could not be confirmed: %v; parent=%+v child=%+v", err, record.Parent, record.Child)
		}
	}
	if !*joined {
		outcome, ok := fixture.waitOutcome(processCleanupBudget + readyRunMargin)
		if ok {
			*joined = true
			t.Logf("joined fixture Run after cleanup: err_type=%T err=%v", outcome.err, outcome.err)
		} else {
			t.Errorf("fixture Run goroutine did not join after cleanup")
		}
	}
}

func waitReadyHolderRecord(path string, deadline time.Time) (readyHolderRecord, error) {
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			var record readyHolderRecord
			if err := json.Unmarshal(data, &record); err != nil {
				return readyHolderRecord{}, fmt.Errorf("decode holder record %q: %w", path, err)
			}
			return record, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return readyHolderRecord{}, fmt.Errorf("read holder record %q: %w", path, err)
		}
		if !time.Now().Before(deadline) {
			return readyHolderRecord{}, fmt.Errorf("holder record %q was not published before deadline: %w", path, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func writeReadyHolderRecord(path string, record readyHolderRecord) error {
	if path == "" {
		return errors.New("holder record path is empty")
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode holder identity: %w", err)
	}
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary holder record: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write temporary holder record: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync temporary holder record: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary holder record: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("publish holder record atomically: %w", err)
	}
	return nil
}

func validateReadyHolderRecord(record readyHolderRecord) error {
	if record.Parent.PID <= 0 || record.Child.PID <= 0 || record.Parent.BirthToken == "" || record.Child.BirthToken == "" {
		return errors.New("holder record lacks a native PID or birth token")
	}
	if record.Child.PPID != record.Parent.PID {
		return fmt.Errorf("child PPID %d does not match recorded parent PID %d", record.Child.PPID, record.Parent.PID)
	}
	if record.Parent.PGID != record.Child.PGID || record.Parent.PGID != record.Parent.PID {
		return fmt.Errorf("recorded parent/child process groups differ: parent=%+v child=%+v", record.Parent, record.Child)
	}
	parent, err := readNativeProcessIdentity(record.Parent.PID)
	if err != nil {
		return fmt.Errorf("read live parent identity: %w", err)
	}
	if !sameNativeProcess(parent, record.Parent) || parent.PGID != record.Parent.PGID {
		return fmt.Errorf("live parent identity differs from child record: recorded=%+v current=%+v", record.Parent, parent)
	}
	child, err := readNativeProcessIdentity(record.Child.PID)
	if err != nil {
		return fmt.Errorf("read live child identity: %w", err)
	}
	if !sameNativeProcess(child, record.Child) || child.PPID != record.Parent.PID || child.PGID != record.Child.PGID {
		return fmt.Errorf("live child identity differs from readiness record: recorded=%+v current=%+v", record.Child, child)
	}
	return nil
}

func sameNativeProcess(current, expected nativeProcessIdentity) bool {
	return current.PID == expected.PID && current.BirthToken == expected.BirthToken
}

func waitForNativeIdentityGone(expected nativeProcessIdentity, deadline time.Time) (bool, error) {
	for {
		current, err := readNativeProcessIdentity(expected.PID)
		if err != nil {
			if nativeProcessIsAbsent(err) {
				return true, nil
			}
			return false, fmt.Errorf("read native identity for PID %d: %w", expected.PID, err)
		}
		if !sameNativeProcess(current, expected) {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func requireNativeIdentityInGroup(expected nativeProcessIdentity, pgid int) error {
	current, err := readNativeProcessIdentity(expected.PID)
	if err != nil {
		return fmt.Errorf("read native identity for PID %d: %w", expected.PID, err)
	}
	if !sameNativeProcess(current, expected) || current.PGID != pgid {
		return fmt.Errorf("process identity is not live in PGID %d: recorded=%+v current=%+v", pgid, expected, current)
	}
	return nil
}

func requireNativeIdentityGone(expected nativeProcessIdentity, deadline time.Time) error {
	gone, err := waitForNativeIdentityGone(expected, deadline)
	if err != nil {
		return err
	}
	if !gone {
		return fmt.Errorf("native process identity remains live: %+v", expected)
	}
	return nil
}

func nativeProcessGroupAlive(pgid int) (bool, error) {
	if pgid <= 1 {
		return false, fmt.Errorf("refuse to inspect invalid process group %d", pgid)
	}
	if err := syscall.Kill(-pgid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		if errors.Is(err, syscall.EPERM) {
			return true, nil
		}
		return false, fmt.Errorf("probe process group %d: %w", pgid, err)
	}
	return true, nil
}

func requireNativeProcessGroupGone(pgid int, deadline time.Time) error {
	for {
		alive, err := nativeProcessGroupAlive(pgid)
		if err != nil {
			return err
		}
		if !alive {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("process group %d still has live members", pgid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func cleanupCapturedProcessGroup(record readyHolderRecord) error {
	if record.Parent.PID <= 0 || record.Child.PID <= 0 || record.Parent.PGID <= 1 || record.Parent.PGID != record.Parent.PID || record.Child.PGID != record.Parent.PGID || record.Parent.BirthToken == "" || record.Child.BirthToken == "" {
		return errors.New("refuse group cleanup with an incomplete captured identity record")
	}
	child, err := readNativeProcessIdentity(record.Child.PID)
	if err != nil {
		if nativeProcessIsAbsent(err) {
			return requireNativeProcessGroupGone(record.Child.PGID, time.Now().Add(processCleanupBudget+readyRunMargin))
		}
		return fmt.Errorf("refuse group cleanup with unknown child identity: %w", err)
	}
	groupAlive, groupErr := nativeProcessGroupAlive(record.Child.PGID)
	signalGroup, originalGone, decisionErr := capturedGroupCleanupDecision(record.Child, child, groupAlive, groupErr)
	if decisionErr != nil {
		return decisionErr
	}
	if originalGone {
		return nil
	}
	if !signalGroup {
		return errors.New("refuse group cleanup because ownership could not be confirmed")
	}
	if err := syscall.Kill(-record.Child.PGID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill only the captured fixture process group %d: %w", record.Child.PGID, err)
	}
	if err := requireNativeIdentityGone(record.Child, time.Now().Add(processCleanupBudget+readyRunMargin)); err != nil {
		return err
	}
	return requireNativeProcessGroupGone(record.Child.PGID, time.Now().Add(processCleanupBudget+readyRunMargin))
}

func processExitCode(state *os.ProcessState) int {
	if state == nil {
		return -1
	}
	return state.ExitCode()
}

func TestRunReturnCleanupSnapshotRejectsLateAbsence(t *testing.T) {
	record := readyHolderRecord{
		Parent: nativeProcessIdentity{PID: 101, PPID: 90, PGID: 101, BirthToken: "parent-birth"},
		Child:  nativeProcessIdentity{PID: 102, PPID: 101, PGID: 101, BirthToken: "child-birth"},
	}
	identityReads := 0
	groupReads := 0
	readIdentity := func(pid int) (nativeProcessIdentity, error) {
		identityReads++
		if pid != record.Child.PID {
			t.Fatalf("identity probe PID = %d, want %d", pid, record.Child.PID)
		}
		if identityReads == 1 {
			return record.Child, nil
		}
		return nativeProcessIdentity{}, os.ErrNotExist
	}
	probeGroup := func(pgid int) (bool, error) {
		groupReads++
		if pgid != record.Child.PGID {
			t.Fatalf("group probe PGID = %d, want %d", pgid, record.Child.PGID)
		}
		return groupReads == 1, nil
	}

	snapshot := snapshotRunReturnCleanup(&record, readIdentity, probeGroup)
	if snapshot.childAbsent || snapshot.groupAbsent {
		t.Fatalf("Run-return snapshot laundered live resources through later probes: child_absent=%t group_absent=%t identity_reads=%d group_reads=%d", snapshot.childAbsent, snapshot.groupAbsent, identityReads, groupReads)
	}
	if identityReads != 1 || groupReads != 1 {
		t.Fatalf("Run-return snapshot sampled more than once: identity_reads=%d group_reads=%d", identityReads, groupReads)
	}
	_, laterChildErr := readIdentity(record.Child.PID)
	if !errors.Is(laterChildErr, os.ErrNotExist) {
		t.Fatalf("late child cleanup fixture error = %v, want native absence", laterChildErr)
	}
	laterGroupAlive, laterGroupErr := probeGroup(record.Child.PGID)
	if laterGroupErr != nil || laterGroupAlive {
		t.Fatalf("late process-group cleanup fixture = alive %t, err %v; want absent", laterGroupAlive, laterGroupErr)
	}
	t.Logf("scored Run-return snapshot remains live=%t/%t although later cleanup probes are absent=%t/%t", !snapshot.childAbsent, !snapshot.groupAbsent, errors.Is(laterChildErr, os.ErrNotExist), !laterGroupAlive)
}

func TestCapturedGroupCleanupDecisionAcceptsReplacementOnlyAfterGroupGone(t *testing.T) {
	expected := nativeProcessIdentity{PID: 102, PPID: 101, PGID: 101, BirthToken: "original-birth"}
	replacement := nativeProcessIdentity{PID: 102, PPID: 1, PGID: 202, BirthToken: "replacement-birth"}
	tests := []struct {
		name       string
		current    nativeProcessIdentity
		groupAlive bool
		groupErr   error
		wantSignal bool
		wantGone   bool
		wantErr    bool
	}{
		{name: "same PID replacement after original group ESRCH", current: replacement, wantGone: true},
		{name: "same PID replacement while original group lives", current: replacement, groupAlive: true, wantErr: true},
		{name: "same PID replacement with unknown original group", current: replacement, groupErr: errors.New("group probe unavailable"), wantErr: true},
		{name: "same PID replacement with unknown birth", current: nativeProcessIdentity{PID: 102, PGID: 202}, wantErr: true},
		{name: "wrong PID after original group ESRCH", current: nativeProcessIdentity{PID: 999, PGID: 101, BirthToken: "other"}, wantErr: true},
		{name: "same original process in live group", current: expected, groupAlive: true, wantSignal: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			signalGroup, originalGone, err := capturedGroupCleanupDecision(expected, test.current, test.groupAlive, test.groupErr)
			if signalGroup != test.wantSignal || originalGone != test.wantGone || (err != nil) != test.wantErr {
				t.Fatalf("cleanup decision = signal %t, original gone %t, err %v; want signal %t, original gone %t, wantErr %t", signalGroup, originalGone, err, test.wantSignal, test.wantGone, test.wantErr)
			}
		})
	}
}

type nativeIdentityReader func(pid int) (nativeProcessIdentity, error)

type nativeProcessGroupProbe func(pgid int) (bool, error)

type runReturnCleanupSnapshot struct {
	capturedAt    time.Time
	recordKnown   bool
	expectedChild nativeProcessIdentity
	currentChild  nativeProcessIdentity
	childAbsent   bool
	childProbeErr error
	groupAbsent   bool
	groupProbeErr error
}

func snapshotRunReturnCleanup(record *readyHolderRecord, readIdentity nativeIdentityReader, probeGroup nativeProcessGroupProbe) runReturnCleanupSnapshot {
	snapshot := runReturnCleanupSnapshot{capturedAt: time.Now()}
	if record == nil {
		snapshot.childProbeErr = errors.New("no verified child identity was available before Run returned")
		snapshot.groupProbeErr = errors.New("no verified process-group identity was available before Run returned")
		return snapshot
	}
	snapshot.recordKnown = true
	snapshot.expectedChild = record.Child
	current, err := readIdentity(record.Child.PID)
	if err != nil {
		if nativeProcessIsAbsent(err) {
			snapshot.childAbsent = true
		} else {
			snapshot.childProbeErr = fmt.Errorf("read child identity at Run return: %w", err)
		}
	} else {
		snapshot.currentChild = current
		snapshot.childAbsent = !sameNativeProcess(current, record.Child)
	}
	alive, err := probeGroup(record.Child.PGID)
	if err != nil {
		snapshot.groupProbeErr = fmt.Errorf("inspect process group at Run return: %w", err)
	} else {
		snapshot.groupAbsent = !alive
	}
	return snapshot
}

func capturedGroupCleanupDecision(expected, current nativeProcessIdentity, groupAlive bool, groupErr error) (signalGroup, originalGone bool, err error) {
	if expected.PID <= 0 || expected.PGID <= 1 || expected.BirthToken == "" || current.BirthToken == "" {
		return false, false, errors.New("captured process-group cleanup identity is incomplete")
	}
	if current.PID != expected.PID {
		return false, false, fmt.Errorf("current process PID %d differs from captured PID %d", current.PID, expected.PID)
	}
	if !sameNativeProcess(current, expected) {
		if groupErr != nil {
			return false, false, fmt.Errorf("cannot confirm original process group after PID reuse: %w", groupErr)
		}
		if groupAlive {
			return false, false, fmt.Errorf("refuse cleanup because PID %d was reused while original group %d remains", expected.PID, expected.PGID)
		}
		return false, true, nil
	}
	if current.PGID != expected.PGID {
		return false, false, fmt.Errorf("current process group %d differs from captured group %d", current.PGID, expected.PGID)
	}
	if groupErr != nil {
		return false, false, fmt.Errorf("cannot confirm captured process group state: %w", groupErr)
	}
	if !groupAlive {
		return false, false, errors.New("captured process identity remains but original process group is absent")
	}
	return true, false, nil
}
