//go:build linux

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
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
)

func init() {
	if os.Getenv("GO_CLI_REAPER_TEST_HELPER") != "signal" || os.Getenv(subreaperPrivatePrefix+"ROLE") != "inner" {
		return
	}
	// Go runs package initialization on its startup thread. Observe the
	// exec-created leader's attribute before the test goroutine can migrate.
	var configured int32
	err := unix.Prctl(unix.PR_GET_PDEATHSIG, uintptr(unsafe.Pointer(&configured)), 0, 0, 0)
	runtime.KeepAlive(&configured)
	data := fmt.Sprintf("pid=%d tid=%d pdeath=%d error=%v", os.Getpid(), unix.Gettid(), configured, err)
	_ = os.WriteFile(os.Getenv("GO_CLI_REAPER_TEST_PID")+".leader-bootstrap", []byte(data), 0o600)
}

// Each helper is a distinct executable process. The Go test host never enables
// the process-global subreaper attribute or consumes another test's children.
func TestStandaloneSubreaperLinuxHelper(t *testing.T) {
	mode := os.Getenv("GO_CLI_REAPER_TEST_HELPER")
	if mode == "" {
		return
	}
	if mode == "duplicate-private-start" {
		_ = os.Setenv("GO_CLI_REAPER_TEST_HELPER", "duplicate-private-check")
		environment := append(stripSubreaperPrivateEnvironment(os.Environ()), subreaperPrivatePrefix+"ROLE=inner", subreaperPrivatePrefix+"ROLE=inner",
			subreaperPrivatePrefix+"NONCE=fake", subreaperPrivatePrefix+"PARENT=7:12", subreaperPrivatePrefix+"EXE=3:4")
		_ = unix.Exec("/proc/self/exe", os.Args, environment)
		os.Exit(100)
	}
	if strings.HasPrefix(mode, "deny-") {
		target := uint32(unix.SYS_PIDFD_OPEN)
		if mode == "deny-prctl" {
			target = uint32(unix.SYS_PRCTL)
		}
		filters := []unix.SockFilter{
			{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
			{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: target, Jf: 1},
			{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
			{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
		}
		if mode == "deny-waitid-probe" {
			// Deny only the non-consuming probe, so Cmd.Wait retains its
			// original consuming wait syscall and can still join the inner.
			filters = []unix.SockFilter{
				{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
				{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: uint32(unix.SYS_WAITID), Jf: 3},
				{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 40},
				{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: unix.WNOWAIT, Jf: 1},
				{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
				{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
			}
		}
		program := unix.SockFprog{Len: uint16(len(filters)), Filter: &filters[0]}
		if unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != nil || unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&program)), 0, 0) != nil {
			os.Exit(99)
		}
		runtime.KeepAlive(&program)
	}
	if strings.HasPrefix(mode, "preexisting-start-") {
		command := exec.Command("/bin/sh", "-c", "sleep 0.3; exit 17")
		if command.Start() != nil {
			os.Exit(91)
		}
		if mode == "preexisting-start-zombie" {
			for deadline := time.Now().Add(5 * time.Second); ; {
				identity, err := readSubreaperIdentity(command.Process.Pid)
				if err == nil && identity.state == 'Z' {
					break
				}
				if time.Now().After(deadline) {
					_ = command.Process.Kill()
					_ = command.Wait()
					os.Exit(92)
				}
				time.Sleep(time.Millisecond)
			}
		}
		_ = os.Setenv("GO_CLI_REAPER_TEST_HELPER", "preexisting-check")
		_ = os.Setenv("GO_CLI_REAPER_TEST_CHILD", strconv.Itoa(command.Process.Pid))
		if err := unix.Exec("/proc/self/exe", os.Args, os.Environ()); err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			os.Exit(93)
		}
	}
	if code, handled := standaloneSubreaperDispatch(); handled {
		if mode == "preexisting-check" {
			pid, _ := strconv.Atoi(os.Getenv("GO_CLI_REAPER_TEST_CHILD"))
			var status unix.WaitStatus
			waited, err := unix.Wait4(pid, &status, 0, nil) // exact original owner, test only
			if err != nil || waited != pid || !status.Exited() || status.ExitStatus() != 17 {
				os.Exit(94)
			}
		}
		os.Exit(code)
	}
	switch mode {
	case "exit":
		os.Exit(37)
	case "die-signal":
		_ = unix.Kill(os.Getpid(), unix.SIGKILL)
		os.Exit(95)
	case "signal":
		signals := make(chan os.Signal, 4)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
		var parentDeath int32
		parentDeathErr := unix.Prctl(unix.PR_GET_PDEATHSIG, uintptr(unsafe.Pointer(&parentDeath)), 0, 0, 0)
		runtime.KeepAlive(&parentDeath)
		_ = os.WriteFile(os.Getenv("GO_CLI_REAPER_TEST_PID")+".bootstrap", []byte(fmt.Sprintf("pid=%d ppid=%d tid=%d pdeath=%d error=%v", os.Getpid(), os.Getppid(), unix.Gettid(), parentDeath, parentDeathErr)), 0o600)
		if os.WriteFile(os.Getenv("GO_CLI_REAPER_TEST_PID"), []byte(strconv.Itoa(os.Getpid())), 0o600) != nil {
			os.Exit(96)
		}
		received := <-signals
		count := 1
		_ = os.WriteFile(os.Getenv("GO_CLI_REAPER_TEST_PID")+".first-signal", []byte(fmt.Sprintf("signal=%d time=%d pid=%d ppid=%d tid=%d", int(received.(syscall.Signal)), time.Now().UnixNano(), os.Getpid(), os.Getppid(), unix.Gettid())), 0o600)
		_ = os.WriteFile(os.Getenv("GO_CLI_REAPER_TEST_PID")+".received", []byte(strconv.Itoa(count)), 0o600)
		timer := time.NewTimer(100 * time.Millisecond)
		for {
			select {
			case <-signals:
				count++
				_ = os.WriteFile(os.Getenv("GO_CLI_REAPER_TEST_PID")+".received", []byte(strconv.Itoa(count)), 0o600)
			case <-timer.C:
				_ = os.WriteFile(os.Getenv("GO_CLI_REAPER_TEST_PID")+".signals", []byte(strconv.Itoa(count)), 0o600)
				os.Exit(128 + int(received.(syscall.Signal)))
			}
		}
	case "ordinary-waits":
		one := exec.Command("/bin/sh", "-c", "exit 11")
		two := exec.Command("/bin/sh", "-c", "exit 23")
		if one.Start() != nil || two.Start() != nil {
			os.Exit(81)
		}
		first, second := one.Wait(), two.Wait()
		var firstExit, secondExit *exec.ExitError
		if !errors.As(first, &firstExit) || firstExit.ExitCode() != 11 || !errors.As(second, &secondExit) || secondExit.ExitCode() != 23 {
			os.Exit(82)
		}
		os.Exit(39)
	case "environment":
		for _, item := range os.Environ() {
			if strings.HasPrefix(item, subreaperPrivatePrefix) {
				os.Exit(83)
			}
		}
		if _, err := os.Readlink("/proc/self/fd/3"); !errors.Is(err, os.ErrNotExist) {
			os.Exit(84)
		}
		data, err := os.ReadFile("/proc/self/status")
		if err != nil || !strings.Contains(string(data), "PPid:") {
			os.Exit(85)
		}
		_, _ = fmt.Fprint(os.Stdout, "public stdout\n")
		_, _ = fmt.Fprint(os.Stderr, "public stderr\n")
		os.Exit(0)
	case "no-outer":
		if strconv.Itoa(os.Getppid()) != os.Getenv("GO_CLI_REAPER_TEST_PARENT") {
			os.Exit(97)
		}
		os.Exit(0)
	case "orphan":
		command := exec.Command("/bin/sh", "-c", `/bin/sh -c 'echo $$ > "$GO_CLI_REAPER_TEST_PID"; exec sleep 0.1' </dev/null >/dev/null 2>&1 & while [ ! -s "$GO_CLI_REAPER_TEST_PID" ]; do sleep 0.005; done`)
		if err := command.Run(); err != nil {
			os.Exit(86)
		}
		// Inner still runs while the orphan exits; only the outer may reap it.
		time.Sleep(300 * time.Millisecond)
		data, err := os.ReadFile(os.Getenv("GO_CLI_REAPER_TEST_PID"))
		if err != nil {
			os.Exit(87)
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			os.Exit(88)
		}
		os.Exit(0)
	case "live-orphan", "live-orphan-killed":
		command := exec.Command("/bin/sh", "-c", `/bin/sh -c 'echo $$ > "$GO_CLI_REAPER_TEST_PID"; exec sleep 60' </dev/null >/dev/null 2>&1 & while [ ! -s "$GO_CLI_REAPER_TEST_PID" ]; do sleep 0.005; done`)
		if command.Run() != nil {
			os.Exit(89)
		}
		if mode == "live-orphan-killed" {
			_ = unix.Kill(os.Getpid(), unix.SIGKILL)
			os.Exit(98)
		}
		os.Exit(0)
	default:
		os.Exit(90)
	}
}

func TestStandaloneSubreaperLinuxExitAndWaitOwnership(t *testing.T) {
	for _, scenario := range []struct {
		name string
		code int
	}{
		{"exit", 37}, {"die-signal", 137}, {"ordinary-waits", 39}, {"environment", 0}, {"orphan", 0}, {"live-orphan", 125}, {"live-orphan-killed", 125},
		{"preexisting-start-live", 125}, {"preexisting-start-zombie", 125},
		{"deny-pidfd", 125}, {"deny-prctl", 125}, {"deny-waitid-probe", 125},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestStandaloneSubreaperLinuxHelper$")
			pidFile := filepath.Join(t.TempDir(), "pid")
			command.Env = append(stripSubreaperPrivateEnvironment(os.Environ()), subreaperEnvironment+"=1", "GO_CLI_REAPER_TEST_HELPER="+scenario.name, "GO_CLI_REAPER_TEST_PID="+pidFile)
			// Go's exec pipe descriptors are CLOEXEC in the supervisor itself.
			output, err := command.CombinedOutput()
			code := 0
			if err != nil {
				var exitError *exec.ExitError
				require.ErrorAs(t, err, &exitError, "%s", output)
				code = exitError.ExitCode()
			}
			require.Equal(t, scenario.code, code, "%s", output)
			if strings.HasPrefix(scenario.name, "live-orphan") {
				data, readErr := os.ReadFile(pidFile)
				require.NoError(t, readErr)
				pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
				require.NoError(t, parseErr)
				require.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH)
			}
		})
	}
}

func TestStandaloneSubreaperLinuxForwardsSignalsOnce(t *testing.T) {
	for _, sent := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sent.String(), func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "ready")
			command := exec.Command(os.Args[0], "-test.run=^TestStandaloneSubreaperLinuxHelper$")
			command.Env = append(stripSubreaperPrivateEnvironment(os.Environ()), subreaperEnvironment+"=1", "GO_CLI_REAPER_TEST_HELPER=signal", "GO_CLI_REAPER_TEST_PID="+file)
			require.NoError(t, command.Start())
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			joined := false
			t.Cleanup(func() {
				if !joined {
					_ = command.Process.Kill()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("owned supervisor did not join after cleanup")
					}
				}
			})
			require.Eventually(t, func() bool { _, err := os.Stat(file); return err == nil }, 5*time.Second, 5*time.Millisecond)
			require.NoError(t, command.Process.Signal(sent))
			select {
			case err := <-done:
				joined = true
				var exitError *exec.ExitError
				require.ErrorAs(t, err, &exitError)
				require.Equal(t, 128+int(sent), exitError.ExitCode())
			case <-time.After(5 * time.Second):
				t.Fatal("signal forwarding did not join actual CLI")
			}
			data, err := os.ReadFile(file + ".signals")
			require.NoError(t, err)
			require.Equal(t, "1", string(data))
		})
	}
}

func TestStandaloneSubreaperLinuxParentDeathCancelsInner(t *testing.T) {
	file := filepath.Join(t.TempDir(), "ready")
	command := exec.Command(os.Args[0], "-test.run=^TestStandaloneSubreaperLinuxHelper$")
	command.Env = append(stripSubreaperPrivateEnvironment(os.Environ()), subreaperEnvironment+"=1", "GO_CLI_REAPER_TEST_HELPER=signal", "GO_CLI_REAPER_TEST_PID="+file)
	require.NoError(t, command.Start())
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	joined := false
	childFD := -1
	t.Cleanup(func() {
		if childFD >= 0 {
			_ = unix.PidfdSendSignal(childFD, unix.SIGKILL, nil, 0)
			_ = unix.Close(childFD)
		}
		if !joined {
			_ = command.Process.Kill()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("owned supervisor did not join after cleanup")
			}
		}
	})
	require.Eventually(t, func() bool { _, err := os.Stat(file); return err == nil }, 5*time.Second, 5*time.Millisecond)
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	childPID, err := strconv.Atoi(string(data))
	require.NoError(t, err)
	identity, err := readSubreaperIdentity(childPID)
	require.NoError(t, err)
	require.Equal(t, command.Process.Pid, identity.parent)
	t.Logf("before outer death: outer_pid=%d actual_identity=%+v", command.Process.Pid, identity)
	bootstrap, err := os.ReadFile(file + ".bootstrap")
	require.NoError(t, err)
	t.Logf("inner bootstrap: %s", bootstrap)
	leaderBootstrap, err := os.ReadFile(file + ".leader-bootstrap")
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("pid=%d tid=%d pdeath=15 error=<nil>", childPID, childPID), string(leaderBootstrap))
	t.Logf("exec leader bootstrap: %s", leaderBootstrap)
	childFD, err = unix.PidfdOpen(childPID, 0)
	require.NoError(t, err)
	require.NoError(t, command.Process.Kill())
	select {
	case err := <-done:
		joined = true
		var exitError *exec.ExitError
		require.ErrorAs(t, err, &exitError)
		require.Equal(t, syscall.SIGKILL, exitError.Sys().(syscall.WaitStatus).Signal())
	case <-time.After(5 * time.Second):
		t.Fatal("killed outer did not join")
	}
	var signalValue []byte
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		signalValue, err = os.ReadFile(file + ".signals")
		if err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	current, identityErr := readSubreaperIdentity(childPID)
	receivedValue, receivedErr := os.ReadFile(file + ".received")
	firstSignal, firstSignalErr := os.ReadFile(file + ".first-signal")
	t.Logf("after outer death: inner=%+v identity_error=%v received=%q received_error=%v finalized=%q finalized_error=%v", current, identityErr, receivedValue, receivedErr, signalValue, err)
	t.Logf("first observed cancellation signal (sender not exported): %s error=%v; test cleanup has not yet run", firstSignal, firstSignalErr)
	require.NoError(t, firstSignalErr)
	require.True(t, strings.HasPrefix(string(firstSignal), "signal=15 "), "the actual inner received SIGTERM before test cleanup")
	require.NoError(t, err, "parent-death cancellation handler must finish")
	count, err := strconv.Atoi(string(signalValue))
	require.NoError(t, err)
	require.GreaterOrEqual(t, count, 1, "parent-death cancellation is required; unlike explicit forwarding, the kernel may deliver more than once")
	require.Eventually(t, func() bool {
		current, err := readSubreaperIdentity(childPID)
		if errors.Is(err, os.ErrNotExist) || (err == nil && !sameSubreaperIdentity(identity, current)) {
			return true
		}
		if err == nil && current.parent == os.Getpid() && current.state == 'Z' {
			// When this test is namespace PID1, the dead outer's known inner
			// becomes our child. Consume only that bound identity, not wait(-1).
			var info unix.Siginfo
			_ = unix.Waitid(unix.P_PIDFD, childFD, &info, unix.WEXITED|unix.WNOHANG, nil)
		}
		return false
	}, 5*time.Second, 5*time.Millisecond)
}

func TestStandaloneSubreaperLinuxChangedIdentityNeverWaited(t *testing.T) {
	expected := subreaperProcessIdentity{pid: 23, parent: 7, state: 'Z', started: 11}
	closed := false
	operations := subreaperChildOperations{
		read: func(int) (subreaperProcessIdentity, error) {
			current := expected
			current.started++
			return current, nil
		},
		open:  func(int) (int, error) { return 99, nil },
		close: func(fd int) error { require.Equal(t, 99, fd); closed = true; return nil },
		wait:  func(int) error { t.Fatal("changed identity must not be waited"); return nil },
	}
	err := reapSubreaperZombieWithOperations(expected, 7, operations)
	require.Error(t, err)
	require.True(t, closed)
}

func TestStandaloneSubreaperLinuxWaitZeroDoesNotProveAbsence(t *testing.T) {
	expected := subreaperProcessIdentity{pid: 23, parent: 7, state: 'Z', started: 11}
	for _, waitErr := range []error{nil, unix.ECHILD, unix.ESRCH} {
		operations := subreaperChildOperations{
			read:  func(int) (subreaperProcessIdentity, error) { return expected, nil },
			open:  func(int) (int, error) { return 99, nil },
			close: func(int) error { return nil },
			wait:  func(int) error { return waitErr },
		}
		require.Error(t, reapSubreaperZombieWithOperations(expected, 7, operations))
	}
}

func TestStandaloneSubreaperLinuxActualDescriptorHasNoNumericFallback(t *testing.T) {
	missing := -1
	command := &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}, SysProcAttr: &syscall.SysProcAttr{PidFD: &missing}}
	_, err := subreaperActualDescriptor(command)
	require.Error(t, err, "a valid numeric PID cannot replace the missing fork descriptor")
	atomicFD := -1
	actual := exec.Command("/bin/sh", "-c", "exit 23")
	actual.SysProcAttr = &syscall.SysProcAttr{PidFD: &atomicFD}
	require.NoError(t, actual.Start())
	require.GreaterOrEqual(t, atomicFD, 0)
	t.Cleanup(func() { _ = unix.Close(atomicFD) })
	var exitError *exec.ExitError
	require.ErrorAs(t, actual.Wait(), &exitError)
	require.Equal(t, 23, exitError.ExitCode())
	// Inject a replaced numeric metadata PID after the actual child was joined.
	// This is a fault injection, not a claim of observing a real PID wrap.
	actual.Process = &os.Process{Pid: os.Getpid()}
	fd, err := subreaperActualDescriptor(actual)
	require.NoError(t, err)
	require.Equal(t, atomicFD, fd)
	var info unix.Siginfo
	require.ErrorIs(t, unix.Waitid(unix.P_PIDFD, fd, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil), unix.ECHILD)
}

func TestStandaloneSubreaperLinuxBootstrapFailureNeverSignalsUntrustedFD(t *testing.T) {
	file := filepath.Join(t.TempDir(), "sentinel-ready")
	sentinel := exec.Command(os.Args[0], "-test.run=^TestStandaloneSubreaperLinuxHelper$")
	sentinel.Env = append(stripSubreaperPrivateEnvironment(os.Environ()), subreaperEnvironment+"=0", "GO_CLI_REAPER_TEST_HELPER=signal", "GO_CLI_REAPER_TEST_PID="+file)
	require.NoError(t, sentinel.Start())
	t.Cleanup(func() { _ = sentinel.Process.Kill(); _ = sentinel.Wait() })
	require.Eventually(t, func() bool { _, err := os.Stat(file); return err == nil }, 5*time.Second, 5*time.Millisecond)
	for _, cause := range []error{unix.ECHILD, unix.EPERM, errors.New("identity replaced"), errors.New("early exit")} {
		parent, peer := net.Pipe()
		done := make(chan error, 1)
		done <- cause
		err := failSubreaperBootstrap(parent, done, cause)
		require.ErrorIs(t, err, cause)
		require.NoError(t, peer.Close())
	}
	require.Never(t, func() bool { _, err := os.Stat(file + ".received"); return err == nil }, 200*time.Millisecond, 5*time.Millisecond,
		"a replacement/untrusted descriptor must receive no abort signal")
	require.NoError(t, sentinel.Process.Signal(syscall.Signal(0)))
}

func TestStandaloneSubreaperLinuxAllThreadChildrenInventory(t *testing.T) {
	root := t.TempDir()
	for _, row := range []struct{ thread, children string }{{"7", ""}, {"8", "23 41"}} {
		directory := filepath.Join(root, "self", "task", row.thread)
		require.NoError(t, os.MkdirAll(directory, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(directory, "children"), []byte(row.children), 0o600))
	}
	children, err := readSubreaperChildren(root)
	require.NoError(t, err)
	require.ElementsMatch(t, []int{23, 41}, children)
	require.NoError(t, os.Remove(filepath.Join(root, "self", "task", "8", "children")))
	_, err = readSubreaperChildren(root)
	require.Error(t, err, "an unobservable thread cannot be assumed childless")
}

func TestStandaloneSubreaperLinuxRejectsPrivateFileAndInheritedFD(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "not-a-socket")
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })
	for _, private := range []bool{false, true} {
		command := exec.Command(os.Args[0], "-test.run=^TestStandaloneSubreaperLinuxHelper$")
		command.Env = append(stripSubreaperPrivateEnvironment(os.Environ()), subreaperEnvironment+"=1", "GO_CLI_REAPER_TEST_HELPER=exit")
		if private {
			command.Env = append(command.Env, subreaperPrivatePrefix+"ROLE=inner", subreaperPrivatePrefix+"NONCE=invalid", subreaperPrivatePrefix+"PARENT=7:12", subreaperPrivatePrefix+"EXE=3:4")
		}
		command.ExtraFiles = []*os.File{file}
		output, err := command.CombinedOutput()
		var exitError *exec.ExitError
		require.ErrorAs(t, err, &exitError, "%s", output)
		require.Equal(t, 125, exitError.ExitCode(), "%s", output)
	}
}

func TestStandaloneSubreaperLinuxRejectsEachTTY(t *testing.T) {
	_, slave, closePTY := openTrustPTY(t)
	t.Cleanup(closePTY)
	for _, descriptor := range []string{"stdin", "stdout", "stderr"} {
		t.Run(descriptor, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestStandaloneSubreaperLinuxHelper$")
			command.Env = append(stripSubreaperPrivateEnvironment(os.Environ()), subreaperEnvironment+"=1", "GO_CLI_REAPER_TEST_HELPER=exit")
			switch descriptor {
			case "stdin":
				command.Stdin = slave
			case "stdout":
				command.Stdout = slave
			case "stderr":
				command.Stderr = slave
			}
			err := command.Run()
			var exitError *exec.ExitError
			require.ErrorAs(t, err, &exitError)
			require.Equal(t, 125, exitError.ExitCode())
		})
	}
}

func TestStandaloneSubreaperLinuxDefaultOff(t *testing.T) {
	for _, value := range []string{"", "0"} {
		command := exec.Command(os.Args[0], "-test.run=^TestStandaloneSubreaperLinuxHelper$")
		command.Env = append(stripSubreaperPrivateEnvironment(os.Environ()), subreaperEnvironment+"="+value, "GO_CLI_REAPER_TEST_HELPER=no-outer", "GO_CLI_REAPER_TEST_PARENT="+strconv.Itoa(os.Getpid()))
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
}

func TestStandaloneSubreaperLinuxRejectsRawDuplicatePrivateEnvironment(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestStandaloneSubreaperLinuxHelper$")
	command.Env = append(stripSubreaperPrivateEnvironment(os.Environ()), subreaperEnvironment+"=1", "GO_CLI_REAPER_TEST_HELPER=duplicate-private-start")
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	require.ErrorAs(t, err, &exitError, "%s", output)
	require.Equal(t, 125, exitError.ExitCode())
	require.Contains(t, string(output), "duplicate", "raw exec duplicates must be rejected before merely failing the missing private FD")
}

func TestStandaloneSubreaperLinuxRejectsInvalidBootstrap(t *testing.T) {
	parent, err := readSubreaperIdentity(os.Getpid())
	require.NoError(t, err)
	executable, err := subreaperExecutableIdentity("/proc/self/exe")
	require.NoError(t, err)
	for _, scenario := range []string{"wrong-nonce", "wrong-phase", "wrong-parent", "wrong-executable", "duplicate-challenge", "eof"} {
		t.Run(scenario, func(t *testing.T) {
			sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
			require.NoError(t, err)
			outer, inner := os.NewFile(uintptr(sockets[0]), "test-bootstrap-parent"), os.NewFile(uintptr(sockets[1]), "test-bootstrap-child")
			t.Cleanup(func() { _ = outer.Close(); _ = inner.Close() })
			parentValue := fmt.Sprintf("%d:%d", parent.pid, parent.started)
			executableValue := executable
			if scenario == "wrong-parent" {
				parentValue = "7:12"
			}
			if scenario == "wrong-executable" {
				executableValue = "3:4"
			}
			command := exec.Command(os.Args[0], "-test.run=^TestStandaloneSubreaperLinuxHelper$")
			command.Env = append(stripSubreaperPrivateEnvironment(os.Environ()), subreaperEnvironment+"=1", "GO_CLI_REAPER_TEST_HELPER=exit",
				subreaperPrivatePrefix+"ROLE=inner", subreaperPrivatePrefix+"NONCE=correct", subreaperPrivatePrefix+"PARENT="+parentValue, subreaperPrivatePrefix+"EXE="+executableValue)
			command.ExtraFiles = []*os.File{inner}
			packet := fmt.Sprintf(`{"phase":"challenge","nonce":"correct","pid":%d}`, os.Getpid())
			if scenario == "wrong-nonce" {
				packet = strings.Replace(packet, "correct", "wrong", 1)
			}
			if scenario == "wrong-phase" {
				packet = strings.Replace(packet, "challenge", "release", 1)
			}
			if scenario == "eof" {
				require.NoError(t, outer.Close())
			} else {
				_, err = outer.Write([]byte(packet))
				require.NoError(t, err)
				if scenario == "duplicate-challenge" {
					_, err = outer.Write([]byte(packet))
					require.NoError(t, err)
				}
			}
			output, err := command.CombinedOutput()
			var exitError *exec.ExitError
			require.ErrorAs(t, err, &exitError, "%s", output)
			require.Equal(t, 125, exitError.ExitCode(), "%s", output)
		})
	}
}
