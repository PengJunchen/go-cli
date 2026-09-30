//go:build linux

package core

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testLinuxProcessGroupIdentity() ExecutionIdentity {
	return ExecutionIdentity{
		Kind:              ExecutionIdentityProcessGroup,
		Platform:          "linux",
		HostGeneration:    "linux:boot-a",
		PIDNamespaceToken: "linux:pidns:v1:dev:ino:init-start",
		LeaderPID:         37,
		ProcessStartToken: "linux:leader-start",
		ProcessGroupID:    41,
	}
}

func TestInspectLinuxProcessGroupExitRequiresPIDNamespaceToken(t *testing.T) {
	identity := testLinuxProcessGroupIdentity()
	identity.PIDNamespaceToken = ""
	called := false

	inspection := inspectLinuxProcessGroupExit(identity, identity.HostGeneration, identity.PIDNamespaceToken, func(int) (string, int, string, error) {
		called = true
		return "", 0, "", nil
	}, func(int) error {
		called = true
		return syscall.ESRCH
	})

	if inspection.Status != ProcessGroupExitUnknown || called {
		t.Fatalf("inspection = %#v, probe called = %t; missing namespace must remain unknown without probing", inspection, called)
	}
}

func TestInspectLinuxProcessGroupExitRequiresMatchingHostAndPIDNamespace(t *testing.T) {
	for _, test := range []struct {
		name      string
		host      string
		namespace string
	}{
		{name: "different host generation", host: "linux:boot-b", namespace: "linux:pidns:v1:dev:ino:init-start"},
		{name: "different PID namespace", host: "linux:boot-a", namespace: "linux:pidns:v1:other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity := testLinuxProcessGroupIdentity()
			called := false
			inspection := inspectLinuxProcessGroupExit(identity, test.host, test.namespace, func(int) (string, int, string, error) {
				called = true
				return "", 0, "", nil
			}, func(int) error {
				called = true
				return syscall.ESRCH
			})
			if inspection.Status != ProcessGroupExitUnknown || called {
				t.Fatalf("inspection = %#v, probe called = %t; mismatched runtime identity must remain unknown", inspection, called)
			}
		})
	}
}

func TestInspectLinuxProcessGroupExitMapsGroupProbeResults(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want ProcessGroupExitStatus
	}{
		{name: "group exists", want: ProcessGroupExitPresent},
		{name: "no such group while leader remains", err: syscall.ESRCH, want: ProcessGroupExitUnknown},
		{name: "permission denied", err: syscall.EPERM, want: ProcessGroupExitUnknown},
		{name: "other probe error", err: errors.New("probe failed"), want: ProcessGroupExitUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity := testLinuxProcessGroupIdentity()
			observedGroupID := 0
			inspection := inspectLinuxProcessGroupExit(identity, identity.HostGeneration, identity.PIDNamespaceToken, matchingLinuxLeaderProbe(identity), func(groupID int) error {
				observedGroupID = groupID
				return test.err
			})
			if inspection.Status != test.want {
				t.Fatalf("status = %q, want %q (inspection: %#v)", inspection.Status, test.want, inspection)
			}
			if observedGroupID != identity.ProcessGroupID {
				t.Fatalf("probed process group %d, want persisted group %d", observedGroupID, identity.ProcessGroupID)
			}
		})
	}
}

func TestInspectLinuxProcessGroupExitKeepsEscapedLeaderUnknown(t *testing.T) {
	identity := testLinuxProcessGroupIdentity()
	groupProbeCalled := false
	inspection := inspectLinuxProcessGroupExit(identity, identity.HostGeneration, identity.PIDNamespaceToken, func(int) (string, int, string, error) {
		return identity.ProcessStartToken, identity.ProcessGroupID + 1, "S", nil
	}, func(int) error {
		groupProbeCalled = true
		return syscall.ESRCH
	})
	if inspection.Status != ProcessGroupExitUnknown || groupProbeCalled {
		t.Fatalf("inspection = %#v, group probe called = %t; a still-live leader outside its recorded group is ambiguous", inspection, groupProbeCalled)
	}
}

func TestInspectLinuxProcessGroupExitKeepsUnreadableLeaderUnknown(t *testing.T) {
	identity := testLinuxProcessGroupIdentity()
	groupProbeCalled := false
	inspection := inspectLinuxProcessGroupExit(identity, identity.HostGeneration, identity.PIDNamespaceToken, func(int) (string, int, string, error) {
		return "", 0, "", os.ErrPermission
	}, func(int) error {
		groupProbeCalled = true
		return syscall.ESRCH
	})
	if inspection.Status != ProcessGroupExitUnknown || groupProbeCalled {
		t.Fatalf("inspection = %#v, group probe called = %t; an inaccessible leader is ambiguous", inspection, groupProbeCalled)
	}
}

func TestInspectLinuxProcessGroupExitChecksRemainingGroupWhenLeaderIsGoneOrReused(t *testing.T) {
	for _, test := range []struct {
		name       string
		leaderRead func(int) (string, int, string, error)
		groupErr   error
		want       ProcessGroupExitStatus
	}{
		{
			name:       "leader exited and group absent",
			leaderRead: func(int) (string, int, string, error) { return "", 0, "", os.ErrNotExist },
			groupErr:   syscall.ESRCH,
			want:       ProcessGroupExitAbsent,
		},
		{
			name:       "leader PID reused and group absent",
			leaderRead: func(int) (string, int, string, error) { return "linux:other-start", 99, "S", nil },
			groupErr:   syscall.ESRCH,
			want:       ProcessGroupExitAbsent,
		},
		{
			name:       "leader exited and numeric group ID reused",
			leaderRead: func(int) (string, int, string, error) { return "", 0, "", os.ErrNotExist },
			want:       ProcessGroupExitUnknown,
		},
		{
			name:       "leader PID reused and numeric group ID still exists",
			leaderRead: func(int) (string, int, string, error) { return "linux:other-start", 99, "S", nil },
			want:       ProcessGroupExitUnknown,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity := testLinuxProcessGroupIdentity()
			groupProbeCalled := false
			inspection := inspectLinuxProcessGroupExit(identity, identity.HostGeneration, identity.PIDNamespaceToken, test.leaderRead, func(groupID int) error {
				groupProbeCalled = true
				if groupID != identity.ProcessGroupID {
					t.Fatalf("probed group %d, want %d", groupID, identity.ProcessGroupID)
				}
				return test.groupErr
			})
			if inspection.Status != test.want || !groupProbeCalled {
				t.Fatalf("inspection = %#v, group probe called = %t; group state after leader exit/reuse = %q, want %q", inspection, groupProbeCalled, inspection.Status, test.want)
			}
		})
	}
}

func TestInspectLinuxProcessGroupExitRechecksLeaderAcrossGroupProbe(t *testing.T) {
	identity := testLinuxProcessGroupIdentity()
	matchingLeader := linuxLeaderObservation{startToken: identity.ProcessStartToken, processGroupID: identity.ProcessGroupID, state: "S"}
	for _, test := range []struct {
		name         string
		observations []linuxLeaderObservation
		groupErr     error
		want         ProcessGroupExitStatus
	}{
		{
			name:         "leader remains in the group",
			observations: []linuxLeaderObservation{matchingLeader, matchingLeader},
			want:         ProcessGroupExitPresent,
		},
		{
			name: "leader becomes zombie while group exists",
			observations: []linuxLeaderObservation{
				matchingLeader,
				{startToken: identity.ProcessStartToken, processGroupID: identity.ProcessGroupID, state: "Z"},
			},
			want: ProcessGroupExitUnknown,
		},
		{
			name:         "leader exits while group exists",
			observations: []linuxLeaderObservation{matchingLeader, {err: os.ErrNotExist}},
			want:         ProcessGroupExitUnknown,
		},
		{
			name:         "leader exits and group is absent",
			observations: []linuxLeaderObservation{matchingLeader, {err: os.ErrNotExist}},
			groupErr:     syscall.ESRCH,
			want:         ProcessGroupExitAbsent,
		},
		{
			name:         "leader escapes before absent probe",
			observations: []linuxLeaderObservation{matchingLeader, {startToken: identity.ProcessStartToken, processGroupID: identity.ProcessGroupID + 1}},
			groupErr:     syscall.ESRCH,
			want:         ProcessGroupExitUnknown,
		},
		{
			name:         "leader escapes while group exists",
			observations: []linuxLeaderObservation{matchingLeader, {startToken: identity.ProcessStartToken, processGroupID: identity.ProcessGroupID + 1}},
			want:         ProcessGroupExitUnknown,
		},
		{
			name:         "leader becomes inaccessible after absent probe",
			observations: []linuxLeaderObservation{matchingLeader, {err: os.ErrPermission}},
			groupErr:     syscall.ESRCH,
			want:         ProcessGroupExitUnknown,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			leaderProbe := func(pid int) (string, int, string, error) {
				if pid != identity.LeaderPID {
					t.Fatalf("leader probe received PID %d, want %d", pid, identity.LeaderPID)
				}
				if calls >= len(test.observations) {
					t.Fatalf("unexpected leader probe call %d", calls+1)
				}
				observation := test.observations[calls]
				calls++
				return observation.startToken, observation.processGroupID, observation.state, observation.err
			}
			inspection := inspectLinuxProcessGroupExit(identity, identity.HostGeneration, identity.PIDNamespaceToken, leaderProbe, func(groupID int) error {
				if groupID != identity.ProcessGroupID {
					t.Fatalf("group probe received PGID %d, want %d", groupID, identity.ProcessGroupID)
				}
				return test.groupErr
			})
			if inspection.Status != test.want {
				t.Fatalf("status = %q, want %q (inspection: %#v)", inspection.Status, test.want, inspection)
			}
			if calls != len(test.observations) {
				t.Fatalf("leader probe calls = %d, want %d", calls, len(test.observations))
			}
		})
	}
}

func TestInspectLinuxProcessGroupExitKeepsZombieLeaderUnknown(t *testing.T) {
	for _, test := range []struct {
		name     string
		groupErr error
	}{
		{name: "kernel reports group exists"},
		{name: "kernel reports no group", groupErr: syscall.ESRCH},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity := testLinuxProcessGroupIdentity()
			calls := 0
			inspection := inspectLinuxProcessGroupExit(identity, identity.HostGeneration, identity.PIDNamespaceToken, func(pid int) (string, int, string, error) {
				if pid != identity.LeaderPID {
					t.Fatalf("leader probe received PID %d, want %d", pid, identity.LeaderPID)
				}
				calls++
				return identity.ProcessStartToken, identity.ProcessGroupID, "Z", nil
			}, func(groupID int) error {
				if groupID != identity.ProcessGroupID {
					t.Fatalf("group probe received PGID %d, want %d", groupID, identity.ProcessGroupID)
				}
				return test.groupErr
			})
			if inspection.Status != ProcessGroupExitUnknown {
				t.Fatalf("inspection = %#v, want unknown for a zombie leader", inspection)
			}
			wantCalls := 1
			if test.groupErr != nil {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Fatalf("leader probe calls = %d, want %d", calls, wantCalls)
			}
		})
	}
}

func TestInspectLinuxProcessGroupExitRejectsNonProcessGroupIdentity(t *testing.T) {
	identity := testLinuxProcessGroupIdentity()
	identity.Kind = ExecutionIdentityWindowsJob
	called := false

	inspection := inspectLinuxProcessGroupExit(identity, identity.HostGeneration, identity.PIDNamespaceToken, matchingLinuxLeaderProbe(identity), func(int) error {
		called = true
		return syscall.ESRCH
	})

	if inspection.Status != ProcessGroupExitUnknown || called {
		t.Fatalf("inspection = %#v, probe called = %t; unsupported identity must remain unknown", inspection, called)
	}
}

func TestInspectLinuxProcessGroupExitUsesReadOnlyGroupProbe(t *testing.T) {
	identity := testLinuxProcessGroupIdentity()
	observedGroupID := 0
	inspection := inspectLinuxProcessGroupExit(identity, identity.HostGeneration, identity.PIDNamespaceToken, func(int) (string, int, string, error) {
		return "linux:reused-start", identity.ProcessGroupID, "S", nil
	}, func(groupID int) error {
		observedGroupID = groupID
		return syscall.ESRCH
	})
	if inspection.Status != ProcessGroupExitAbsent {
		t.Fatalf("inspection = %#v, want absent", inspection)
	}
	if observedGroupID == identity.LeaderPID {
		t.Fatal("inspector probed the recovered leader PID instead of the process group")
	}
}

func TestInspectLinuxProcessGroupExitRejectsGroupOneProbe(t *testing.T) {
	identity := testLinuxProcessGroupIdentity()
	identity.ProcessGroupID = 1
	called := false
	inspection := inspectLinuxProcessGroupExit(identity, identity.HostGeneration, identity.PIDNamespaceToken, matchingLinuxLeaderProbe(identity), func(int) error {
		called = true
		return syscall.ESRCH
	})
	if inspection.Status != ProcessGroupExitUnknown || called {
		t.Fatalf("inspection = %#v, group probe called = %t; kill(-1, 0) is not a process-group-one probe", inspection, called)
	}
}

func TestInspectLinuxProcessGroupExitRejectsPIDMaxAsUnallocatable(t *testing.T) {
	pidMax, err := currentLinuxPIDMax()
	if err != nil {
		t.Fatalf("currentLinuxPIDMax() error = %v", err)
	}
	identity := testLinuxProcessGroupIdentity()
	identity.ProcessGroupID = pidMax
	groupProbeCalled := false
	inspection := inspectLinuxProcessGroupExit(identity, identity.HostGeneration, identity.PIDNamespaceToken, func(int) (string, int, string, error) {
		return "", 0, "", os.ErrNotExist
	}, func(int) error {
		groupProbeCalled = true
		return syscall.ESRCH
	})
	if inspection.Status != ProcessGroupExitUnknown || groupProbeCalled {
		t.Fatalf("inspection = %#v, group probe called = %t; pid_max itself is not an allocatable PID or PGID", inspection, groupProbeCalled)
	}
}

func TestParseLinuxProcessStatHandlesParenthesesInCommandName(t *testing.T) {
	fields := make([]string, 20)
	fields[0] = "S"
	fields[1] = "1"
	fields[2] = "41"
	for index := 3; index < 19; index++ {
		fields[index] = "0"
	}
	fields[19] = "98765"
	stat := []byte("4321 (worker ) with spaces) " + strings.Join(fields, " "))

	startToken, processGroupID, err := parseLinuxProcessStat(stat)
	if err != nil {
		t.Fatalf("parseLinuxProcessStat() error = %v", err)
	}
	if startToken != "linux:98765" || processGroupID != 41 {
		t.Fatalf("parsed process identity = (%q, %d), want (linux:98765, 41)", startToken, processGroupID)
	}
}

func matchingLinuxLeaderProbe(identity ExecutionIdentity) func(int) (string, int, string, error) {
	return func(pid int) (string, int, string, error) {
		if pid != identity.LeaderPID {
			return "", 0, "", errors.New("inspector probed a PID other than the persisted leader")
		}
		return identity.ProcessStartToken, identity.ProcessGroupID, "S", nil
	}
}

type linuxLeaderObservation struct {
	startToken     string
	processGroupID int
	state          string
	err            error
}

func TestCurrentPIDNamespaceTokenIsStableAndNamespaced(t *testing.T) {
	first, err := CurrentPIDNamespaceToken()
	if err != nil {
		t.Fatalf("CurrentPIDNamespaceToken() error = %v", err)
	}
	second, err := CurrentPIDNamespaceToken()
	if err != nil {
		t.Fatalf("second CurrentPIDNamespaceToken() error = %v", err)
	}
	if first == "" || first != second {
		t.Fatalf("namespace tokens = %q and %q; want a stable non-empty token", first, second)
	}
	if len(first) < len("linux:pidns:v1:") || first[:len("linux:pidns:v1:")] != "linux:pidns:v1:" {
		t.Fatalf("namespace token %q lacks the Linux PID namespace prefix", first)
	}
}

func TestInspectProcessGroupExitFindsCurrentGroup(t *testing.T) {
	command, identity := startProcessGroupInspectorChild(t)
	hostGeneration, err := currentLinuxHostGeneration()
	if err != nil {
		t.Fatalf("currentLinuxHostGeneration() error = %v", err)
	}
	pidNamespaceToken, err := CurrentPIDNamespaceToken()
	if err != nil {
		t.Fatalf("CurrentPIDNamespaceToken() error = %v", err)
	}
	inspection := InspectProcessGroupExit(ExecutionIdentity{
		Kind:              ExecutionIdentityProcessGroup,
		Platform:          "linux",
		HostGeneration:    hostGeneration,
		PIDNamespaceToken: pidNamespaceToken,
		LeaderPID:         command.Process.Pid,
		ProcessStartToken: identity.ProcessStartToken,
		ProcessGroupID:    identity.ProcessGroupID,
	})
	if inspection.Status != ProcessGroupExitPresent || inspection.ObservedAt.IsZero() {
		t.Fatalf("isolated process group inspection = %#v, want an observed present group", inspection)
	}
}

func TestInspectProcessGroupExitProvesStoppedGroup(t *testing.T) {
	command, identity := startProcessGroupInspectorChild(t)
	hostGeneration, err := currentLinuxHostGeneration()
	if err != nil {
		t.Fatalf("currentLinuxHostGeneration() error = %v", err)
	}
	pidNamespaceToken, err := CurrentPIDNamespaceToken()
	if err != nil {
		t.Fatalf("CurrentPIDNamespaceToken() error = %v", err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatalf("stop isolated process-group child: %v", err)
	}
	if err := command.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("wait for stopped child: %v", err)
		}
	}
	identity.HostGeneration = hostGeneration
	identity.PIDNamespaceToken = pidNamespaceToken
	inspection := InspectProcessGroupExit(identity)
	if inspection.Status != ProcessGroupExitAbsent || inspection.ObservedAt.IsZero() {
		t.Fatalf("stopped process group inspection = %#v, want observed absent group", inspection)
	}
}

func TestInspectProcessGroupExitKeepsZombieOnlyGroupUnknown(t *testing.T) {
	command, identity := startProcessGroupInspectorChild(t)
	if err := command.Process.Kill(); err != nil {
		t.Fatalf("stop isolated process-group child: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, _, state, err := inspectLinuxLeader(command.Process.Pid)
		if err == nil && state == "Z" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("isolated child did not become a zombie before deadline (state %q, error %v)", state, err)
		}
		time.Sleep(time.Millisecond)
	}

	hostGeneration, err := currentLinuxHostGeneration()
	if err != nil {
		t.Fatalf("currentLinuxHostGeneration() error = %v", err)
	}
	identity.HostGeneration = hostGeneration
	pidNamespaceToken, err := CurrentPIDNamespaceToken()
	if err != nil {
		t.Fatalf("CurrentPIDNamespaceToken() error = %v", err)
	}
	identity.PIDNamespaceToken = pidNamespaceToken
	inspection := InspectProcessGroupExit(identity)
	if inspection.Status != ProcessGroupExitUnknown {
		t.Fatalf("zombie-only group inspection = %#v, want unknown until the zombie is reaped", inspection)
	}
}

func startProcessGroupInspectorChild(t *testing.T) (*exec.Cmd, ExecutionIdentity) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	command := exec.Command(executable, "-test.run=^TestProcessGroupInspectorChild$")
	command.Env = append(os.Environ(), "GOCLI_PROCESS_GROUP_INSPECTOR_CHILD=1")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatalf("start isolated process-group child: %v", err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	startToken, processGroupID, err := readLinuxProcessStat(command.Process.Pid)
	if err != nil {
		t.Fatalf("read isolated child identity: %v", err)
	}
	if processGroupID != command.Process.Pid {
		t.Fatalf("isolated child's process group = %d, want leader PID %d", processGroupID, command.Process.Pid)
	}
	return command, ExecutionIdentity{
		Kind:              ExecutionIdentityProcessGroup,
		Platform:          "linux",
		LeaderPID:         command.Process.Pid,
		ProcessStartToken: startToken,
		ProcessGroupID:    processGroupID,
	}
}

func TestProcessGroupInspectorChild(t *testing.T) {
	if os.Getenv("GOCLI_PROCESS_GROUP_INSPECTOR_CHILD") != "1" {
		return
	}
	select {}
}

func TestInspectProcessGroupExitIsUnknownForLegacyIdentity(t *testing.T) {
	identity := testLinuxProcessGroupIdentity()
	identity.PIDNamespaceToken = ""
	inspection := InspectProcessGroupExit(identity)
	if inspection.Status != ProcessGroupExitUnknown {
		t.Fatalf("legacy process-group inspection = %#v, want unknown", inspection)
	}
}
