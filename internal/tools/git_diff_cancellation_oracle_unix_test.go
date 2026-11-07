//go:build darwin || linux

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type gitCancelIdentity struct {
	pid, pgid int
	birth     string
}

type gitCancelExpected struct {
	host          string
	leader, child gitCancelIdentity
	target        string
}

type gitCancelPIDSample struct {
	pid, expectedPGID int
	birth             string
	observedPGID      int
	birthErr, killErr error
}

type gitCancelSample struct {
	done          bool
	observedAt    time.Time
	host          string
	hostErr       error
	pgid          int
	groupErr      error
	leader, child gitCancelPIDSample
	proof         PreparedCompletionProof
	target        string
}

func captureGitCancelIdentity(pid int) (gitCancelIdentity, error) {
	birth, pgid, err := processStartToken(pid)
	if err != nil {
		return gitCancelIdentity{}, fmt.Errorf("capture PID %d: %w", pid, err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return gitCancelIdentity{}, fmt.Errorf("check captured PID %d: %w", pid, err)
	}
	return gitCancelIdentity{pid: pid, pgid: pgid, birth: birth}, nil
}

func checkGitCancelCapturedIdentity(expected gitCancelIdentity) error {
	current, err := captureGitCancelIdentity(expected.pid)
	if err != nil {
		return err
	}
	if current != expected {
		return fmt.Errorf("PID %d no longer matches the captured birth and process group", expected.pid)
	}
	return nil
}

// sampleGitCancelPID never retries: an unreadable identity is not evidence of
// absence, and a readable original identity is evidence of presence.
func sampleGitCancelPID(expected gitCancelIdentity) gitCancelPIDSample {
	birth, pgid, birthErr := processStartToken(expected.pid)
	killErr := syscall.Kill(expected.pid, 0)
	return gitCancelPIDSample{
		pid: expected.pid, expectedPGID: expected.pgid, birth: birth,
		observedPGID: pgid, birthErr: birthErr, killErr: killErr,
	}
}

func sampleGitCancelDone(expected gitCancelExpected, signal PreparedCompletionSignal) gitCancelSample {
	sample := gitCancelSample{done: true, observedAt: time.Now(), pgid: expected.leader.pgid}
	sample.leader = sampleGitCancelPID(expected.leader)
	sample.child = sampleGitCancelPID(expected.child)
	sample.groupErr = syscall.Kill(-expected.leader.pgid, 0)
	sample.host, sample.hostErr = currentHostGeneration()
	sample.proof = signal.Proof()
	return sample
}

// gitCancelLeaderStopped binds the ps state to the captured birth identity on
// both sides of the kernel-state query. No signal is sent to the sampled PID.
func gitCancelLeaderStopped(ctx context.Context, leader gitCancelIdentity) (bool, error) {
	if err := checkGitCancelCapturedIdentity(leader); err != nil {
		return false, err
	}
	output, err := exec.CommandContext(ctx, "ps", "-o", "stat=", "-p", strconv.Itoa(leader.pid)).Output()
	if err != nil {
		return false, fmt.Errorf("inspect stopped process state: %w", err)
	}
	if err := checkGitCancelCapturedIdentity(leader); err != nil {
		return false, err
	}
	fields := strings.Fields(string(output))
	if len(fields) != 1 || fields[0] == "" {
		return false, fmt.Errorf("unexpected process state output %q", output)
	}
	return fields[0][0] == 'T' || fields[0][0] == 't', nil
}

// cleanupGitCancelGroup is failure-path insurance, never part of the witness.
// A fresh identity match is required before signaling a captured group number.
func cleanupGitCancelGroup(t *testing.T, expected gitCancelExpected) {
	t.Helper()
	if expected.leader.pid <= 0 || expected.leader.pgid != expected.leader.pid || expected.host == "" {
		return
	}
	err := syscall.Kill(-expected.leader.pgid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return
	}
	if err != nil {
		t.Errorf("cannot inspect fixture group during cleanup: %v", err)
		return
	}
	host, err := currentHostGeneration()
	if err != nil || host != expected.host {
		t.Errorf("cannot establish fixture host identity during cleanup: host=%q err=%v", host, err)
		return
	}
	for _, identity := range []gitCancelIdentity{expected.leader, expected.child} {
		if identity.pid <= 0 || identity.pgid != expected.leader.pgid || identity.birth == "" {
			continue
		}
		if checkGitCancelCapturedIdentity(identity) != nil {
			continue
		}
		if err := syscall.Kill(-expected.leader.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("kill verified fixture group during cleanup: %v", err)
		}
		return
	}
	t.Error("fixture group remains but no original member has a verified birth identity; refusing to signal")
}

func checkGitCancelSample(expected gitCancelExpected, sample gitCancelSample) error {
	if expected.host == "" || expected.leader.pid <= 0 || expected.child.pid <= 0 ||
		expected.leader.pid == expected.child.pid || expected.leader.pgid != expected.leader.pid ||
		expected.child.pgid != expected.leader.pgid || expected.leader.birth == "" || expected.child.birth == "" {
		return errors.New("invalid captured Git process identities")
	}
	if !sample.done {
		return errors.New("queue Done was not observed")
	}
	if sample.hostErr != nil {
		return fmt.Errorf("inspect host generation: %w", sample.hostErr)
	}
	if sample.host != expected.host {
		return errors.New("host generation changed")
	}
	if sample.pgid != expected.leader.pgid {
		return errors.New("sampled process group does not match captured group")
	}
	if !errors.Is(sample.groupErr, syscall.ESRCH) {
		if sample.groupErr == nil {
			return errors.New("captured process group is still present")
		}
		return fmt.Errorf("inspect captured process group: %w", sample.groupErr)
	}
	if err := checkGitCancelPID(expected.leader, sample.leader); err != nil {
		return fmt.Errorf("fake Git leader: %w", err)
	}
	if err := checkGitCancelPID(expected.child, sample.child); err != nil {
		return fmt.Errorf("fake Git child: %w", err)
	}
	if sample.proof != CompletionQueuedJobJoined {
		return fmt.Errorf("unexpected queue completion proof: %q", sample.proof)
	}
	if sample.target != expected.target {
		return errors.New("canceled Git preview changed the target file")
	}
	return nil
}

func checkGitCancelPID(expected gitCancelIdentity, sample gitCancelPIDSample) error {
	if sample.pid != expected.pid || sample.expectedPGID != expected.pgid {
		return errors.New("sampled identity metadata does not match the captured identity")
	}
	if sample.killErr != nil && !errors.Is(sample.killErr, syscall.ESRCH) {
		return fmt.Errorf("inspect PID: %w", sample.killErr)
	}
	if sample.birthErr == nil {
		if sample.birth == "" || sample.observedPGID <= 0 {
			return errors.New("incomplete native process identity")
		}
		if sample.birth == expected.birth {
			return errors.New("original process was present after Done")
		}
		// A different native birth token for this exact PID proves reuse; the
		// group is independently required to be absent by the caller.
		return nil
	}
	if sample.birth != "" || sample.observedPGID != 0 {
		return errors.New("inconsistent native process identity observation")
	}
	missingIdentity := (runtime.GOOS == "darwin" && errors.Is(sample.birthErr, syscall.EIO)) ||
		(runtime.GOOS == "linux" && errors.Is(sample.birthErr, os.ErrNotExist))
	if !missingIdentity {
		return fmt.Errorf("inspect process birth token: %w", sample.birthErr)
	}
	if !errors.Is(sample.killErr, syscall.ESRCH) {
		return errors.New("process identity unavailable without positive PID absence")
	}
	return nil
}

func TestGitCancellationOracleGate(t *testing.T) {
	absentBirthErr := error(os.ErrNotExist)
	if runtime.GOOS == "darwin" {
		absentBirthErr = syscall.EIO
	}
	baseExpected := gitCancelExpected{
		host:   "same-host-generation",
		leader: gitCancelIdentity{pid: 101, pgid: 101, birth: "original-leader"},
		child:  gitCancelIdentity{pid: 102, pgid: 101, birth: "original-child"},
		target: "old content\n",
	}
	baseSample := gitCancelSample{
		done:     true,
		host:     baseExpected.host,
		pgid:     baseExpected.leader.pgid,
		groupErr: syscall.ESRCH,
		leader:   gitCancelPIDSample{pid: 101, expectedPGID: 101, birthErr: absentBirthErr, killErr: syscall.ESRCH},
		child:    gitCancelPIDSample{pid: 102, expectedPGID: 101, birthErr: absentBirthErr, killErr: syscall.ESRCH},
		proof:    CompletionQueuedJobJoined,
		target:   baseExpected.target,
	}
	present := func(s *gitCancelPIDSample, identity gitCancelIdentity) {
		s.birthErr, s.killErr = nil, nil
		s.birth, s.observedPGID = identity.birth, identity.pgid
	}
	reused := func(s *gitCancelPIDSample) {
		s.birthErr, s.killErr = nil, nil
		s.birth, s.observedPGID = "different-native-birth", 900
	}
	tests := []struct {
		name    string
		mutate  func(*gitCancelExpected, *gitCancelSample)
		wantErr bool
	}{
		{name: "both_absent", mutate: func(*gitCancelExpected, *gitCancelSample) {}},
		{name: "reused_pid_alive", mutate: func(_ *gitCancelExpected, s *gitCancelSample) { reused(&s.leader) }},
		{name: "reused_pid_now_absent", mutate: func(_ *gitCancelExpected, s *gitCancelSample) { reused(&s.child); s.child.killErr = syscall.ESRCH }},
		{name: "original_leader_alive", mutate: func(e *gitCancelExpected, s *gitCancelSample) { present(&s.leader, e.leader) }, wantErr: true},
		{name: "original_child_alive_or_zombie", mutate: func(e *gitCancelExpected, s *gitCancelSample) { present(&s.child, e.child) }, wantErr: true},
		{name: "same_birth_then_absent", mutate: func(e *gitCancelExpected, s *gitCancelSample) {
			present(&s.child, e.child)
			s.child.killErr = syscall.ESRCH
		}, wantErr: true},
		{name: "other_group_member_alive", mutate: func(_ *gitCancelExpected, s *gitCancelSample) { s.groupErr = nil }, wantErr: true},
		{name: "group_denied", mutate: func(_ *gitCancelExpected, s *gitCancelSample) { s.groupErr = syscall.EPERM }, wantErr: true},
		{name: "group_unknown", mutate: func(_ *gitCancelExpected, s *gitCancelSample) { s.groupErr = syscall.EIO }, wantErr: true},
		{name: "pid_denied", mutate: func(_ *gitCancelExpected, s *gitCancelSample) { s.child.killErr = syscall.EPERM }, wantErr: true},
		{name: "pid_unknown", mutate: func(_ *gitCancelExpected, s *gitCancelSample) { s.leader.killErr = syscall.EIO }, wantErr: true},
		{name: "unpaired_birth_error", mutate: func(_ *gitCancelExpected, s *gitCancelSample) { s.child.killErr = nil }, wantErr: true},
		{name: "birth_denied_even_after_esrch", mutate: func(_ *gitCancelExpected, s *gitCancelSample) { s.child.birthErr = syscall.EPERM }, wantErr: true},
		{name: "unexpected_birth_error", mutate: func(_ *gitCancelExpected, s *gitCancelSample) { s.child.birthErr = errors.New("unknown") }, wantErr: true},
		{name: "sample_pid_mismatch", mutate: func(_ *gitCancelExpected, s *gitCancelSample) { s.leader.pid++ }, wantErr: true},
		{name: "sample_expected_pgid_mismatch", mutate: func(_ *gitCancelExpected, s *gitCancelSample) { s.child.expectedPGID++ }, wantErr: true},
		{name: "sample_group_mismatch", mutate: func(_ *gitCancelExpected, s *gitCancelSample) { s.pgid++ }, wantErr: true},
		{name: "reused_pid_sampled_in_old_group_but_group_absent_at_check", mutate: func(_ *gitCancelExpected, s *gitCancelSample) { reused(&s.child); s.child.observedPGID = s.pgid }},
		{name: "unreadable_replacement_identity", mutate: func(_ *gitCancelExpected, s *gitCancelSample) { reused(&s.child); s.child.birth = "" }, wantErr: true},
		{name: "zero_replacement_group", mutate: func(_ *gitCancelExpected, s *gitCancelSample) { reused(&s.child); s.child.observedPGID = 0 }, wantErr: true},
		{name: "zero_capture_pid", mutate: func(e *gitCancelExpected, _ *gitCancelSample) { e.child.pid = 0 }, wantErr: true},
		{name: "identical_capture_pids", mutate: func(e *gitCancelExpected, _ *gitCancelSample) { e.child.pid = e.leader.pid }, wantErr: true},
		{name: "bad_capture_group", mutate: func(e *gitCancelExpected, _ *gitCancelSample) { e.child.pgid++ }, wantErr: true},
		{name: "missing_capture_birth", mutate: func(e *gitCancelExpected, _ *gitCancelSample) { e.child.birth = "" }, wantErr: true},
		{name: "missing_capture_host", mutate: func(e *gitCancelExpected, _ *gitCancelSample) { e.host = "" }, wantErr: true},
		{name: "changed_host", mutate: func(_ *gitCancelExpected, s *gitCancelSample) { s.host = "different-host" }, wantErr: true},
		{name: "unknown_host", mutate: func(_ *gitCancelExpected, s *gitCancelSample) { s.hostErr = syscall.EIO }, wantErr: true},
		{name: "done_not_observed", mutate: func(_ *gitCancelExpected, s *gitCancelSample) { s.done = false }, wantErr: true},
		{name: "missing_proof", mutate: func(_ *gitCancelExpected, s *gitCancelSample) { s.proof = "" }, wantErr: true},
		{name: "wrong_proof", mutate: func(_ *gitCancelExpected, s *gitCancelSample) { s.proof = CompletionOwnedProcessGroupJoined }, wantErr: true},
		{name: "target_changed", mutate: func(_ *gitCancelExpected, s *gitCancelSample) { s.target = "new content\n" }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expected, sample := baseExpected, baseSample
			tt.mutate(&expected, &sample)
			err := checkGitCancelSample(expected, sample)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
