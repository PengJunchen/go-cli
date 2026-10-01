package acp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStdioProcessOwnerRejectsUnsupportedContainmentBeforeStart(t *testing.T) {
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		t.Skip("process-group containment is supported on this operating system")
	}

	marker := filepath.Join(t.TempDir(), "must-not-start")
	owner, err := NewStdioProcessOwner(StdioProcessConfig{
		Command: "/bin/sh",
		Args:    []string{"-c", "touch " + marker},
	})
	if !errors.Is(err, ErrStdioProcessContainmentUnsupported) {
		t.Fatalf("constructor error = %v, want %v", err, ErrStdioProcessContainmentUnsupported)
	}
	if owner != nil {
		t.Fatal("unsupported containment returned a process owner")
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unsupported constructor created the child marker: stat error = %v", statErr)
	}
}

func TestStdioProcessOwnerEarlyExitIsWaitedAndReaped(t *testing.T) {
	requireStdioProcessGroups(t)
	var owner *StdioProcessOwner
	var cleanupCalls atomic.Int32
	owner, err := NewStdioProcessOwner(StdioProcessConfig{
		Command: "/bin/sh",
		Args:    []string{"-c", "printf '%s\\n' early-exit-ready; exit 23", "stdio-owner-helper"},
		Cleanup: func() error {
			if !stdioOwnerChannelClosed(owner.waitDone) {
				return errors.New("cleanup ran before the direct child was waited")
			}
			owner.mu.Lock()
			groupGone := owner.groupGone
			owner.mu.Unlock()
			if !groupGone || owner.cmd.ProcessState == nil {
				return errors.New("cleanup ran before process-group confirmation and reaping")
			}
			cleanupCalls.Add(1)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	defer stdioOwnerCleanup(t, owner)
	if err := owner.Start(); err != nil {
		t.Fatalf("start early-exit helper: %v", err)
	}

	readResult := make(chan struct {
		data string
		err  error
	}, 1)
	go func() {
		data, readErr := io.ReadAll(owner.Reader())
		readResult <- struct {
			data string
			err  error
		}{data: string(data), err: readErr}
	}()
	select {
	case result := <-readResult:
		if result.err != nil {
			t.Fatalf("read early-exit helper stdout: %v", result.err)
		}
		if !strings.Contains(result.data, "early-exit-ready") {
			t.Fatalf("early-exit helper stdout = %q", result.data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("early-exit helper did not close stdout")
	}
	if err := stdioOwnerCallWithin("Close after early exit", owner.Close); err != nil {
		t.Fatalf("Close after early exit: %v", err)
	}
	joinErr := stdioOwnerCallWithin("Join after early exit", owner.Join)
	if joinErr == nil || !strings.Contains(joinErr.Error(), "exit status 23") {
		t.Fatalf("Join error = %v, want the helper's exit status 23", joinErr)
	}
	if owner.cmd.ProcessState == nil || owner.cmd.ProcessState.ExitCode() != 23 {
		t.Fatalf("direct child process state = %#v, want exit code 23", owner.cmd.ProcessState)
	}
	if got := cleanupCalls.Load(); got != 1 {
		t.Fatalf("cleanup callback count = %d, want 1", got)
	}
	stdioOwnerRequireJoinedState(t, owner)
}

func TestStdioProcessOwnerStartFailureDefersCleanupUntilJoin(t *testing.T) {
	requireStdioProcessGroups(t)
	cleanupPath := filepath.Join(t.TempDir(), "private-executable-copy")
	if err := os.WriteFile(cleanupPath, []byte("private copy"), 0o600); err != nil {
		t.Fatalf("create cleanup fixture: %v", err)
	}
	var cleanupCalls atomic.Int32
	owner, err := NewStdioProcessOwner(StdioProcessConfig{
		Command:        "missing-stdio-owner-executable",
		ExecutablePath: filepath.Join(t.TempDir(), "missing-executable"),
		Cleanup: func() error {
			cleanupCalls.Add(1)
			removeErr := os.Remove(cleanupPath)
			if errors.Is(removeErr, os.ErrNotExist) {
				return nil
			}
			return removeErr
		},
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	defer stdioOwnerCleanup(t, owner)
	startErr := owner.Start()
	if startErr == nil {
		t.Fatal("Start unexpectedly succeeded with a missing executable")
	}
	closeErr := stdioOwnerCallWithin("Close after start failure", owner.Close)
	if closeErr != nil {
		t.Fatalf("Close after an unstarted child failed cleanup: %v", closeErr)
	}
	if got := cleanupCalls.Load(); got != 0 {
		t.Errorf("cleanup ran %d time(s) during Close before Join", got)
	}
	if _, statErr := os.Stat(cleanupPath); statErr != nil {
		t.Errorf("private cleanup fixture disappeared before Join: %v", statErr)
	}

	joinErr := stdioOwnerCallWithin("Join after start failure", owner.Join)
	if joinErr != nil {
		t.Errorf("Join after an unstarted child failed cleanup: %v", joinErr)
	}
	if got := cleanupCalls.Load(); got != 1 {
		t.Errorf("cleanup callback count after Join = %d, want 1", got)
	}
	if _, statErr := os.Stat(cleanupPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("private cleanup fixture remains after Join: stat error = %v", statErr)
	}
	owner.mu.Lock()
	groupGone := owner.groupGone
	owner.mu.Unlock()
	if !groupGone {
		t.Error("start-failure Join returned before the no-process group state was confirmed")
	}
}

func TestStdioProcessOwnerContextCancellationStopsAndJoinsChild(t *testing.T) {
	requireStdioProcessGroups(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner, err := NewStdioProcessOwner(StdioProcessConfig{
		Context: ctx,
		Command: "/bin/sh",
		Args:    []string{"-c", "printf '%s\\n' block-ready; exec sleep 60", "stdio-owner-helper"},
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	defer stdioOwnerCleanup(t, owner)
	if err := owner.Start(); err != nil {
		t.Fatalf("start blocking helper: %v", err)
	}
	if err := stdioOwnerWaitForOutput(owner.Reader(), "block-ready\n", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	cancel()
	if !stdioOwnerWaitChannel(owner.shutdownDone, 5*time.Second) {
		t.Fatal("context cancellation did not complete bounded process-group shutdown")
	}
	joinErr := stdioOwnerCallWithin("Join after context cancellation", owner.Join)
	if joinErr != nil && strings.Contains(joinErr.Error(), "Wait was already called") {
		t.Fatalf("Join exposed a duplicate Wait: %v", joinErr)
	}
	stdioOwnerRequireJoinedState(t, owner)
}

func TestStdioAdapterConnectCancellationStillJoinsOwnedProcess(t *testing.T) {
	requireStdioProcessGroups(t)

	dir := t.TempDir()
	readyPath := filepath.Join(dir, "helper-ready")
	executablePath := stdioOwnerWriteScript(t, dir, "materialized-acp-helper", `
: > "$1"
while :; do sleep 30; done
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var owner *StdioProcessOwner
	var cleanupCalls atomic.Int32
	owner, err := NewStdioProcessOwner(StdioProcessConfig{
		Context:        ctx,
		Command:        "verified-acp-helper",
		ExecutablePath: executablePath,
		Args:           []string{readyPath},
		Cleanup: func() error {
			if !stdioOwnerChannelClosed(owner.waitDone) {
				return errors.New("materialized executable cleanup ran before the direct child was waited")
			}
			owner.mu.Lock()
			groupGone := owner.groupGone
			owner.mu.Unlock()
			if !groupGone || owner.cmd.ProcessState == nil {
				return errors.New("materialized executable cleanup ran before process cleanup joined")
			}
			cleanupCalls.Add(1)
			removeErr := os.Remove(executablePath)
			if errors.Is(removeErr, os.ErrNotExist) {
				return nil
			}
			return removeErr
		},
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	defer stdioOwnerCleanup(t, owner)
	if err := owner.Start(); err != nil {
		t.Fatalf("start materialized helper: %v", err)
	}
	if err := stdioOwnerWaitForFile(readyPath, 5*time.Second); err != nil {
		t.Fatal(err)
	}

	client := NewStdioAdapter(owner.Reader(), owner.Writer(), WithStdioOwnedTransportCloser(owner)).(*StdioAdapter)
	if _, err := os.Stat(executablePath); err != nil {
		t.Fatalf("materialized executable disappeared before Connect: %v", err)
	}
	cancel()
	if err := client.Connect(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Connect error = %v, want context cancellation before dispatch", err)
	}
	if !stdioOwnerWaitChannel(owner.shutdownDone, 5*time.Second) {
		t.Fatal("owner context cancellation did not complete bounded process-group shutdown")
	}

	if err := stdioOwnerCallWithin("Disconnect after canceled owned Connect", func() error {
		return client.Disconnect(context.Background())
	}); err != nil {
		t.Fatalf("Disconnect after canceled owned Connect: %v", err)
	}
	if got := cleanupCalls.Load(); got != 1 {
		t.Fatalf("materialized executable cleanup count = %d, want 1 after adapter cleanup", got)
	}
	if _, err := os.Stat(executablePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("materialized executable still exists after adapter cleanup: %v", err)
	}
	client.mu.Lock()
	session := client.session
	cleanupDone := client.cleanupDone
	closed := client.closed
	client.mu.Unlock()
	if session == nil || !closed || !stdioOwnerChannelClosed(cleanupDone) {
		t.Fatal("adapter detached its owned session or returned before the retained cleanup completed")
	}
	if !stdioOwnerChannelClosed(session.joined) || session.workers != 0 {
		t.Fatalf("stdio workers did not join: workers=%d joined=%v", session.workers, stdioOwnerChannelClosed(session.joined))
	}
	if err := stdioOwnerCallWithin("Join after canceled owned Connect", owner.Join); err != nil {
		t.Fatalf("owner Join after canceled owned Connect: %v", err)
	}
	stdioOwnerRequireJoinedState(t, owner)
	if _, err := owner.stdout.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("owned stdout fd remains usable after adapter join: %v", err)
	}
}

func TestStdioProcessOwnerJoinsSameGroupDescendantsBeforeSuccess(t *testing.T) {
	requireStdioProcessGroups(t)
	dir := t.TempDir()
	paths := []string{
		filepath.Join(dir, "child-ready"),
		filepath.Join(dir, "grandchild-ready"),
		filepath.Join(dir, "leader-ready"),
		filepath.Join(dir, "leader-term"),
		filepath.Join(dir, "child-term"),
		filepath.Join(dir, "child-reaped"),
		filepath.Join(dir, "grandchild-term"),
		filepath.Join(dir, "leader-reaped"),
		filepath.Join(dir, "leader-trap-ready"),
		filepath.Join(dir, "child-trap-ready"),
		filepath.Join(dir, "grandchild-trap-ready"),
	}
	grandchildScript := stdioOwnerWriteScript(t, dir, "grandchild.sh", `
trap ': > "$2"; exit 0' TERM
: > "$3"
: > "$1"
printf '%s\n' grandchild-ready
while :; do :; done
`)
	childScript := stdioOwnerWriteScript(t, dir, "child.sh", `
child_ready=$1
grandchild_ready=$2
child_term=$3
child_reaped=$4
grandchild_term=$5
grandchild_script=$6
grandchild_trap_ready=$7
child_trap_ready=$8
trap ': > "$child_term"; wait "$grandchild" || :; : > "$child_reaped"' TERM
: > "$child_trap_ready"
/bin/sh "$grandchild_script" "$grandchild_ready" "$grandchild_term" "$grandchild_trap_ready" &
grandchild=$!
: > "$child_ready"
printf '%s\n' child-ready
wait "$grandchild" || :
while [ ! -e "$grandchild_term" ]; do :; done
wait "$grandchild" || :
: > "$child_reaped"
`)
	leaderScript := stdioOwnerWriteScript(t, dir, "leader.sh", `
child_ready=$1
grandchild_ready=$2
leader_ready=$3
leader_term=$4
child_term=$5
child_reaped=$6
grandchild_term=$7
leader_reaped=$8
child_script=$9
grandchild_script=${10}
leader_trap_ready=${11}
child_trap_ready=${12}
grandchild_trap_ready=${13}
trap ': > "$leader_term"' TERM
: > "$leader_trap_ready"
/bin/sh "$child_script" "$child_ready" "$grandchild_ready" "$child_term" "$child_reaped" "$grandchild_term" "$grandchild_script" "$child_trap_ready" "$grandchild_trap_ready" &
child=$!
: > "$leader_ready"
printf '%s\n' leader-ready
wait "$child" || :
wait "$child" || :
: > "$leader_reaped"
while :; do sleep 30; done
`)
	var cleanupCalls atomic.Int32
	var owner *StdioProcessOwner
	var err error
	owner, err = NewStdioProcessOwner(StdioProcessConfig{
		Command: leaderScript,
		Args:    []string{paths[0], paths[1], paths[2], paths[3], paths[4], paths[5], paths[6], paths[7], childScript, grandchildScript, paths[8], paths[9], paths[10]},
		Cleanup: func() error {
			cleanupCalls.Add(1)
			if !stdioOwnerChannelClosed(owner.waitDone) {
				return errors.New("cleanup ran before the direct child was waited")
			}
			owner.mu.Lock()
			groupGone := owner.groupGone
			owner.mu.Unlock()
			if !groupGone {
				return errors.New("cleanup ran before process-group confirmation")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	defer stdioOwnerCleanup(t, owner)
	if err := owner.Start(); err != nil {
		t.Fatalf("start process-tree leader: %v", err)
	}
	readerDone := make(chan error, 1)
	go func() {
		_, readErr := io.Copy(io.Discard, owner.Reader())
		readerDone <- readErr
	}()
	for _, path := range append(paths[:3], paths[8:]...) {
		if err := stdioOwnerWaitForFile(path, 5*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	owner.mu.Lock()
	groupID := owner.groupID
	owner.mu.Unlock()
	alive, err := acpStdioProcessGroupAlive(groupID)
	if err != nil || !alive {
		t.Fatalf("process group before shutdown: alive=%v err=%v", alive, err)
	}
	if stdioOwnerChannelClosed(owner.waitDone) {
		t.Fatal("direct leader exited before owner shutdown")
	}
	select {
	case readErr := <-readerDone:
		t.Fatalf("descendant stdout closed before shutdown: %v", readErr)
	default:
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- owner.Close() }()
	if err := stdioOwnerWaitForFile(paths[3], 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := stdioOwnerWaitForFile(paths[4], 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := stdioOwnerWaitForFile(paths[5], 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := stdioOwnerWaitForFile(paths[6], 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := stdioOwnerWaitForFile(paths[7], 5*time.Second); err != nil {
		t.Fatal(err)
	}
	// The child has reaped its own child; the direct leader remains alive in
	// the process group until the owner escalates to KILL. A successful Close
	// must not be reported while that group is still present.
	alive, err = acpStdioProcessGroupAlive(groupID)
	if err != nil || !alive {
		t.Fatalf("process group while leader is held: alive=%v err=%v", alive, err)
	}
	if stdioOwnerChannelClosed(owner.waitDone) {
		t.Fatal("owner reaped the direct leader while the controlled group member was alive")
	}
	select {
	case closeErr := <-closeDone:
		t.Fatalf("Close returned before group cleanup was confirmed: %v", closeErr)
	default:
	}
	select {
	case readErr := <-readerDone:
		t.Fatalf("stdout reached EOF while the direct group leader remained alive: %v", readErr)
	default:
	}

	const concurrentJoiners = 3
	joinResults := make(chan error, concurrentJoiners)
	for i := 0; i < concurrentJoiners; i++ {
		go func() { joinResults <- owner.Join() }()
	}
	if closeErr := stdioOwnerWaitErrorNamed("Close after group shutdown", closeDone, 5*time.Second); closeErr != nil {
		t.Fatalf("Close after terminating same-group descendants: %v", closeErr)
	}
	for i := 0; i < concurrentJoiners; i++ {
		joinErr := stdioOwnerWaitErrorNamed(fmt.Sprintf("Join call %d", i+1), joinResults, 5*time.Second)
		if joinErr != nil && strings.Contains(joinErr.Error(), "Wait was already called") {
			t.Errorf("concurrent Join exposed duplicate Wait: %v", joinErr)
		}
	}
	if extra := stdioOwnerCallWithin("repeated Join", owner.Join); extra != nil && strings.Contains(extra.Error(), "Wait was already called") {
		t.Errorf("repeated Join exposed duplicate Wait: %v", extra)
	}
	if got := cleanupCalls.Load(); got != 1 {
		t.Errorf("cleanup callback count = %d, want 1", got)
	}
	stdioOwnerRequireJoinedState(t, owner)
	if alive, err := acpStdioProcessGroupAlive(groupID); err != nil || alive {
		t.Errorf("process group after all Join calls: alive=%v err=%v", alive, err)
	}
	select {
	case readErr := <-readerDone:
		if readErr != nil {
			t.Errorf("drain owner stdout after group shutdown: %v", readErr)
		}
	case <-time.After(5 * time.Second):
		t.Error("owner stdout remained open after group shutdown")
	}
}

func TestStdioProcessOwnerCleanupWaitsUntilJoinReturns(t *testing.T) {
	requireStdioProcessGroups(t)
	privateCopy := filepath.Join(t.TempDir(), "private-executable-copy")
	if err := os.Symlink("/bin/sh", privateCopy); err != nil {
		t.Fatalf("create private executable symlink: %v", err)
	}
	var owner *StdioProcessOwner
	var cleanupCalls atomic.Int32
	owner, err := NewStdioProcessOwner(StdioProcessConfig{
		Command:        "/bin/sh",
		ExecutablePath: privateCopy,
		Args:           []string{"-c", "printf '%s\\n' block-ready; exec sleep 60", "stdio-owner-helper"},
		Cleanup: func() error {
			cleanupCalls.Add(1)
			if !stdioOwnerChannelClosed(owner.waitDone) {
				return errors.New("private executable cleanup ran before child Wait")
			}
			owner.mu.Lock()
			groupGone := owner.groupGone
			owner.mu.Unlock()
			if !groupGone || owner.cmd.ProcessState == nil {
				return errors.New("private executable cleanup ran before process-group confirmation")
			}
			return os.Remove(privateCopy)
		},
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	defer stdioOwnerCleanup(t, owner)
	if err := owner.Start(); err != nil {
		t.Fatalf("start blocking helper from private executable: %v", err)
	}
	if err := stdioOwnerWaitForOutput(owner.Reader(), "block-ready\n", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := stdioOwnerCallWithin("Close before Join", owner.Close); err != nil {
		t.Fatalf("Close before Join: %v", err)
	}
	if _, err := os.Lstat(privateCopy); err != nil {
		t.Errorf("private executable was removed before Join: %v", err)
	}
	if got := cleanupCalls.Load(); got != 0 {
		t.Errorf("cleanup callback count before Join = %d, want 0", got)
	}

	joinErr := stdioOwnerCallWithin("Join and clean private executable", owner.Join)
	if joinErr != nil && strings.Contains(joinErr.Error(), "Wait was already called") {
		t.Errorf("Join exposed a duplicate Wait: %v", joinErr)
	}
	if _, err := os.Lstat(privateCopy); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("private executable remains after Join: stat error = %v", err)
	}
	if got := cleanupCalls.Load(); got != 1 {
		t.Errorf("cleanup callback count after Join = %d, want 1", got)
	}
	stdioOwnerRequireJoinedState(t, owner)
}

func requireStdioProcessGroups(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("process-group helper requires Linux or Darwin, got %s", runtime.GOOS)
	}
}

func stdioOwnerWriteScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	content := "#!/bin/sh\nset -eu\n" + body
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatalf("write process helper %q: %v", path, err)
	}
	return path
}

func stdioOwnerWaitForFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect helper marker %q: %w", path, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for helper marker %q", path)
}

func stdioOwnerWaitForOutput(reader io.Reader, want string, timeout time.Duration) error {
	result := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(reader).ReadString('\n')
		if err != nil {
			result <- err
			return
		}
		if !strings.Contains(line, want) {
			result <- fmt.Errorf("stdout = %q, want %q", line, want)
			return
		}
		result <- nil
	}()
	select {
	case err := <-result:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("timed out waiting for stdout marker %q", want)
	}
}

func stdioOwnerCallWithin(name string, call func() error) error {
	result := make(chan error, 1)
	go func() { result <- call() }()
	return stdioOwnerWaitErrorNamed(name, result, 5*time.Second)
}

func stdioOwnerWaitErrorNamed(name string, result <-chan error, timeout time.Duration) error {
	select {
	case err := <-result:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("%s did not complete within %s", name, timeout)
	}
}

func stdioOwnerWaitChannel(channel <-chan struct{}, timeout time.Duration) bool {
	select {
	case <-channel:
		return true
	case <-time.After(timeout):
		return false
	}
}

func stdioOwnerChannelClosed(channel <-chan struct{}) bool {
	select {
	case <-channel:
		return true
	default:
		return false
	}
}

func stdioOwnerRequireJoinedState(t *testing.T, owner *StdioProcessOwner) {
	t.Helper()
	if !stdioOwnerChannelClosed(owner.waitDone) {
		t.Error("Join returned before the exact child Wait completed")
	}
	owner.mu.Lock()
	groupGone := owner.groupGone
	groupID := owner.groupID
	processState := owner.cmd.ProcessState
	owner.mu.Unlock()
	if !groupGone {
		t.Error("Join returned before process-group absence was confirmed")
	}
	if processState == nil {
		t.Error("Join returned before the direct process was reaped")
	}
	if groupID > 0 {
		alive, err := acpStdioProcessGroupAlive(groupID)
		if err != nil || alive {
			t.Errorf("process group after Join: alive=%v err=%v", alive, err)
		}
	}
}

func stdioOwnerCleanup(t *testing.T, owner *StdioProcessOwner) {
	t.Helper()
	if owner == nil {
		return
	}
	result := make(chan struct{}, 1)
	go func() {
		_ = owner.Join()
		result <- struct{}{}
	}()
	if !stdioOwnerWaitChannel(result, 5*time.Second) {
		t.Errorf("test cleanup did not join stdio process owner within 5s")
	}
}
